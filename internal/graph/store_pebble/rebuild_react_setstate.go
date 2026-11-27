package store_pebble

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"iter"

	"github.com/cockroachdb/pebble/v2"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/pathkey"
)

var (
	ErrRebuildReactSetState   = errors.New("pebble rebuild React setState stage failed")
	ErrRebuildFlutterSetState = errors.New("pebble rebuild Flutter setState stage failed")
)

var rebuildSetStatePrefix = []byte{'g', 'W'}

// RebuildReactSetStateStats reports installed calls.
// SpillBytes counts physical bytes of closed threshold SST runs consumed by
// the pass. It is zero because this pass uses only the pending overlay.
type RebuildReactSetStateStats struct {
	Edges, SpillBytes uint64
}

// RebuildReactSetState implements the native membership and setState join.
// The first membership follows the frozen receipt ordinal, not private key
// order. The native adjacency query has no ORDER BY; receipt order is not a
// proof of all native reindex paths. The lifecycle winner uses the last
// method ID in the native explicit sort.
func (b *BuildStore) RebuildReactSetState(ctx context.Context, receipts iter.Seq2[ParseFacts, error], spillAt int,
	isSetState func(string) bool, makeCall func(*graph.Node, *graph.Node, string) *graph.Edge,
) (RebuildReactSetStateStats, error) {
	return b.rebuildSetState(ctx, receipts, spillAt, "render", ErrRebuildReactSetState, rebuildProofReactSetState, isSetState, makeCall)
}

// RebuildFlutterSetStateStats reports the same counters as the React turn.
type RebuildFlutterSetStateStats = RebuildReactSetStateStats

// RebuildFlutterSetState runs the shared lifecycle join with build as the
// target. It reads the quiet view after the React turn has installed its calls.
func (b *BuildStore) RebuildFlutterSetState(ctx context.Context, receipts iter.Seq2[ParseFacts, error], spillAt int,
	isSetState func(string) bool, makeCall func(*graph.Node, *graph.Node, string) *graph.Edge,
) (RebuildFlutterSetStateStats, error) {
	return b.rebuildSetState(ctx, receipts, spillAt, "build", ErrRebuildFlutterSetState, rebuildProofFlutterSetState, isSetState, makeCall)
}

func (b *BuildStore) rebuildSetState(ctx context.Context, receipts iter.Seq2[ParseFacts, error], spillAt int,
	lifecycle string, stage error, proof rebuildEdgeProof,
	isSetState func(string) bool, makeCall func(*graph.Node, *graph.Node, string) *graph.Edge,
) (stats RebuildReactSetStateStats, err error) {
	defer func() {
		if err != nil {
			stats = RebuildReactSetStateStats{}
			err = fmt.Errorf("%w: %w", stage, err)
		}
	}()
	if receipts == nil || spillAt < 1 || spillAt > goldBufferBytes || isSetState == nil || makeCall == nil {
		if proof == rebuildProofReactSetState {
			return stats, fmt.Errorf("%w: invalid React setState inputs", ErrRebuildLinkInputSize)
		}
		return stats, fmt.Errorf("%w: invalid setState inputs", ErrRebuildLinkInputSize)
	}
	occupied, err := rebuildGuardHasPrefix(ctx, b.pending, rebuildSetStatePrefix)
	if err != nil {
		return stats, err
	}
	if occupied {
		return stats, invalid("setState scratch range occupied")
	}
	defer func() {
		err = errors.Join(err, b.pending.DeleteRange(rebuildSetStatePrefix, prefixEnd(rebuildSetStatePrefix), pebble.NoSync))
	}()
	p := rebuildSetStatePass{b: b, proof: proof}
	// Capture stops at the first custody conflict, before replaying receipts.
	// Only the completed membership proof scan counts all unproved inputs.
	active, err := p.capture(ctx, lifecycle, isSetState)
	if err != nil || !active {
		return p.stats, err
	}
	if err := p.record(ctx, receipts); err != nil {
		return stats, err
	}
	if err := p.elect(ctx, lifecycle, makeCall); err != nil {
		return stats, err
	}
	err = b.withWrites(func() error { return p.apply(ctx) })
	return p.stats, err
}

