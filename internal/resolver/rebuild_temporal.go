package resolver

import (
	"context"
	"encoding/binary"
	"iter"
	"strconv"
	"strings"

	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/graph/store_pebble"
)

// RunRebuildTemporal executes registry position two on the quiet private
// graph after gRPC. Language projection belongs after native election.
func RunRebuildTemporal(ctx context.Context, b *store_pebble.BuildStore, receipts iter.Seq2[store_pebble.ParseFacts, error],
	selection FrameworkSynthesizerSelection, spillAt int,
) (store_pebble.RebuildTemporalStats, error) {
	if !selection.allows(SynthTemporalStub) {
		return store_pebble.RebuildTemporalStats{}, nil
	}
	return b.RebuildTemporal(ctx, receipts, spillAt, func(s *store_pebble.RebuildTemporalPass) (store_pebble.RebuildTemporalStats, error) {
		p := rebuildTemporal{ctx: ctx, s: s}
		return p.run(spillAt)
	})
}

type rebuildTemporal struct {
	ctx           context.Context
	s             *store_pebble.RebuildTemporalPass
	err           error // first checked read failure from the shared lookup interface
	wrappersAdded uint64
}

func temporalOrdinal(n uint64) string {
	return string(binary.BigEndian.AppendUint64(nil, n))
}

func (p *rebuildTemporal) run(spillAt int) (stats store_pebble.RebuildTemporalStats, err error) {
	active, err := p.s.IndexInputs(p.ctx, func(e *graph.Edge) (candidate, active bool) {
		if e.Kind == graph.EdgeAnnotated {
			iface, method := temporalRoleForJavaAnnotation(e.To)
			match := iface != "" || method != ""
			return match, match
		}
		via, _ := e.Meta["via"].(string)
		return strings.HasPrefix(via, "temporal."), via == "temporal.register" || via == "temporal.stub" || via == "temporal.start"
	})
	if err != nil || !active {
		return stats, err
	}
	stats.WrapperRounds, stats.WrapperCapped, err = p.wrappers()
	if err != nil {
		return stats, err
	}
	stats.Wrappers = p.wrappersAdded
	if err := p.executorFields(); err != nil {
		return stats, err
	}
	// A malformed stub admits wrapper discovery but does not by itself
	// enter index building. Test the same post-executor dispatch predicate.
	active = false
	if err := p.visitTemporalCalls(func(e *graph.Edge) error {
		if e.Meta["via"] == "temporal.register" {
			active = true
		}
		return nil
	}); err != nil {
		return stats, err
	}
	if err := p.visitTemporalAnnotations(func(*graph.Edge) error {
		active = true
		return nil
	}); err != nil {
		return stats, err
	}
	if err := p.visitDispatches(func(_ *graph.Edge, _, _ string, _ bool) error {
		active = true
		return nil
	}); err != nil {
		return stats, err
	}
	if !active {
		return stats, nil
	}
	if err := p.buildIndex(); err != nil {
		return stats, err
	}
	if err := p.rebind(spillAt, &stats); err != nil {
		return stats, err
	}
	return stats, p.crossLanguage()
}

func (p *rebuildTemporal) visitTemporalCalls(yield func(*graph.Edge) error) error {
	return p.s.VisitInputs(p.ctx, graph.EdgeCalls, yield)
}

func (p *rebuildTemporal) visitTemporalAnnotations(yield func(*graph.Edge) error) error {
	return p.s.VisitInputs(p.ctx, graph.EdgeAnnotated, yield)
}

func (p *rebuildTemporal) visitDispatches(yield func(e *graph.Edge, kind, name string, staged bool) error) error {
	one := func(e *graph.Edge, staged bool) error {
		if e.Meta["via"] != "temporal.stub" && e.Meta["via"] != "temporal.start" {
			return nil
		}
		kind, _ := e.Meta["temporal_kind"].(string)
		name, _ := e.Meta["temporal_name"].(string)
		if kind == "" || name == "" {
			return nil
		}
		return yield(e, kind, name, staged)
	}
	if err := p.visitTemporalCalls(func(e *graph.Edge) error {
		if err := p.applyExecutor(e); err != nil {
			return err
		}
		return one(e, false)
	}); err != nil {
		return err
	}
	return p.visitNewWrappers(func(e *graph.Edge) error { return one(e, true) })
}

