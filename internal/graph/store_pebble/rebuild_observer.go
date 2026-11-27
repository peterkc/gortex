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

// RebuildObserverStats reports installed calls.
type RebuildObserverStats struct {
	Edges uint64
}

// RebuildObserver streams the native field/registrar/site join against the
// quiet private view. The callback cutoff precedes workspace filtering. Prove
// exact receipt or checked producer custody before writing any calls.
// Discard the private build if applying the calls fails.
func (b *BuildStore) RebuildObserver(ctx context.Context, receipts iter.Seq2[ParseFacts, error],
	registrar, dispatcher func(string) bool, sameBoundary func(*graph.Node, *graph.Node) bool, maxCallbacks int,
	makeCall func(*graph.Node, string, string) *graph.Edge,
) (stats RebuildObserverStats, err error) {
	defer func() {
		if err != nil {
			stats = RebuildObserverStats{}
			err = fmt.Errorf("%w: %w", ErrRebuildObserver, err)
		}
	}()
	if receipts == nil || registrar == nil || dispatcher == nil ||
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
	p := rebuildObserverPass{b: b, view: b.RebuildView()}
	if err := p.capture(ctx, registrar, dispatcher); err != nil {
		return stats, err
	}
	if err := p.elect(ctx, sameBoundary, maxCallbacks, makeCall, false); err != nil {
		return stats, err
	}
	incomplete, err := p.prove(ctx, receipts)
	if err != nil {
		return stats, err
	}
	if incomplete {
		if err := p.elect(ctx, sameBoundary, maxCallbacks, makeCall, true); err != nil {
			return stats, err
		}
		if err := p.refusal(ctx); err != nil {
			return stats, err
		}
	}
	err = b.withWrites(func() error { return p.apply(ctx) })
	return p.stats, err
}

type rebuildObserverPass struct {
	b     *BuildStore
	view  RebuildView
	stats RebuildObserverStats
}

// f: field IDs; r/d: access rows by field/role; c: distinct callbacks by
// field (empty until needed); v: live inputs requiring exact raw keys;
// e: receipt proof states;
// n: declaration proof states with a live witness; w: staged output rows;
// x: output proof flags used only when a witness fails.
func observerScratch(col byte, fields ...string) []byte {
	return keyStrings([]byte{'g', 'O', col}, fields...)
}

func (p *rebuildObserverPass) put(key, value []byte) error {
	return p.b.putRebuildScratch(key, value, 16<<20, "Observer")
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
		if err != nil || !graph.ObserverCallable(n) {
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
		return yield(e)
	})
}

func (p *rebuildObserverPass) fields(ctx context.Context, yield func(string) error) error {
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
		return yield(field)
	})
}

const (
	observerCallbackNeeded byte = 1
	observerCallbackProved byte = 2
)

func (p *rebuildObserverPass) callbackRoutes(ctx context.Context, field string, yield func(access, in, ref *graph.Edge) error) error {
	return p.access(ctx, 'r', field, func(access *graph.Edge) error {
		return p.callbackWitnesses(ctx, access.From, func(in, ref *graph.Edge) error {
			return yield(access, in, ref)
		})
	})
}

func (p *rebuildObserverPass) collectCallbacks(ctx context.Context, field string) error {
	return p.callbackRoutes(ctx, field, func(_, _, ref *graph.Edge) error {
		return p.put(observerScratch('c', field, ref.To), nil)
	})
}

func (p *rebuildObserverPass) demandCallbacks(ctx context.Context, field string) error {
	return p.callbackRoutes(ctx, field, func(access, in, ref *graph.Edge) error {
		state, err := p.get(observerScratch('c', field, ref.To))
		if err != nil || len(state) == 0 {
			return err
		}
		if err := p.wantWitness(access, access.From); err != nil {
			return err
		}
		if err := p.wantEdge(in); err != nil {
			return err
		}
		return p.wantWitness(ref, ref.To)
	})
}

func (p *rebuildObserverPass) elect(ctx context.Context, sameBoundary func(*graph.Node, *graph.Node) bool, maxCallbacks int,
	makeCall func(*graph.Node, string, string) *graph.Edge, reporting bool,
) error {
	return p.fields(ctx, func(field string) error {
		if !reporting {
			if err := p.collectCallbacks(ctx, field); err != nil {
				return err
			}
		}
		prefix := observerScratch('c', field)
		count := 0
		if err := pendingVisit(ctx, p.b.pending, prefix, func(row) error {
			count++
			return nil
		}); err != nil || !graph.ObserverCallbackCountAllowed(count, maxCallbacks) {
			return err
		}
		if reporting {
			if err := p.attributeCallbacks(ctx, field); err != nil {
				return err
			}
		}
		if err := p.access(ctx, 'd', field, func(access *graph.Edge) error {
			return p.pairDispatcher(ctx, access, field, sameBoundary, makeCall, reporting)
		}); err != nil || reporting {
			return err
		}
		return p.demandCallbacks(ctx, field)
	})
}

