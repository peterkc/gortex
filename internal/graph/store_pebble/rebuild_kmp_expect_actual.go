package store_pebble

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"iter"
	"strings"

	"github.com/cockroachdb/pebble/v2"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/pathkey"
)

var ErrRebuildKMPExpectActual = errors.New("pebble rebuild KMP expect/actual stage failed")

var rebuildKMPPrefix = []byte{'g', 'M'}

const rebuildKMPRowLimit = 16 << 20

// RebuildKMPExpectActualStats reports installed edges and physical spill bytes.
type RebuildKMPExpectActualStats struct {
	Edges, SpillBytes uint64
}

// RebuildKMPExpectActual pairs all eligible live declarations by name and kind.
// Verify both receipt owners of every proposed new row before applying any.
func (b *BuildStore) RebuildKMPExpectActual(ctx context.Context, receipts iter.Seq2[ParseFacts, error], spillAt int,
	makeEdge func(*graph.Node, string, graph.EdgeKind) *graph.Edge,
) (stats RebuildKMPExpectActualStats, err error) {
	defer func() {
		if err != nil {
			stats = RebuildKMPExpectActualStats{}
			err = fmt.Errorf("%w: %w", ErrRebuildKMPExpectActual, err)
		}
	}()
	if receipts == nil || spillAt < 1 || spillAt > goldBufferBytes || makeEdge == nil {
		return stats, fmt.Errorf("%w: invalid KMP expect/actual inputs", ErrRebuildLinkInputSize)
	}
	occupied, err := rebuildGuardHasPrefix(ctx, b.pending, rebuildKMPPrefix)
	if err != nil {
		return stats, err
	}
	if occupied {
		return stats, invalid("KMP expect/actual scratch range occupied")
	}
	view := b.RebuildView()
	active, err := rebuildKMPHasKotlin(ctx, view)
	if err != nil || !active {
		return stats, err
	}
	defer func() {
		err = errors.Join(err, b.pending.DeleteRange(rebuildKMPPrefix, prefixEnd(rebuildKMPPrefix), pebble.NoSync))
	}()
	p := rebuildKMPPass{b: b, spillAt: spillAt}
	if err := p.collect(ctx, view); err != nil {
		return stats, err
	}
	if err := p.pair(ctx, makeEdge); err != nil || p.proposals == 0 {
		return p.stats, err
	}
	if err := p.prove(ctx, receipts); err != nil {
		return stats, err
	}
	err = b.withWrites(func() error {
		return p.visitProposals(ctx, func(e *graph.Edge) error {
			if err := b.putEdgeWithCustody(e, rebuildEdgeCustody{proof: rebuildProofKMPExpectActual}); err != nil {
				return err
			}
			p.stats.Edges++
			return nil
		})
	})
	return p.stats, err
}

type rebuildKMPPass struct {
	b         *BuildStore
	spillAt   int
	buffered  int
	proposals uint64
	stats     RebuildKMPExpectActualStats
}

// e/a: eligible expect/actual row by name, kind, ID; n: wanted endpoint
// proof by ID; w: proposal by traversal ordinal then full edge key.
func rebuildKMPKey(col byte, fields ...string) []byte {
	return keyStrings([]byte{'g', 'M', col}, fields...)
}

