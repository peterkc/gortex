package store_pebble

import (
	"bytes"
	"context"
	"errors"
	"iter"

	"github.com/cockroachdb/pebble/v2"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/pathkey"
)

// MarkRebuildBridgeIsolation projects only matching bridge methods and marker
// sites. It uses receipt order for elected selected sites, without running the
// native writer or its refusal accounting over held payloads.
func (b *BuildStore) MarkRebuildBridgeIsolation(ctx context.Context, receipts iter.Seq2[ParseFacts, error],
	expo bool, admitted bool, gateCauses []string, language func(string) string,
) error {
	names, cause := rebuildRNNames, "rn-bridge"
	if expo {
		names, cause = rebuildExpoNames, "expo-bridge"
	}
	p := rebuildBridgeIsolation{rebuildFrameworkIsolation: rebuildFrameworkIsolation{b: b, language: language, cause: cause},
		names: names, admitted: admitted, gateCauses: gateCauses}
	return p.scratch(ctx, rebuildReactNativePrefix, func() error {
		return p.run(ctx, receipts)
	})
}

type rebuildBridgeIsolation struct {
	rebuildFrameworkIsolation
	names      func(map[string]any) (string, string)
	admitted   bool
	gateCauses []string
}

func (p rebuildBridgeIsolation) offer(n *graph.Node, deferred string) error {
	if n.Kind != graph.KindMethod || n.ID == "" {
		return nil
	}
	module, method := p.names(n.Meta)
	if module == "" || method == "" {
		return nil
	}
	return p.candidate(rebuildRNKey('n', module, method, n.ID), n, deferred)
}

func (p rebuildBridgeIsolation) run(ctx context.Context, receipts iter.Seq2[ParseFacts, error]) error {
	if err := p.b.visitRebuildHeldNodes(ctx, func(n *graph.Node, _ string, _ []byte) error {
		if n.Kind != graph.KindMethod {
			return nil
		}
		deferred, err := rebuildHeldLanguage(p.b.pending, n.ID)
		if err != nil {
			return err
		}
		return p.offer(n, deferred)
	}); err != nil {
		return err
	}
	// Selected methods can acquire a call from a dropped marker, or a
	// selected-to-selected bridge can become admitted by a held family.
	heldMarkers, err := rebuildGuardHasPrefix(ctx, p.b.pending, rebuildHoldoutKey('f', nil))
	if err != nil {
		return err
	}
	if heldMarkers || !p.admitted {
		if err := p.b.RebuildView().visit(ctx, []byte{familyNode}, func(_ *pebble.Snapshot, r row) error {
			n, err := checkedRebuildLightNode(r, p.cause)
			if err != nil || n.Kind != graph.KindMethod {
				return err
			}
			n, err = decodeNode(r.value)
			if err != nil {
				return err
			}
			return p.offer(n, "")
		}); err != nil {
			return err
		}
	}
	methods, err := rebuildGuardHasPrefix(ctx, p.b.pending, rebuildRNKey('n'))
	if err != nil || !methods {
		return err
	}
	if err := p.heldMarkers(ctx); err != nil {
		return err
	}
	if err := p.b.RebuildView().VisitEdges(ctx, func(e *graph.Edge) error {
		if !rebuildFrameworkBridgeMarker(e) {
			return nil
		}
		module, method := rebuildRNNames(e.Meta)
		matches, err := rebuildGuardHasPrefix(ctx, p.b.pending, rebuildRNKey('n', module, method))
		if err != nil || !matches {
			return err
		}
		key := pendingEdgeKey(e)
		if err := p.b.putRebuildScratch(rebuildRNKey('q', string(key)), []byte{rebuildCapabilityWanted}, 16<<20, p.cause); err != nil {
			return err
		}
		custody, err := readRebuildEdgeCustody(p.b.pending, key)
		if errors.Is(err, ErrRebuildUnknownProvenance) || err == nil && custody.raw == nil && custody.proof != 0 {
			return p.uncertain(e)
		}
		if err != nil {
			return err
		}
		raw := custody.raw
		if raw == nil {
			raw = key
		}
		return p.b.putRebuildScratch(rebuildRNKey('r', string(raw), string(key)), key, 16<<20, p.cause)
	}); err != nil {
		return err
	}
	// Replay the actual selected receipt sequence. A live key under a different
	// raw claim is not its recorder, and a repeated key is not a new site.
	for f, err := range receipts {
		if err != nil {
			return err
		}
		for _, raw := range f.Edges {
			prefix := rebuildRNKey('r', string(pendingEdgeKey(raw)))
			if err := pendingVisit(ctx, p.b.pending, prefix, func(r row) error {
				e, err := p.b.pendingEdge(r.value)
				if err != nil {
					return err
				}
				if e == nil {
					return invalid("framework isolation lost bridge marker")
				}
				module, method := rebuildRNNames(e.Meta)
				rawModule, rawMethod := rebuildRNNames(raw.Meta)
				proved := rebuildFrameworkBridgeMarker(raw) && rawModule == module && rawMethod == method &&
					f.EffectiveLanguage != "" && raw.FilePath == pathkey.Normalize(f.File.FilePath)
				if !proved {
					return p.uncertain(e)
				}
				key := pendingEdgeKey(e)
				if err := p.b.putRebuildScratch(rebuildRNKey('q', string(key)), []byte{rebuildScratchExact}, 16<<20, p.cause); err != nil {
					return err
				}
				first := rebuildRNKey('g', e.From, module, method)
				prior, err := pendingGet(p.b.pending, first, fnPointerPresent)
				if err != nil || prior {
					return err
				}
				return p.b.putRebuildScratch(first, key, 16<<20, p.cause)
			}); err != nil {
				return err
			}
		}
	}
	// A missing receipt or an unproved competing claim makes the entire
	// caller/module/method election uncertain, including its other recorders.
	prefix := rebuildRNKey('q')
	readSite := func(r row) (*graph.Edge, error) {
		key, rest, err := readKeyString(r.key[len(prefix):])
		if err != nil || len(rest) != 0 {
			return nil, invalid("framework isolation site key")
		}
		e, err := p.b.pendingEdge([]byte(key))
		if err == nil && e == nil {
			return nil, invalid("framework isolation lost bridge site")
		}
		return e, err
	}
	if err := pendingVisit(ctx, p.b.pending, prefix, func(r row) error {
		if len(r.value) == 1 && r.value[0] == rebuildScratchExact {
			return nil
		}
		e, err := readSite(r)
		if err != nil {
			return err
		}
		return p.uncertain(e)
	}); err != nil {
		return err
	}
	return pendingVisit(ctx, p.b.pending, prefix, func(r row) error {
		e, err := readSite(r)
		if err != nil {
			return err
		}
		return p.site(ctx, e)
	})
}