func (p *rebuildObserverPass) pairDispatcher(ctx context.Context, access *graph.Edge, field string,
	sameBoundary func(*graph.Node, *graph.Node) bool, makeCall func(*graph.Node, string, string) *graph.Edge, reporting bool,
) error {
	disp, err := p.node(ctx, access.From)
	if err != nil {
		return err
	}
	badDispatch := false
	if reporting {
		badDispatch, err = p.witnessUnproved(access, access.From)
		if err != nil {
			return err
		}
	}
	needed := false
	prefix := observerScratch('c', field)
	if err := pendingVisit(ctx, p.b.pending, prefix, func(r row) error {
		id, _, err := readKeyString(r.key[len(prefix):])
		if err != nil || !graph.ObserverCallbackIsDistinct(access.From, id) {
			return err
		}
		cb, err := p.node(ctx, id)
		if err != nil || !sameBoundary(disp, cb) {
			return err
		}
		e := makeCall(disp, id, field)
		key := pendingEdgeKey(e)
		prior, err := p.b.pendingEdge(key)
		if err != nil || prior != nil {
			return err
		}
		if reporting {
			badCallback := len(r.value) == 0 || r.value[0] != observerCallbackProved
			return p.reportProposal(key, field, badDispatch || badCallback)
		}
		needed = true
		if len(r.value) == 0 {
			if err := p.put(r.key, []byte{observerCallbackNeeded}); err != nil {
				return err
			}
		}
		return p.propose(key, e)
	}); err != nil || !needed {
		return err
	}
	return p.wantWitness(access, access.From)
}

func (p *rebuildObserverPass) callbackWitnesses(ctx context.Context, registrar string, yield func(*graph.Edge, *graph.Edge) error) error {
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
		for _, kind := range graph.ObserverCallbackKinds() {
			if err := pendingVisit(ctx, p.b.pending, guardOutgoing(in.From, kind), func(r row) error {
				ref, err := observerInput(r)
				if err != nil || !graph.ObserverCallbackAtSite(in, ref) {
					return err
				}
				n, err := p.node(ctx, ref.To)
				if err != nil || !graph.ObserverCallable(n) {
					return err
				}
				return yield(in, ref)
			}); err != nil {
				return err
			}
		}
		return nil
	})
}

func (p *rebuildObserverPass) propose(key []byte, e *graph.Edge) error {
	value, err := encodeEdge(e)
	if err != nil {
		return err
	}
	return p.put(append(observerScratch('w'), key...), value)
}

func (p *rebuildObserverPass) reportProposal(key []byte, field string, unproved bool) error {
	// The later field replaces the earlier payload. Within one field,
	// one proved route suffices, regardless of route order.
	proofKey := append(observerScratch('x'), key...)
	previous, err := p.get(proofKey)
	if err != nil {
		return err
	}
	if len(previous) != 0 && string(previous[1:]) == field && previous[0] == 0 {
		return nil
	}
	bad := byte(0)
	if unproved {
		bad = 1
	}
	return p.put(proofKey, append([]byte{bad}, field...))
}