func (p *rebuildKMPPass) put(key, value []byte) error {
	if len(key)+len(value) > rebuildKMPRowLimit {
		return fmt.Errorf("%w: KMP expect/actual scratch row", ErrRebuildLinkInputSize)
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

func rebuildKMPNodeLight(r row) (*graph.Node, error) {
	if len(r.key)+len(r.value) > rebuildKMPRowLimit {
		return nil, fmt.Errorf("%w: KMP expect/actual node row", ErrRebuildLinkInputSize)
	}
	n, err := decodeNodeLight(r.value)
	if err == nil && !bytes.Equal(r.key, pendingNodeKey(n.ID)) {
		err = invalid("KMP expect/actual node identity")
	}
	return n, err
}

func rebuildKMPHasKotlin(ctx context.Context, view RebuildView) (bool, error) {
	// The native registry requires JVM plus a Kotlin marker anywhere in
	// the node census. A normalized Kotlin label satisfies both tests.
	// This is a pass-level check, not an endpoint language restriction.
	found := false
	err := view.visit(ctx, []byte{familyNode}, func(_ *pebble.Snapshot, r row) error {
		if found {
			return nil
		}
		n, err := rebuildKMPNodeLight(r)
		if err == nil {
			found = strings.ToLower(strings.TrimSpace(n.Language)) == "kotlin"
		}
		return err
	})
	return found, err
}

func (p *rebuildKMPPass) collect(ctx context.Context, view RebuildView) error {
	return view.visit(ctx, []byte{familyNode}, func(_ *pebble.Snapshot, r row) error {
		light, err := rebuildKMPNodeLight(r)
		if err != nil || light.Name == "" {
			return err
		}
		switch light.Kind {
		case graph.KindFunction, graph.KindMethod, graph.KindType, graph.KindInterface:
		default:
			return nil
		}
		n, err := decodeNode(r.value)
		if err != nil {
			return err
		}
		role, _ := n.Meta["kmp_role"].(string)
		var col byte
		switch role {
		case "expect":
			col = 'e'
		case "actual":
			col = 'a'
		default:
			return nil
		}
		// Only the fields read by kmpEdge or the join need to enter scratch.
		projection, err := encodeNode(&graph.Node{
			ID: n.ID, Name: n.Name, Kind: n.Kind, FilePath: n.FilePath, StartLine: n.StartLine,
		})
		if err != nil {
			return err
		}
		return p.put(rebuildKMPKey(col, n.Name, string(n.Kind), n.ID), projection)
	})
}

func (p *rebuildKMPPass) pair(ctx context.Context, makeEdge func(*graph.Node, string, graph.EdgeKind) *graph.Edge) error {
	// Preserve native (name, kind), then expect ID and actual ID order.
	// Each range is streamed; the pair product is never held in memory.
	return pendingVisit(ctx, p.b.pending, rebuildKMPKey('e'), func(r row) error {
		expect, err := decodeNodeLight(r.value)
		if err != nil {
			return err
		}
		prefix := rebuildKMPKey('a', expect.Name, string(expect.Kind))
		return pendingVisit(ctx, p.b.pending, prefix, func(r row) error {
			actual, err := decodeNodeLight(r.value)
			if err != nil || expect.ID == actual.ID {
				return err
			}
			if err := p.propose(makeEdge(actual, expect.ID, graph.EdgeImplements)); err != nil {
				return err
			}
			return p.propose(makeEdge(expect, actual.ID, graph.EdgeReferences))
		})
	})
}

func (p *rebuildKMPPass) propose(e *graph.Edge) error {
	if graph.IsProxyID(e.From) || graph.IsProxyID(e.To) || graph.StructuralEdgeTargetInvalid(e.Kind, e.To) {
		return nil
	}
	key := pendingEdgeKey(e)
	exists, err := pendingGet(p.b.pending, key, func([]byte) (bool, error) { return true, nil })
	if err != nil || exists {
		// SQLite INSERT OR IGNORE preserves each direction's incumbent.
		return err
	}
	raw, err := encodeEdge(e)
	if err != nil {
		return err
	}
	for _, id := range [...]string{e.From, e.To} {
		proof := rebuildKMPKey('n', id)
		state, err := p.state(proof)
		if err != nil {
			return err
		}
		if state == 0 {
			if err := p.put(proof, []byte{rebuildCapabilityWanted}); err != nil {
				return err
			}
		}
	}
	proposal := binary.BigEndian.AppendUint64(rebuildKMPKey('w'), p.proposals)
	p.proposals++
	return p.put(append(proposal, key...), raw)
}

func (p *rebuildKMPPass) state(key []byte) (byte, error) {
	return pendingGet(p.b.pending, key, func(raw []byte) (byte, error) {
		if len(raw) != 1 {
			return 0, invalid("KMP expect/actual proof state")
		}
		return raw[0], nil
	})
}

func (p *rebuildKMPPass) prove(ctx context.Context, receipts iter.Seq2[ParseFacts, error]) error {
	for facts, err := range receipts {
		if err != nil {
			return err
		}
		file := pathkey.Normalize(facts.File.FilePath)
		for _, raw := range facts.Nodes {
			if err := ctx.Err(); err != nil {
				return err
			}
			if err := p.noteReceipt(raw, file, facts.EffectiveLanguage); err != nil {
				return err
			}
		}
	}
	var refused RebuildUnknownProvenanceError
	var first []byte
	if err := p.visitProposals(ctx, func(e *graph.Edge) error {
		for _, id := range [...]string{e.From, e.To} {
			state, err := p.state(rebuildKMPKey('n', id))
			if err != nil {
				return err
			}
			if state != rebuildCapabilityWanted|rebuildCapabilityProved {
				key := pendingEdgeKey(e)
				if first == nil || bytes.Compare(key, first) < 0 {
					first = key
					refused.First = graph.EdgeIdentityFor(e)
				}
				refused.Count++
				break
			}
		}
		return nil
	}); err != nil {
		return err
	}
	if refused.Count != 0 {
		return &refused
	}
	return nil
}

func (p *rebuildKMPPass) noteReceipt(raw *graph.Node, file, language string) error {
	key := rebuildKMPKey('n', raw.ID)
	state, err := p.state(key)
	if err != nil || state == 0 {
		return err
	}
	live, err := p.b.pendingNode(raw.ID)
	if err != nil {
		return err
	}
	bit := byte(rebuildCapabilityUnproved)
	if language != "" && language == raw.Language && raw.FilePath == file && sameRebuildKMPInput(raw, live) {
		bit = rebuildCapabilityProved
	}
	if state&bit != 0 {
		return nil
	}
	return p.put(key, []byte{state | bit})
}

func sameRebuildKMPInput(raw, live *graph.Node) bool {
	if live == nil || live.ID != raw.ID || live.Kind != raw.Kind || live.Language != raw.Language || live.Name != raw.Name ||
		live.FilePath != raw.FilePath || live.StartLine != raw.StartLine || live.RepoPrefix != raw.RepoPrefix {
		return false
	}
	rawRole, _ := raw.Meta["kmp_role"].(string)
	liveRole, _ := live.Meta["kmp_role"].(string)
	return rawRole == liveRole
}

func (p *rebuildKMPPass) visitProposals(ctx context.Context, yield func(*graph.Edge) error) error {
	prefix := rebuildKMPKey('w')
	return pendingVisit(ctx, p.b.pending, prefix, func(r row) error {
		e, err := decodeEdge(r.value)
		if err != nil {
			return err
		}
		keyOffset := len(prefix) + 8 // ordinal width
		if len(r.key) < keyOffset || !bytes.Equal(r.key[keyOffset:], pendingEdgeKey(e)) {
			return invalid("KMP expect/actual proposal key")
		}
		return yield(e)
	})
}