type rebuildSetStatePass struct {
	b     *BuildStore
	proof rebuildEdgeProof
	stats RebuildReactSetStateStats
}

// c: highest live lifecycle candidate ID by class; m: candidate methods;
// q: classes that can emit from a gated caller; r: membership receipt key ->
// live key; o: first owner by method; t: qualifying call receipt key -> caller;
// g: live call gate; p: proved call gate; n: declaration proof;
// l: lifecycle winner by class; w: staged output by exact key;
// x: unproved output keys.
func setStateScratch(col byte, fields ...string) []byte {
	return keyStrings([]byte{'g', 'W', col}, fields...)
}

func (p *rebuildSetStatePass) put(key, value []byte) error {
	return p.b.putRebuildScratch(key, value, 16<<20, "setState")
}

func (p *rebuildSetStatePass) get(key []byte) ([]byte, error) {
	return pendingGet(p.b.pending, key, copyValue)
}

func (p *rebuildSetStatePass) mark(col byte, id string) error {
	return p.put(setStateScratch(col, id), []byte{1})
}

func (p *rebuildSetStatePass) marked(col byte, id string) (bool, error) {
	v, err := p.get(setStateScratch(col, id))
	return len(v) != 0, err
}

func (p *rebuildSetStatePass) node(ctx context.Context, id string) (*graph.Node, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	key := pendingNodeKey(id)
	return pendingGet(p.b.pending, key, func(raw []byte) (*graph.Node, error) {
		if len(key)+len(raw) > 16<<20 {
			return nil, fmt.Errorf("%w: setState method row", ErrRebuildLinkInputSize)
		}
		n, err := decodeNodeLight(raw)
		if err == nil && n.ID != id {
			return nil, invalid("setState method identity")
		}
		return n, err
	})
}

func setStateInput(r row) (*graph.Edge, error) {
	if len(r.key)+len(r.value) > 16<<20 {
		return nil, fmt.Errorf("%w: setState edge row", ErrRebuildLinkInputSize)
	}
	return guardLightEdge(r)
}

func (p *rebuildSetStatePass) capture(ctx context.Context, lifecycle string, isSetState func(string) bool) (bool, error) {
	active := false
	// The native membership projection has no class-node join. A missing
	// class declaration does not exclude the class or its member methods.
	if err := p.b.RebuildView().visit(ctx, []byte{familyEdge, edgeOut}, func(_ *pebble.Snapshot, r row) error {
		kind, err := rebuildAttributionEdgeKind(r.key)
		if err != nil || kind != graph.EdgeMemberOf {
			return err
		}
		e, err := setStateInput(r)
		if err != nil || e.To == "" {
			return err
		}
		n, err := p.node(ctx, e.From)
		if err != nil || n == nil || n.Kind != graph.KindMethod || n.Name != lifecycle || n.ID == "" {
			return err
		}
		active = true
		key := setStateScratch('c', e.To)
		highest, err := p.get(key)
		if err != nil || string(highest) >= n.ID {
			return err
		}
		return p.put(key, []byte(n.ID))
	}); err != nil || !active {
		return false, err
	}
	if err := pendingVisit(ctx, p.b.pending, setStateScratch('c'), func(r row) error {
		class, _, err := readKeyString(r.key[3:])
		if err != nil {
			return err
		}
		return pendingVisit(ctx, p.b.pending, keyStrings([]byte{familyEdge, edgeIn}, class), func(r row) error {
			id, err := decodeLinkRecorder(r.value)
			if err != nil || id.Kind != graph.EdgeMemberOf {
				return err
			}
			n, err := p.node(ctx, id.From)
			if err != nil || n == nil || n.Kind != graph.KindMethod || n.ID == "" {
				return err
			}
			return p.mark('m', n.ID)
		})
	}); err != nil {
		return false, err
	}
	return p.captureGates(ctx, lifecycle, isSetState)
}

