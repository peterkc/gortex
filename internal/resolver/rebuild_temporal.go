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
		return p.run()
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

func (p *rebuildTemporal) run() (stats store_pebble.RebuildTemporalStats, err error) {
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
	if err := p.rebind(&stats); err != nil {
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

func (p *rebuildTemporal) rebind(stats *store_pebble.RebuildTemporalStats) error {
	_, err := withRebuildTemporalConstants(p.ctx, p.s.View(), func(seed func(string) error) error {
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
			resolved, moved := rebindTemporalStub(e, kind, name, repo, lang, p, deref)
			if p.err != nil {
				return p.err
			}
			if resolved {
				stats.Resolved++
			}
			if moved {
				proof := p.s.EdgeProof(e)
				if !staged {
					original, err := p.s.Edge(p.ctx, old)
					if err != nil {
						return err
					}
					if original == nil {
						return store_pebble.ErrRebuildUnknownProvenance
					}
					if err := proof.WantRecorder(p.ctx, original); err != nil {
						return err
					}
					if err := proof.WantNode(p.ctx, e.From); err != nil {
						return err
					}
				}
				if resolved {
					if err := proof.WantNode(p.ctx, e.To); err != nil {
						return err
					}
				}
				if err := p.s.QueueMove(p.ctx, old.To, e, staged); err != nil {
					return err
				}
				stats.Moves++
			}
			if staged {
				if err := p.s.FinishWrapper(p.ctx, old, e); err != nil {
					return err
				}
				return p.s.QueueAdd(p.ctx, e)
			}
			return nil
		})
	})
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
			out := &graph.Edge{
				From: n.ID, To: to, Kind: graph.EdgeCalls, Origin: graph.OriginASTInferred,
				Meta: map[string]any{"via": via, "cross_language": true},
			}
			proof := p.s.EdgeProof(out)
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
				if err := proof.WantRecorder(p.ctx, e); err != nil {
					return err
				}
			}
			if err := proof.WantRoleNode(p.ctx, n); err != nil {
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
				if err := proof.WantRoleNode(p.ctx, provider); err != nil {
					return err
				}
			} else if err := proof.WantNode(p.ctx, to); err != nil {
				return err
			}
			return p.s.QueueAdd(p.ctx, out)
		})
	})
}
