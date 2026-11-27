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

var ErrRebuildReactSetState = errors.New("pebble rebuild React setState stage failed")

var rebuildSetStatePrefix = []byte{'g', 'W'}

// RebuildReactSetStateStats reports installed calls and physical spill bytes.
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
) (stats RebuildReactSetStateStats, err error) {
	defer func() {
		if err != nil {
			stats = RebuildReactSetStateStats{}
			err = fmt.Errorf("%w: %w", ErrRebuildReactSetState, err)
		}
	}()
	if receipts == nil || spillAt < 1 || spillAt > goldBufferBytes || isSetState == nil || makeCall == nil {
		return stats, fmt.Errorf("%w: invalid React setState inputs", ErrRebuildLinkInputSize)
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
	p := rebuildSetStatePass{b: b, spillAt: spillAt}
	active, err := p.capture(ctx, "render", isSetState)
	if err != nil || !active {
		return p.stats, err
	}
	if err := p.record(ctx, receipts); err != nil {
		return stats, err
	}
	if err := p.elect(ctx, "render", makeCall); err != nil {
		return stats, err
	}
	err = b.withWrites(func() error { return p.apply(ctx, rebuildProofReactSetState) })
	return p.stats, err
}

type rebuildSetStatePass struct {
	b        *BuildStore
	spillAt  int
	buffered int
	stats    RebuildReactSetStateStats
}

// c: lifecycle classes; m: candidate methods; r: exact membership receipt
// identity -> live key; o: first owner by method; t: qualifying call receipt
// identity -> caller; g: live call gate; p: proved call gate; n: declaration
// proof; l: lifecycle winner by class; w: staged output by exact key.
func setStateScratch(col byte, fields ...string) []byte {
	return keyStrings([]byte{'g', 'W', col}, fields...)
}

func (p *rebuildSetStatePass) put(key, value []byte) error {
	if len(key)+len(value) > 16<<20 {
		return fmt.Errorf("%w: setState scratch row", ErrRebuildLinkInputSize)
	}
	if err := p.b.pending.Set(key, value, pebble.NoSync); err != nil {
		return err
	}
	p.buffered += len(key) + len(value)
	if p.buffered >= p.spillAt {
		before := p.b.pending.Metrics().Levels[0].TableBytesFlushed
		if err := p.b.pending.Flush(); err != nil {
			return err
		}
		p.stats.SpillBytes += p.b.pending.Metrics().Levels[0].TableBytesFlushed - before
		p.buffered = 0
	}
	return nil
}

func (p *rebuildSetStatePass) get(key []byte) ([]byte, error) {
	return pendingGet(p.b.pending, key, func(raw []byte) ([]byte, error) { return bytes.Clone(raw), nil })
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
		if err != nil || n == nil || n.Kind != graph.KindMethod || n.Name != lifecycle {
			return err
		}
		active = true
		return p.mark('c', e.To)
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
	return true, p.methods(ctx, func(n *graph.Node) error {
		// Include every live membership, including owners outside the seed
		// classes. Raw ancestry proves receipt order, not private key order.
		if err := pendingVisit(ctx, p.b.pending, guardOutgoing(n.ID, graph.EdgeMemberOf), func(r row) error {
			e, err := setStateInput(r)
			if err != nil {
				return err
			}
			return p.wantMembership(e, r.key)
		}); err != nil {
			return err
		}
		return pendingVisit(ctx, p.b.pending, guardOutgoing(n.ID, graph.EdgeCalls), func(r row) error {
			e, err := setStateInput(r)
			if err != nil || !isSetState(e.To) {
				return err
			}
			if err := p.mark('g', n.ID); err != nil {
				return err
			}
			return p.wantCall(e, r.key)
		})
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

func setStateCallProof(proof rebuildEdgeProof) bool {
	switch proof {
	case rebuildProofTemporalSynthetic, rebuildProofEventChannel, rebuildProofRNBridge, rebuildProofObserver, rebuildProofClosureCollection, rebuildProofReactSetState:
		return true
	default:
		return false
	}
}

func (p *rebuildSetStatePass) wantCall(e *graph.Edge, live []byte) error {
	custody, err := readRebuildEdgeCustody(p.b.pending, live)
	if err != nil {
		return err
	}
	if custody.proof != 0 {
		if setStateCallProof(custody.proof) {
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
			if facts.EffectiveLanguage == "" || e.FilePath != file {
				continue
			}
			key := pendingEdgeKey(e)
			if err := p.noteMembership(key); err != nil {
				return err
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
	return pendingVisit(ctx, p.b.pending, setStateScratch('r'), func(r row) error {
		if len(r.value) < 2 {
			return invalid("setState membership proof state")
		}
		if r.value[0] == rebuildCapabilityWanted|rebuildCapabilityProved {
			return nil
		}
		id, err := decodeLinkRecorder(r.value[1:])
		if err != nil {
			return err
		}
		return &RebuildUnknownProvenanceError{Count: 1, First: id}
	})
}

func (p *rebuildSetStatePass) noteMembership(raw []byte) error {
	key := append(setStateScratch('r'), raw...)
	state, err := p.get(key)
	if err != nil || len(state) == 0 {
		return err
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
	state[0] |= rebuildCapabilityProved
	return p.put(key, state)
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
	var refused RebuildUnknownProvenanceError
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
			if refused.Count == 0 {
				refused.First = graph.EdgeIdentityFor(e)
			}
			refused.Count++
			return nil
		}
		raw, err := encodeEdge(e)
		if err != nil {
			return err
		}
		return p.put(append(setStateScratch('w'), key...), raw)
	}); err != nil {
		return err
	}
	if refused.Count != 0 {
		return &refused
	}
	return nil
}

func (p *rebuildSetStatePass) apply(ctx context.Context, proof rebuildEdgeProof) error {
	prefix := setStateScratch('w')
	return pendingVisit(ctx, p.b.pending, prefix, func(r row) error {
		e, err := decodeEdge(r.value)
		if err != nil {
			return err
		}
		if !bytes.Equal(r.key[len(prefix):], pendingEdgeKey(e)) {
			return invalid("setState proposal key")
		}
		if err := p.b.putEdgeWithCustody(e, rebuildEdgeCustody{proof: proof}); err != nil {
			return err
		}
		p.stats.Edges++
		return nil
	})
}