func (p *rebuildSetStatePass) captureGates(ctx context.Context, lifecycle string, isSetState func(string) bool) (bool, error) {
	active := false
	if err := p.methods(ctx, func(n *graph.Node) error {
		gated, useful := false, false
		if err := pendingVisit(ctx, p.b.pending, guardOutgoing(n.ID, graph.EdgeCalls), func(r row) error {
			e, err := setStateInput(r)
			if err != nil || !isSetState(e.To) {
				return err
			}
			if !gated {
				gated = true
				useful, err = p.markCallerClasses(ctx, n, lifecycle)
				if err != nil {
					return err
				}
			}
			if !useful {
				return nil
			}
			return p.wantCall(e, r.key)
		}); err != nil || !useful {
			return err
		}
		active = true
		if err := p.mark('g', n.ID); err != nil {
			return err
		}
		// Any owner can win first, even one with no lifecycle target.
		return p.memberships(ctx, n.ID, p.wantMembership)
	}); err != nil || !active {
		return false, err
	}
	return true, p.methods(ctx, func(n *graph.Node) error {
		if n.Name != lifecycle {
			return nil
		}
		gated, err := p.marked('g', n.ID)
		if err != nil || gated {
			return err
		}
		needed := false
		if err := p.memberships(ctx, n.ID, func(e *graph.Edge, _ []byte) error {
			match, err := p.marked('q', e.To)
			needed = needed || match
			return err
		}); err != nil || !needed {
			return err
		}
		// An owner outside the gated classes can still win first and
		// disqualify this lifecycle target. Prove every competing owner.
		return p.memberships(ctx, n.ID, p.wantMembership)
	})
}

func (p *rebuildSetStatePass) markCallerClasses(ctx context.Context, n *graph.Node, lifecycle string) (bool, error) {
	useful := false
	err := p.memberships(ctx, n.ID, func(e *graph.Edge, _ []byte) error {
		highest, err := p.get(setStateScratch('c', e.To))
		if err != nil || len(highest) == 0 {
			return err
		}
		// If this caller owns the class, earlier lifecycle IDs cannot beat
		// it. At least one later ID is needed for a non-self output.
		if n.Name == lifecycle && string(highest) <= n.ID {
			return nil
		}
		useful = true
		return p.mark('q', e.To)
	})
	return useful, err
}

func (p *rebuildSetStatePass) memberships(ctx context.Context, id string, yield func(*graph.Edge, []byte) error) error {
	return pendingVisit(ctx, p.b.pending, guardOutgoing(id, graph.EdgeMemberOf), func(r row) error {
		e, err := setStateInput(r)
		if err != nil {
			return err
		}
		return yield(e, r.key)
	})
}

func (p *rebuildSetStatePass) methods(ctx context.Context, yield func(*graph.Node) error) error {
	return pendingVisit(ctx, p.b.pending, setStateScratch('m'), func(r row) error {
		id, _, err := readKeyString(r.key[3:])
		if err != nil {
			return err
		}
		n, err := p.node(ctx, id)
		if err != nil {
			return err
		}
		if n == nil || n.Kind != graph.KindMethod {
			return invalid("setState lost candidate method")
		}
		return yield(n)
	})
}

func (p *rebuildSetStatePass) wantMembership(e *graph.Edge, live []byte) error {
	custody, err := readRebuildEdgeCustody(p.b.pending, live)
	if err != nil {
		return err
	}
	if custody.proof != 0 {
		return &RebuildUnknownProvenanceError{Count: 1, First: graph.EdgeIdentityFor(e)}
	}
	raw := custody.raw
	if raw == nil {
		raw = live
	}
	key := append(setStateScratch('r'), raw...)
	prior, err := p.get(key)
	if err != nil {
		return err
	}
	if prior != nil && !bytes.Equal(prior[1:], live) {
		return &RebuildUnknownProvenanceError{Count: 1, First: graph.EdgeIdentityFor(e)}
	}
	return p.put(key, append([]byte{rebuildCapabilityWanted}, live...))
}

