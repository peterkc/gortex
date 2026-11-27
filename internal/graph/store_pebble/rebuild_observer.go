package store_pebble

import (
	"context"
	"errors"
	"fmt"
	"iter"

	"github.com/cockroachdb/pebble/v2"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/pathkey"
)

var ErrRebuildObserver = errors.New("pebble rebuild Observer stage failed")

var rebuildObserverPrefix = []byte{'g', 'O'}

// RebuildObserverStats reports installed calls and physical spill bytes.
type RebuildObserverStats struct {
	Edges, SpillBytes uint64
}

// RebuildObserver streams the native field/registrar/site join against the
// quiet private view. The callback cutoff precedes workspace filtering. Prove
// exact receipt or checked predecessor custody before writing any calls.
func (b *BuildStore) RebuildObserver(ctx context.Context, receipts iter.Seq2[ParseFacts, error], spillAt int,
	registrar, dispatcher func(string) bool, sameBoundary func(*graph.Node, *graph.Node) bool, maxCallbacks int,
	makeCall func(*graph.Node, string, string) *graph.Edge,
) (stats RebuildObserverStats, err error) {
	defer func() {
		if err != nil {
			stats = RebuildObserverStats{}
			err = fmt.Errorf("%w: %w", ErrRebuildObserver, err)
		}
	}()
	if receipts == nil || spillAt < 1 || spillAt > goldBufferBytes || registrar == nil || dispatcher == nil ||
		sameBoundary == nil || maxCallbacks < 1 || makeCall == nil {
		return stats, fmt.Errorf("%w: invalid Observer stage inputs", ErrRebuildLinkInputSize)
	}
	occupied, err := rebuildGuardHasPrefix(ctx, b.pending, rebuildObserverPrefix)
	if err != nil {
		return stats, err
	}
	if occupied {
		return stats, invalid("Observer scratch range occupied")
	}
	defer func() {
		err = errors.Join(err, b.pending.DeleteRange(rebuildObserverPrefix, prefixEnd(rebuildObserverPrefix), pebble.NoSync))
	}()
	p := rebuildObserverPass{b: b, view: b.RebuildView(), spillAt: spillAt}
	if err := p.capture(ctx, registrar, dispatcher); err != nil {
		return stats, err
	}
	if err := p.elect(ctx, sameBoundary, maxCallbacks, makeCall); err != nil {
		return stats, err
	}
	if err := p.prove(ctx, receipts); err != nil {
		return stats, err
	}
	err = b.withWrites(func() error { return p.apply(ctx) })
	return p.stats, err
}

type rebuildObserverPass struct {
	b        *BuildStore
	view     RebuildView
	spillAt  int
	buffered int
	stats    RebuildObserverStats
}

// f: field IDs; r/d: access rows by field/role; c: distinct callbacks by
// field; v: live inputs requiring exact raw keys; e: receipt proof states;
// n: declaration proof states with a live witness; w: staged output rows.
func observerScratch(col byte, fields ...string) []byte {
	return keyStrings([]byte{'g', 'O', col}, fields...)
}

func (p *rebuildObserverPass) put(key, value []byte) error {
	return p.b.putRebuildScratch(key, value, 16<<20, p.spillAt, &p.buffered, &p.stats.SpillBytes, "Observer")
}

func (p *rebuildObserverPass) get(key []byte) ([]byte, error) {
	return pendingGet(p.b.pending, key, copyValue)
}

func (p *rebuildObserverPass) capture(ctx context.Context, registrar, dispatcher func(string) bool) error {
	return p.view.visit(ctx, []byte{familyEdge, edgeOut}, func(_ *pebble.Snapshot, r row) error {
		kind, err := rebuildAttributionEdgeKind(r.key)
		if err != nil || kind != graph.EdgeAccessesField {
			return err
		}
		e, err := observerInput(r)
		if err != nil || e.From == "" || e.To == "" {
			return err
		}
		n, err := p.node(ctx, e.From)
		if err != nil || !observerCallable(n) {
			return err
		}
		reg, disp := registrar(n.Name), dispatcher(n.Name)
		if !reg && !disp {
			return nil
		}
		if err := p.put(observerScratch('f', e.To), nil); err != nil {
			return err
		}
		if reg {
			if err := p.put(append(observerScratch('r', e.To), r.key...), nil); err != nil {
				return err
			}
		}
		if disp {
			return p.put(append(observerScratch('d', e.To), r.key...), nil)
		}
		return nil
	})
}

func observerCallable(n *graph.Node) bool {
	return n != nil && (n.Kind == graph.KindMethod || n.Kind == graph.KindFunction)
}

func observerInput(r row) (*graph.Edge, error) {
	if len(r.key)+len(r.value) > 16<<20 {
		return nil, fmt.Errorf("%w: Observer input row", ErrRebuildLinkInputSize)
	}
	return guardLightEdge(r)
}