func observerProofMatches(proof rebuildEdgeProof, kind graph.EdgeKind) bool {
	switch proof {
	case rebuildProofDirect, rebuildProofReceiver:
		return kind == graph.EdgeAccessesField
	case rebuildProofTemporalSynthetic, rebuildProofEventChannel, rebuildProofRNBridge, rebuildProofObserver:
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
	if errors.Is(err, ErrRebuildUnknownProvenance) {
		return p.put(key, []byte{0})
	}
	if err != nil {
		return err
	}
	if custody.proof != 0 {
		if !observerProofMatches(custody.proof, e.Kind) {
			return p.put(key, []byte{0})
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

func (p *rebuildObserverPass) wantWitness(e *graph.Edge, id string) error {
	if err := p.wantEdge(e); err != nil {
		return err
	}
	return p.wantNode(id, e)
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

func (p *rebuildObserverPass) prove(ctx context.Context, receipts iter.Seq2[ParseFacts, error]) (bool, error) {
	found, err := rebuildGuardHasPrefix(ctx, p.b.pending, observerScratch('n'))
	if err != nil || !found {
		return false, err
	}
	for facts, err := range receipts {
		if err != nil {
			return false, err
		}
		file := pathkey.Normalize(facts.File.FilePath)
		for _, e := range facts.Edges {
			if err := ctx.Err(); err != nil {
				return false, err
			}
			bit := byte(rebuildCapabilityUnproved)
			if facts.EffectiveLanguage != "" && e.FilePath == file {
				bit = rebuildCapabilityProved
			}
			if err := p.note(append(observerScratch('e'), pendingEdgeKey(e)...), bit); err != nil {
				return false, err
			}
		}
		for _, n := range facts.Nodes {
			if err := ctx.Err(); err != nil {
				return false, err
			}
			key := observerScratch('n', n.ID)
			state, err := p.get(key)
			if err != nil {
				return false, err
			}
			if len(state) == 0 {
				continue
			}
			live, err := p.node(ctx, n.ID)
			if err != nil {
				return false, err
			}
			bit := byte(rebuildCapabilityUnproved)
			if live != nil && facts.EffectiveLanguage != "" && n.FilePath == file &&
				live.FilePath == file && live.Kind == n.Kind && live.Language == n.Language &&
				live.Name == n.Name && live.StartLine == n.StartLine && live.RepoPrefix == n.RepoPrefix {
				bit = rebuildCapabilityProved
			}
			if err := p.note(key, bit); err != nil {
				return false, err
			}
		}
	}
	incomplete := false
	if err := pendingVisit(ctx, p.b.pending, observerScratch('v'), func(r row) error {
		bad, err := p.rawUnproved(r.value)
		incomplete = incomplete || bad
		return err
	}); err != nil {
		return false, err
	}
	if err := pendingVisit(ctx, p.b.pending, observerScratch('n'), func(r row) error {
		if len(r.value) < 2 {
			return invalid("Observer declaration proof row")
		}
		incomplete = incomplete || r.value[0] != rebuildScratchExact
		return nil
	}); err != nil {
		return false, err
	}
	return incomplete, nil
}

// rawUnproved reads the state for a wanted raw key. A one-byte marker
// records checked custody that was stale or supplied by the wrong producer.
func (p *rebuildObserverPass) rawUnproved(raw []byte) (bool, error) {
	if len(raw) == 1 {
		return true, nil
	}
	state, err := p.get(append(observerScratch('e'), raw...))
	return len(state) != 1 || state[0] != rebuildScratchExact, err
}

func (p *rebuildObserverPass) edgeUnproved(e *graph.Edge) (bool, error) {
	raw, err := p.get(append(observerScratch('v'), pendingEdgeKey(e)...))
	if err != nil || raw == nil {
		return false, err // producer custody was already checked
	}
	return p.rawUnproved(raw)
}

func (p *rebuildObserverPass) witnessUnproved(e *graph.Edge, id string) (bool, error) {
	bad, err := p.edgeUnproved(e)
	if err != nil || bad {
		return bad, err
	}
	state, err := p.get(observerScratch('n', id))
	return len(state) < 2 || state[0] != rebuildScratchExact, err
}

// Any proved route suffices for a needed callback. Repeat the join only
// when receipt replay leaves a witness unproved.
func (p *rebuildObserverPass) attributeCallbacks(ctx context.Context, field string) error {
	return p.callbackRoutes(ctx, field, func(access, in, ref *graph.Edge) error {
		key := observerScratch('c', field, ref.To)
		state, err := p.get(key)
		if err != nil || len(state) == 0 || state[0] == observerCallbackProved {
			return err
		}
		bad, err := p.witnessUnproved(access, access.From)
		if err != nil || bad {
			return err
		}
		bad, err = p.edgeUnproved(in)
		if err != nil || bad {
			return err
		}
		bad, err = p.witnessUnproved(ref, ref.To)
		if err != nil || bad {
			return err
		}
		return p.put(key, []byte{observerCallbackProved})
	})
}

func (p *rebuildObserverPass) refusal(ctx context.Context) error {
	prefix := observerScratch('x')
	var refused rebuildScratchRefusals
	if err := pendingVisit(ctx, p.b.pending, prefix, func(r row) error {
		if len(r.value) == 0 {
			return invalid("Observer proposal proof row")
		}
		if r.value[0] == 0 {
			return nil
		}
		id, err := decodeLinkRecorder(r.key[len(prefix):])
		if err != nil {
			return err
		}
		refused.add(id, nil)
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