func (p *rebuildSetStatePass) callProof(proof rebuildEdgeProof) bool {
	switch proof {
	case rebuildProofTemporalSynthetic, rebuildProofEventChannel, rebuildProofRNBridge, rebuildProofObserver, rebuildProofClosureCollection:
		return true
	case rebuildProofReactSetState:
		// React runs before Flutter. Neither turn can prove itself.
		return p.proof == rebuildProofFlutterSetState
	default:
		return false
	}
}

func (p *rebuildSetStatePass) wantCall(e *graph.Edge, live []byte) error {
	custody, err := readRebuildEdgeCustody(p.b.pending, live)
	if errors.Is(err, ErrRebuildUnknownProvenance) {
		// A stale producer digest cannot prove the gate. Another live
		// call may still supply a valid witness for this caller.
		return nil
	}
	if err != nil {
		return err
	}
	if custody.proof != 0 {
		if p.callProof(custody.proof) {
			return p.mark('p', e.From)
		}
		return nil // not a proven witness of the existential call predicate
	}
	raw := custody.raw
	if raw == nil {
		raw = live
	}
	key := append(setStateScratch('t'), raw...)
	prior, err := p.get(key)
	if err != nil {
		return err
	}
	if prior != nil && string(prior) != e.From {
		return &RebuildUnknownProvenanceError{Count: 1, First: graph.EdgeIdentityFor(e)}
	}
	return p.put(key, []byte(e.From))
}

func (p *rebuildSetStatePass) record(ctx context.Context, receipts iter.Seq2[ParseFacts, error]) error {
	for facts, err := range receipts {
		if err != nil {
			return err
		}
		file := pathkey.Normalize(facts.File.FilePath)
		for _, e := range facts.Edges {
			if err := ctx.Err(); err != nil {
				return err
			}
			admitted := facts.EffectiveLanguage != "" && e.FilePath == file
			if !admitted && e.Kind != graph.EdgeMemberOf {
				continue
			}
			key := pendingEdgeKey(e)
			if e.Kind == graph.EdgeMemberOf {
				if err := p.noteMembership(key, admitted); err != nil {
					return err
				}
			}
			if !admitted {
				continue
			}
			caller, err := p.get(append(setStateScratch('t'), key...))
			if err != nil {
				return err
			}
			if caller != nil {
				if err := p.mark('p', string(caller)); err != nil {
					return err
				}
			}
		}
		for _, n := range facts.Nodes {
			if err := ctx.Err(); err != nil {
				return err
			}
			if err := p.noteNode(ctx, n, file, facts.EffectiveLanguage); err != nil {
				return err
			}
		}
	}
	// Capture rejects receipt-key collisions, so each row represents one
	// distinct live membership. Receipt-key order can differ from live order.
	var refused rebuildScratchRefusals
	if err := pendingVisit(ctx, p.b.pending, setStateScratch('r'), func(r row) error {
		if len(r.value) < 2 {
			return invalid("setState membership proof state")
		}
		if r.value[0] == rebuildScratchExact {
			return nil
		}
		live := r.value[1:]
		id, err := decodeLinkRecorder(live)
		if err != nil {
			return err
		}
		refused.add(id, bytes.Clone(live))
		return nil
	}); err != nil {
		return err
	}
	return refused.err()
}

func (p *rebuildSetStatePass) noteMembership(raw []byte, admitted bool) error {
	key := append(setStateScratch('r'), raw...)
	state, err := p.get(key)
	if err != nil || len(state) == 0 {
		return err
	}
	if !admitted {
		// Do not let a later matching receipt erase an earlier unproved
		// position of this same membership in the owner election.
		return noteRebuildScratchPayload(p.put, key, state, rebuildCapabilityUnproved)
	}
	if state[0] != rebuildCapabilityWanted {
		return nil
	}
	id, err := decodeLinkRecorder(state[1:])
	if err != nil {
		return err
	}
	owner := setStateScratch('o', id.From)
	prior, err := p.get(owner)
	if err != nil {
		return err
	}
	// Encounter order is the receipt ordinal. An empty first owner still
	// consumes this method's election; it is not permission to try another.
	if prior == nil {
		if err := p.put(owner, append([]byte{1}, id.To...)); err != nil {
			return err
		}
	}
	return noteRebuildScratchPayload(p.put, key, state, rebuildCapabilityProved)
}