func (p *rebuildObserverPass) node(ctx context.Context, id string) (*graph.Node, error) {
	key := pendingNodeKey(id)
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return pendingGet(p.b.pending, key, func(raw []byte) (*graph.Node, error) {
		if len(key)+len(raw) > 16<<20 {
			return nil, fmt.Errorf("%w: Observer node row", ErrRebuildLinkInputSize)
		}
		n, err := decodeNodeLight(raw)
		if err == nil && n.ID != id {
			return nil, invalid("Observer node identity")
		}
		return n, err
	})
}

func (p *rebuildObserverPass) edge(key []byte) (*graph.Edge, error) {
	e, err := pendingGet(p.b.pending, key, func(raw []byte) (*graph.Edge, error) {
		return observerInput(row{key, raw})
	})
	if err == nil && e == nil {
		err = invalid("Observer input missing")
	}
	return e, err
}

func (p *rebuildObserverPass) access(ctx context.Context, col byte, field string, yield func(*graph.Edge) error) error {
	prefix := observerScratch(col, field)
	return pendingVisit(ctx, p.b.pending, prefix, func(r row) error {
		e, err := p.edge(r.key[len(prefix):])
		if err != nil {
			return err
		}
		if err := p.wantEdge(e); err != nil {
			return err
		}
		if err := p.wantNode(e.From, e); err != nil {
			return err
		}
		return yield(e)
	})
}

func (p *rebuildObserverPass) elect(ctx context.Context, sameBoundary func(*graph.Node, *graph.Node) bool, maxCallbacks int,
	makeCall func(*graph.Node, string, string) *graph.Edge,
) error {
	return pendingVisit(ctx, p.b.pending, observerScratch('f'), func(r row) error {
		field, _, err := readKeyString(r.key[3:])
		if err != nil {
			return err
		}
		for _, role := range [...]byte{'r', 'd'} {
			found, err := rebuildGuardHasPrefix(ctx, p.b.pending, observerScratch(role, field))
			if err != nil || !found {
				return err
			}
		}
		if err := p.access(ctx, 'r', field, func(e *graph.Edge) error {
			return p.collectCallbacks(ctx, field, e.From)
		}); err != nil {
			return err
		}
		prefix := observerScratch('c', field)
		count := 0
		if err := pendingVisit(ctx, p.b.pending, prefix, func(row) error {
			count++
			return nil
		}); err != nil || count == 0 || count > maxCallbacks {
			return err
		}
		return p.access(ctx, 'd', field, func(access *graph.Edge) error {
			disp, err := p.node(ctx, access.From)
			if err != nil {
				return err
			}
			return pendingVisit(ctx, p.b.pending, prefix, func(r row) error {
				id, _, err := readKeyString(r.key[len(prefix):])
				if err != nil || id == access.From {
					return err
				}
				cb, err := p.node(ctx, id)
				if err != nil || !sameBoundary(disp, cb) {
					return err
				}
				return p.propose(makeCall(disp, id, field))
			})
		})
	})
}

func (p *rebuildObserverPass) collectCallbacks(ctx context.Context, field, registrar string) error {
	prefix := keyStrings([]byte{familyEdge, edgeIn}, registrar)
	return pendingVisit(ctx, p.b.pending, prefix, func(r row) error {
		kind, err := rebuildAttributionEdgeKind(r.value)
		if err != nil || kind != graph.EdgeCalls {
			return err
		}
		in, err := p.edge(r.value)
		if err != nil || in.From == "" {
			return err
		}
		if in.To != registrar {
			return invalid("Observer reverse identity")
		}
		for _, kind := range [...]graph.EdgeKind{graph.EdgeReads, graph.EdgeReferences} {
			if err := pendingVisit(ctx, p.b.pending, guardOutgoing(in.From, kind), func(r row) error {
				ref, err := observerInput(r)
				if err != nil || ref.To == "" || ref.FilePath != in.FilePath || ref.Line != in.Line {
					return err
				}
				n, err := p.node(ctx, ref.To)
				if err != nil || !observerCallable(n) {
					return err
				}
				if err := p.wantEdge(in); err != nil {
					return err
				}
				if err := p.wantEdge(ref); err != nil {
					return err
				}
				if err := p.wantNode(ref.To, ref); err != nil {
					return err
				}
				return p.put(observerScratch('c', field, ref.To), nil)
			}); err != nil {
				return err
			}
		}
		return nil
	})
}

func (p *rebuildObserverPass) propose(e *graph.Edge) error {
	key := pendingEdgeKey(e)
	prior, err := p.b.pendingEdge(key)
	if err != nil || prior != nil {
		return err
	}
	value, err := encodeEdge(e)
	if err != nil {
		return err
	}
	// Fields arrive in native sorted ID order. A later field replaces the
	// earlier staged payload; an occupied durable identity remains untouched.
	return p.put(append(observerScratch('w'), key...), value)
}