func (p *rebuildTemporal) rebind(spillAt int, stats *store_pebble.RebuildTemporalStats) error {
	constants, err := withRebuildTemporalConstants(p.ctx, p.s.View(), spillAt, func(seed func(string) error) error {
		return p.visitDispatches(func(e *graph.Edge, _, name string, _ bool) error {
			if err := seed(name); err != nil {
				return err
			}
			if def, _ := e.Meta["temporal_default_const"].(string); def != "" {
				return seed(def)
			}
			return nil
		})
	}, func(constants *store_pebble.TemporalConstDeref) error {
		deref := func(name string) string {
			if p.err != nil {
				return ""
			}
			value, err := constants.Value(p.ctx, name)
			p.err = err
			return value
		}
		return p.visitDispatches(func(e *graph.Edge, kind, name string, staged bool) error {
			caller, err := p.s.Node(p.ctx, e.From)
			if err != nil {
				return err
			}
			var repo, lang string
			if caller != nil {
				repo, lang = caller.RepoPrefix, caller.Language
			}
			old := graph.EdgeIdentityFor(e)
			resolved, moved := rebindRebuildTemporalStub(e, kind, name, repo, lang, p, deref)
			if p.err != nil {
				return p.err
			}
			if resolved {
				stats.Resolved++
			}
			if moved {
				if !staged {
					original, err := p.s.Edge(p.ctx, old)
					if err != nil {
						return err
					}
					if original == nil {
						return store_pebble.ErrRebuildUnknownProvenance
					}
					if err := p.s.WantRecorder(p.ctx, original); err != nil {
						return err
					}
					if err := p.s.WantNode(p.ctx, e.From); err != nil {
						return err
					}
				}
				if resolved {
					if err := p.s.WantNode(p.ctx, e.To); err != nil {
						return err
					}
				}
				if err := p.s.QueueMove(p.ctx, old.To, e, staged); err != nil {
					return err
				}
				stats.Moves++
			}
			if staged {
				return p.s.QueueAdd(p.ctx, e)
			}
			return nil
		})
	})
	stats.ConstSpillBytes = constants.SpillBytes
	return err
}

// crossLanguage sees the fresh role overlay, but the handler-call census is
// still the frozen input to the turn. The only emitted fields are native ones.
func (p *rebuildTemporal) crossLanguage() error {
	visit := func(lang string, yield func(*graph.Node) error) error {
		return p.s.View().VisitNodes(p.ctx, func(base *graph.Node) error {
			if base.Language != lang || (base.Kind != graph.KindFunction && base.Kind != graph.KindMethod) {
				return nil
			}
			n, err := p.s.Node(p.ctx, base.ID)
			if err != nil {
				return err
			}
			return yield(n)
		})
	}
	if err := visit("go", func(n *graph.Node) error {
		if n.Meta["temporal_role"] != "workflow" {
			return nil
		}
		name, _ := n.Meta["temporal_name"].(string)
		if err := p.s.Put(p.ctx, 'Y', "", "workflow", n.Name, n.ID); err != nil {
			return err
		}
		if name != "" && name != n.Name {
			return p.s.Put(p.ctx, 'Y', "", "workflow", name, n.ID)
		}
		return nil
	}); err != nil {
		return err
	}
	if err := p.visitTemporalCalls(func(e *graph.Edge) error {
		if e.Meta["via"] != "temporal.handler" {
			return nil
		}
		kind, _ := e.Meta["temporal_kind"].(string)
		name, _ := e.Meta["temporal_name"].(string)
		if (kind != "signal" && kind != "query") || name == "" {
			return nil
		}
		from, err := p.s.Node(p.ctx, e.From)
		if err != nil || from == nil || from.Language != "go" {
			return err
		}
		// Record every provider occurrence. Equal destinations emit equal
		// edges and are deduplicated on the native first-key flush.
		return p.s.Put(p.ctx, 'Y', "", kind, name, e.From, e.To, e.FilePath, strconv.Itoa(e.Line))
	}); err != nil {
		return err
	}
	return visit("java", func(n *graph.Node) error {
		role, _ := n.Meta["temporal_role"].(string)
		name, _ := n.Meta["temporal_name"].(string)
		if name == "" {
			name = n.Name
		}
		var via string
		switch role {
		case "workflow":
			via = "temporal.start-workflow"
		case "signal":
			via = "temporal.signal-link"
		case "query":
			via = "temporal.query-link"
		default:
			return nil
		}
		return p.s.Visit(p.ctx, 'Y', []string{role, name}, func(fields []string, _ string) error {
			to := fields[2]
			if to == n.ID {
				return nil
			}
			if len(fields) == 6 {
				line, err := strconv.Atoi(fields[5])
				if err != nil {
					return err
				}
				e, err := p.s.Edge(p.ctx, graph.EdgeIdentity{From: to, To: fields[3], Kind: graph.EdgeCalls, FilePath: fields[4], Line: line})
				if err != nil {
					return err
				}
				if e == nil {
					return store_pebble.ErrRebuildUnknownProvenance
				}
				if err := p.s.WantRecorder(p.ctx, e); err != nil {
					return err
				}
			}
			if err := p.s.WantRoleNode(p.ctx, n); err != nil {
				return err
			}
			if role == "workflow" {
				provider, err := p.s.Node(p.ctx, to)
				if err != nil {
					return err
				}
				if provider == nil {
					return store_pebble.ErrRebuildUnknownProvenance
				}
				if err := p.s.WantRoleNode(p.ctx, provider); err != nil {
					return err
				}
			} else if err := p.s.WantNode(p.ctx, to); err != nil {
				return err
			}
			return p.s.QueueAdd(p.ctx, &graph.Edge{
				From: n.ID, To: to, Kind: graph.EdgeCalls, Origin: graph.OriginASTInferred,
				Meta: map[string]any{"via": via, "cross_language": true},
			})
		})
	})
}