func (p *rebuildSetStatePass) noteNode(ctx context.Context, raw *graph.Node, file, language string) error {
	candidate, err := p.marked('m', raw.ID)
	if err != nil || !candidate {
		return err
	}
	live, err := p.node(ctx, raw.ID)
	if err != nil {
		return err
	}
	bit := byte(rebuildCapabilityUnproved)
	if live != nil && language == raw.Language && raw.FilePath == file && live.FilePath == file &&
		live.Name == raw.Name && live.Kind == raw.Kind && live.StartLine == raw.StartLine &&
		live.Language == raw.Language && live.RepoPrefix == raw.RepoPrefix {
		bit = rebuildCapabilityProved
	}
	key := setStateScratch('n', raw.ID)
	prior, err := p.get(key)
	if err != nil {
		return err
	}
	if len(prior) != 0 {
		bit |= prior[0]
	}
	return p.put(key, []byte{bit})
}

func (p *rebuildSetStatePass) owner(id string) (string, error) {
	value, err := p.get(setStateScratch('o', id))
	if err != nil || len(value) == 0 {
		return "", err
	}
	return string(value[1:]), nil
}

func (p *rebuildSetStatePass) elect(ctx context.Context, lifecycle string, makeCall func(*graph.Node, *graph.Node, string) *graph.Edge) error {
	// Native explicitly sorts method IDs, then replaces the target for
	// each selected class. Preserve that sort rather than receipt order.
	if err := p.methods(ctx, func(n *graph.Node) error {
		if n.Name != lifecycle {
			return nil
		}
		class, err := p.owner(n.ID)
		if err != nil || class == "" {
			return err
		}
		return p.put(setStateScratch('l', class), []byte(n.ID))
	}); err != nil {
		return err
	}
	if err := p.methods(ctx, func(n *graph.Node) error {
		class, err := p.owner(n.ID)
		if err != nil || class == "" {
			return err
		}
		id, err := p.get(setStateScratch('l', class))
		if err != nil || id == nil || string(id) == n.ID {
			return err
		}
		gate, err := p.marked('g', n.ID)
		if err != nil || !gate {
			return err
		}
		target, err := p.node(ctx, string(id))
		if err != nil {
			return err
		}
		if target == nil {
			return invalid("setState lost lifecycle method")
		}
		e := makeCall(n, target, class)
		key := pendingEdgeKey(e)
		prior, err := p.b.pendingEdge(key)
		if err != nil || prior != nil {
			return err
		}
		proved, err := p.marked('p', n.ID)
		if err != nil {
			return err
		}
		for _, endpoint := range [...]string{n.ID, target.ID} {
			state, err := p.get(setStateScratch('n', endpoint))
			if err != nil {
				return err
			}
			proved = proved && len(state) == 1 && state[0] == rebuildCapabilityProved
		}
		if !proved {
			return p.put(append(setStateScratch('x'), key...), nil)
		}
		raw, err := encodeEdge(e)
		if err != nil {
			return err
		}
		return p.put(append(setStateScratch('w'), key...), raw)
	}); err != nil {
		return err
	}
	prefix := setStateScratch('x')
	var refused rebuildScratchRefusals
	if err := pendingVisit(ctx, p.b.pending, prefix, func(r row) error {
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

func (p *rebuildSetStatePass) apply(ctx context.Context) error {
	return applyRebuildScratchEdges(ctx, p.visit, p.b, p.proof, &p.stats.Edges)
}

func (p *rebuildSetStatePass) visit(ctx context.Context, yield func(*graph.Edge) error) error {
	return visitRebuildScratchKeyedEdges(ctx, p.b.pending, setStateScratch('w'), 0, "setState proposal key", yield)
}