func (p rebuildBridgeIsolation) uncertain(e *graph.Edge) error {
	module, method := rebuildRNNames(e.Meta)
	return p.b.putRebuildScratch(rebuildRNKey('x', e.From, module, method), nil, 16<<20, p.cause)
}

func (p rebuildBridgeIsolation) visitMethods(ctx context.Context, e *graph.Edge, visit func(*graph.Edge, string, bool) error) error {
	module, method := rebuildRNNames(e.Meta)
	return pendingVisit(ctx, p.b.pending, rebuildRNKey('n', module, method), func(r row) error {
		n, deferred, err := readFrameworkIsolationCandidate(r)
		if err != nil || n.ID == e.From {
			return err
		}
		out := &graph.Edge{From: e.From, To: n.ID, Kind: graph.EdgeCalls, FilePath: e.FilePath, Line: e.Line}
		fresh, err := p.fresh(out)
		if err != nil {
			return err
		}
		return visit(out, deferred, fresh)
	})
}

func (p rebuildBridgeIsolation) heldMarkers(ctx context.Context) error {
	prefix := rebuildHoldoutKey('f', nil)
	return pendingVisit(ctx, p.b.pending, prefix, func(r row) error {
		deferred, tail, err := readKeyString(r.key[len(prefix):])
		if err != nil {
			return err
		}
		key, rest, err := readKeyString(tail)
		if err != nil || len(rest) != 0 {
			return invalid("framework held marker key")
		}
		e, err := decodeEdge(r.value)
		if err != nil {
			return err
		}
		if !bytes.Equal([]byte(key), pendingEdgeKey(e)) {
			return invalid("framework held marker identity")
		}
		module, method := rebuildRNNames(e.Meta)
		return p.visitMethods(ctx, e, func(out *graph.Edge, _ string, fresh bool) error {
			// The lost site's native ordinal is not available at this turn.
			// Its bucket may also elect an earlier site than the selected one.
			if err := p.b.putRebuildScratch(rebuildRNKey('h', e.From, module, method, deferred), nil, 16<<20, p.cause); err != nil {
				return err
			}
			if !fresh {
				return nil
			}
			return p.markEdge(ctx, out, deferred, p.cause)
		})
	})
}

func (p rebuildBridgeIsolation) site(ctx context.Context, e *graph.Edge) error {
	module, method := rebuildRNNames(e.Meta)
	caller, err := p.b.RebuildView().Node(ctx, e.From)
	if err != nil || caller == nil {
		return err
	}
	uncertain, err := rebuildGuardHasPrefix(ctx, p.b.pending, rebuildRNKey('x', e.From, module, method))
	if err != nil {
		return err
	}
	prefix := rebuildRNKey('h', e.From, module, method)
	heldSite, err := rebuildGuardHasPrefix(ctx, p.b.pending, prefix)
	if err != nil {
		return err
	}
	uncertain = uncertain || heldSite
	if !uncertain {
		first, err := pendingGet(p.b.pending, rebuildRNKey('g', e.From, module, method), copyValue)
		if err != nil || !bytes.Equal(first, pendingEdgeKey(e)) {
			return err
		}
	}
	return p.visitMethods(ctx, e, func(out *graph.Edge, deferred string, fresh bool) error {
		if !fresh {
			return nil
		}
		if deferred != "" {
			cause := "held-endpoint"
			if uncertain {
				cause = p.cause
			}
			if err := p.markEdge(ctx, out, deferred, cause); err != nil {
				return err
			}
		}
		if !p.admitted {
			if deferred == "" {
				for _, language := range p.gateCauses {
					if err := p.markEdge(ctx, out, language, p.cause); err != nil {
						return err
					}
				}
			}
			return nil
		}
		return pendingVisit(ctx, p.b.pending, prefix, func(r row) error {
			language, _, err := readKeyString(r.key[len(prefix):])
			if err != nil {
				return err
			}
			return p.markEdge(ctx, out, language, p.cause)
		})
	})
}