func observerProofMatches(proof rebuildEdgeProof, kind graph.EdgeKind) bool {
	switch proof {
	case rebuildProofDirect, rebuildProofReceiver:
		return kind == graph.EdgeAccessesField
	case rebuildProofTemporalSynthetic, rebuildProofEventChannel, rebuildProofRNBridge:
		return kind == graph.EdgeCalls
	case rebuildProofSwiftObjC, rebuildProofRNPair:
		return kind == graph.EdgeReferences
	default:
		return false
	}
}

func (p *rebuildObserverPass) wantEdge(e *graph.Edge) error {
	live := pendingEdgeKey(e)
	key := append(observerScratch('v'), live...)
	seen, err := p.get(key)
	if err != nil || seen != nil {
		return err
	}
	custody, err := readRebuildEdgeCustody(p.b.pending, live)
	if err != nil {
		return err
	}
	if custody.proof != 0 {
		if !observerProofMatches(custody.proof, e.Kind) {
			return &RebuildUnknownProvenanceError{Count: 1, First: graph.EdgeIdentityFor(e)}
		}
		return nil
	}
	raw := custody.raw
	if raw == nil {
		raw = live
	}
	if err := p.put(key, raw); err != nil {
		return err
	}
	return p.put(append(observerScratch('e'), raw...), []byte{rebuildCapabilityWanted})
}

func (p *rebuildObserverPass) wantNode(id string, witness *graph.Edge) error {
	key := observerScratch('n', id)
	state, err := p.get(key)
	if err != nil || state != nil {
		return err
	}
	return p.put(key, append([]byte{rebuildCapabilityWanted}, pendingEdgeKey(witness)...))
}

func (p *rebuildObserverPass) note(key []byte, bit byte) error {
	state, err := p.get(key)
	if err != nil {
		return err
	}
	return noteRebuildScratchPayload(p.put, key, state, bit)
}

func (p *rebuildObserverPass) prove(ctx context.Context, receipts iter.Seq2[ParseFacts, error]) error {
	found, err := rebuildGuardHasPrefix(ctx, p.b.pending, observerScratch('n'))
	if err != nil || !found {
		return err
	}
	if err := recordRebuildScratchReceipts(receipts, func(facts ParseFacts) error {
		file := pathkey.Normalize(facts.File.FilePath)
		for _, e := range facts.Edges {
			if err := ctx.Err(); err != nil {
				return err
			}
			bit := byte(rebuildCapabilityUnproved)
			if facts.EffectiveLanguage != "" && e.FilePath == file {
				bit = rebuildCapabilityProved
			}
			if err := p.note(append(observerScratch('e'), pendingEdgeKey(e)...), bit); err != nil {
				return err
			}
		}
		for _, n := range facts.Nodes {
			if err := ctx.Err(); err != nil {
				return err
			}
			key := observerScratch('n', n.ID)
			state, err := p.get(key)
			if err != nil {
				return err
			}
			if len(state) == 0 {
				continue
			}
			live, err := p.node(ctx, n.ID)
			if err != nil {
				return err
			}
			bit := byte(rebuildCapabilityUnproved)
			if live != nil && facts.EffectiveLanguage == n.Language && n.FilePath == file &&
				live.FilePath == file && live.Kind == n.Kind && live.Language == n.Language &&
				live.Name == n.Name && live.StartLine == n.StartLine && live.RepoPrefix == n.RepoPrefix {
				bit = rebuildCapabilityProved
			}
			if err := p.note(key, bit); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		return err
	}
	const exact = rebuildCapabilityWanted | rebuildCapabilityProved
	var refused rebuildScratchRefusals
	refuse := func(key []byte) error {
		id, err := decodeLinkRecorder(key)
		if err != nil {
			return err
		}
		refused.add(id, nil)
		return nil
	}
	if err := pendingVisit(ctx, p.b.pending, observerScratch('v'), func(r row) error {
		state, err := p.get(append(observerScratch('e'), r.value...))
		if err != nil || len(state) == 1 && state[0] == exact {
			return err
		}
		return refuse(r.key[3:])
	}); err != nil {
		return err
	}
	if err := pendingVisit(ctx, p.b.pending, observerScratch('n'), func(r row) error {
		if len(r.value) < 2 {
			return invalid("Observer declaration proof row")
		}
		if r.value[0] != exact {
			return refuse(r.value[1:])
		}
		return nil
	}); err != nil {
		return err
	}
	return refused.err()
}

func (p *rebuildObserverPass) apply(ctx context.Context) error {
	return applyRebuildScratchEdges(ctx, p.visit, p.b, rebuildProofObserver, &p.stats.Edges)
}

func (p *rebuildObserverPass) visit(ctx context.Context, yield func(*graph.Edge) error) error {
	return visitRebuildScratchKeyedEdges(ctx, p.b.pending, observerScratch('w'), 0, "Observer staged call key", yield)
}