type temporalHandlerLookup interface {
	lookup(kind, name, callerRepo, callerLang string) (id, origin string, confidence float64)
	lookupConvention(kind, name, callerRepo, callerLang string) string
	lookupCrossLang(kind, name, callerLang string) (id string, ok bool)
	lookupExactSig(kind, name, callerRepo string) string
}

// rebindRebuildTemporalStub computes one native mutation. If the target did not
// change, SQLite does not persist the decoded payload.
func rebindRebuildTemporalStub(e *graph.Edge, kind, name, callerRepo, callerLang string,
	idx temporalHandlerLookup, deref func(string) string,
) (resolved, moved bool) {
	handlerID, origin, conf := idx.lookup(kind, name, callerRepo, callerLang)
	// When the direct name didn't resolve, try dereferencing it as a
	// string constant and re-looking-up under the literal value. The
	// deref value is computed up front so it also feeds the
	// cross-language join below (a Java const-ref dispatch through
	// `Constants.X` derefs X to the literal, then matches the Go
	// workflow of that name across the type-system boundary).
	constDeref := ""
	derefVal := deref(name)
	if handlerID == "" && derefVal != "" {
		if hID, o, c := idx.lookup(kind, derefVal, callerRepo, callerLang); hID != "" {
			handlerID, origin, conf = hID, o, c
			constDeref = derefVal
		}
	}
	// Env-default const reference: the env-helper's default argument was a
	// constant reference (`GetEnvOrDefault(KEY, config.ACTIVITY_NAME_DEFAULT)`),
	// recorded as `temporal_default_const`. Substitute the constant's literal
	// VALUE through the deref map (register-confirmed candidates), then look
	// it up. The env-default tier override below keeps the edge at the
	// const_ref tier (inferred, visible) regardless of how it resolved.
	if handlerID == "" {
		if cn, _ := e.Meta["temporal_default_const"].(string); cn != "" {
			if v := deref(cn); v != "" {
				if id, o, c := idx.lookup(kind, v, callerRepo, callerLang); id != "" {
					handlerID, origin, conf = id, o, c
					e.Meta["temporal_const_value"] = v
				}
			}
		}
	}

	// Convention fallback: dispatch to an activity/workflow FUNCTION
	// by name when the worker registers it elsewhere (unregistered
	// here) — Pattern 2 / Stage 1.2. Try the dispatch name, then its
	// const-deref value. Landed at the inferred tier (name convention,
	// not a register-confirmed binding). Tried before the cross-language
	// join: a same-language convention match is a stronger signal than a
	// speculative by-string match across a type-system boundary.
	convention := false
	if handlerID == "" {
		candNames := []string{name}
		if v := derefVal; v != "" && v != name {
			candNames = append(candNames, v)
		}
		for _, nm := range candNames {
			if id := idx.lookupConvention(kind, nm, callerRepo, callerLang); id != "" {
				handlerID, origin, conf = id, graph.OriginASTInferred, 0.6
				convention = true
				if nm != name {
					constDeref = nm
				}
				break
			}
		}
	}

	// Cross-language join: a consumer (typically a temporal.start, e.g.
	// a Java service starting a Go workflow) with no same-language
	// handler is matched to a unique other-language candidate by
	// canonical name, at the speculative tier.
	crossLang := false
	if handlerID == "" {
		matchName := name
		if derefVal != "" {
			matchName = derefVal
		}
		if hID, ok := idx.lookupCrossLang(kind, matchName, callerLang); ok {
			handlerID = hID
			origin = graph.OriginSpeculative
			conf = temporalCrossLangConfidence
			crossLang = true
			if derefVal != "" {
				constDeref = derefVal
			}
		}
	}

	// Exact-name signature-gated fallback (Cat 4): a PascalCase dispatch
	// whose target is a same-named, unregistered, non-suffixed function.
	// Gated on exact name + a kind-matching Temporal signature + a unique
	// candidate; landed speculative + hidden. Last resort, so it runs
	// only after register / const-deref / cross-language all abstained.
	exactSig := false
	if handlerID == "" {
		matchName := name
		if constDeref != "" {
			matchName = constDeref
		}
		if id := idx.lookupExactSig(kind, matchName, callerRepo); id != "" {
			handlerID = id
			origin = graph.OriginSpeculative
			conf = temporalExactSigConfidence
			exactSig = true
		}
	}

	// When the dispatch name came from an env-var-with-literal-default
	// variable (env_default) or from tracing a bare local dispatch
	// variable to its last assignment (var_trace), the value is a
	// best-guess. HOW it was recognised decides the tier:
	//   - "allowlist" / "os_getenv" / "const_ref": confident it IS an
	//     env-with-default — inferred tier (0.6, visible).
	//   - "heuristic" / var_trace / unknown: a guess — hidden speculative
	//     tier (0.4), where the optional LLM cleaning pass can prune it.
	envDefault := false
	envSource := ""
	switch v, _ := e.Meta["temporal_name_origin"].(string); v {
	case "env_default", "var_trace":
		envDefault = true
		envSource, _ = e.Meta["temporal_env_source"].(string)
	}
	envSpeculative := envDefault && envSource != "allowlist" && envSource != "os_getenv" && envSource != "const_ref"
	if handlerID != "" && envDefault {
		if envSpeculative {
			origin = graph.OriginSpeculative
			conf = temporalEnvDefaultConfidence
		} else {
			origin = graph.OriginASTInferred
			conf = temporalEnvDefaultInferredConfidence
		}
	}

	want := handlerID
	if want == "" {
		want = temporalStubPlaceholder(kind, name)
	}
	if e.To == want {
		return handlerID != "", false
	}

	e.To = want
	if handlerID != "" {
		e.Origin = origin
		e.Confidence = conf
		e.ConfidenceLabel = graph.ConfidenceLabelFor(graph.EdgeCalls, conf)
		e.Meta["temporal_resolution"] = origin
		if envSpeculative || crossLang || exactSig {
			e.Meta[graph.MetaSpeculative] = true
		}
		if crossLang {
			e.Meta["temporal_cross_lang"] = true
		} else {
			delete(e.Meta, "temporal_cross_lang")
		}
		if exactSig {
			e.Meta["temporal_resolution_via"] = "exact_sig"
		} else if convention {
			e.Meta["temporal_resolution_via"] = "convention"
		} else {
			delete(e.Meta, "temporal_resolution_via")
		}
		if constDeref != "" {
			e.Meta["temporal_const_deref"] = constDeref
		} else {
			delete(e.Meta, "temporal_const_deref")
		}
		StampSynthesized(e, SynthTemporalStub)
	} else {
		e.Origin = ""
		e.Confidence = 0
		e.ConfidenceLabel = ""
		delete(e.Meta, "temporal_resolution")
		delete(e.Meta, graph.MetaSpeculative)
		delete(e.Meta, "temporal_const_deref")
		delete(e.Meta, "temporal_cross_lang")
		delete(e.Meta, "temporal_resolution_via")
		UnstampSynthesized(e)
	}
	return handlerID != "", true
}
