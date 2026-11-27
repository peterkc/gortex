package store_pebble

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"iter"
	"reflect"
	"slices"
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
		return applyRebuildScratchEdges(ctx, p.visitProposals, b, rebuildProofKMPExpectActual, &p.stats.Edges)
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
// proof by ID; k: Kotlin marker proof by ID; w: proposal by traversal
// ordinal then full edge key.
func rebuildKMPKey(col byte, fields ...string) []byte {
	return keyStrings(append(bytes.Clone(rebuildKMPPrefix), col), fields...)
}

func (p *rebuildKMPPass) put(key, value []byte) error {
	return p.b.putRebuildScratch(key, value, rebuildKMPRowLimit, p.spillAt, &p.buffered, &p.stats.SpillBytes, "KMP expect/actual")
}

func rebuildKMPCheckNodeSize(r row) error {
	if len(r.key)+len(r.value) > rebuildKMPRowLimit {
		return fmt.Errorf("%w: KMP expect/actual node row", ErrRebuildLinkInputSize)
	}
	return nil
}

func rebuildKMPHasKotlin(ctx context.Context, view RebuildView) (bool, error) {
	// A normalized Kotlin marker supplies both native registry gates:
	// the JVM family and the Kotlin marker. Neither restricts endpoints.
	return view.hasLightNode(ctx, "KMP expect/actual", func(n *graph.Node) bool {
		return isRebuildKotlin(n.Language)
	})
}

func isRebuildKotlin(language string) bool {
	return strings.ToLower(strings.TrimSpace(language)) == graph.KMPKotlinLanguage
}

func (p *rebuildKMPPass) collect(ctx context.Context, view RebuildView) error {
	kinds := graph.KMPExpectActualKinds()
	return view.visit(ctx, []byte{familyNode}, func(_ *pebble.Snapshot, r row) error {
		light, err := checkedRebuildLightNode(r, "KMP expect/actual")
		if err != nil || light.Name == "" || !slices.Contains(kinds, light.Kind) {
			return err
		}
		if err := rebuildKMPCheckNodeSize(r); err != nil {
			return err
		}
		n, err := decodeNode(r.value)
		if err != nil {
			return err
		}
		var col byte
		switch graph.KMPExpectActualRole(n) {
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
			if err != nil {
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
	return rebuildScratchState(p.b.pending, key, "KMP expect/actual proof state")
}

func (p *rebuildKMPPass) prove(ctx context.Context, receipts iter.Seq2[ParseFacts, error]) error {
	// A live Kotlin label can open the census, but cannot authorize a
	// proposed row. One exact receipt marker suffices for every pair.
	if err := p.b.RebuildView().visit(ctx, []byte{familyNode}, func(_ *pebble.Snapshot, r row) error {
		n, err := checkedRebuildLightNode(r, "KMP expect/actual")
		if err != nil || !isRebuildKotlin(n.Language) {
			return err
		}
		if err := rebuildKMPCheckNodeSize(r); err != nil {
			return err
		}
		return p.put(rebuildKMPKey('k', n.ID), []byte{rebuildCapabilityWanted})
	}); err != nil {
		return err
	}
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
	var kotlinProved bool
	if err := pendingVisit(ctx, p.b.pending, rebuildKMPKey('k'), func(r row) error {
		if len(r.value) != 1 {
			return invalid("KMP Kotlin marker proof state")
		}
		kotlinProved = kotlinProved || r.value[0] == rebuildScratchExact
		return nil
	}); err != nil {
		return err
	}
	return proveRebuildScratchEndpoints(ctx, p.visitProposals, func(id string) (bool, error) {
		if !kotlinProved {
			return false, nil
		}
		state, err := p.state(rebuildKMPKey('n', id))
		return state == rebuildScratchExact, err
	}, pendingEdgeKey)
}

func (p *rebuildKMPPass) noteReceipt(raw *graph.Node, file, language string) error {
	key, marker := rebuildKMPKey('n', raw.ID), rebuildKMPKey('k', raw.ID)
	state, err := p.state(key)
	if err != nil {
		return err
	}
	markerState, err := p.state(marker)
	if err != nil || (state == 0 && markerState == 0) {
		return err
	}
	live, err := p.b.pendingNode(raw.ID)
	if err != nil {
		return err
	}
	proved := language != "" && language == raw.Language && raw.FilePath == file && sameRebuildKMPInput(raw, live)
	bit := rebuildScratchProofBit(proved)
	if state != 0 {
		if err := noteRebuildScratchState(p.put, key, state, bit); err != nil {
			return err
		}
	}
	if markerState != 0 {
		return noteRebuildScratchState(p.put, marker, markerState, bit)
	}
	return nil
}

func sameRebuildKMPInput(raw, live *graph.Node) bool {
	if live == nil || live.ID != raw.ID || live.Kind != raw.Kind || live.Language != raw.Language || live.Name != raw.Name ||
		live.FilePath != raw.FilePath || live.StartLine != raw.StartLine || live.RepoPrefix != raw.RepoPrefix {
		return false
	}
	value, present := raw.Meta["kmp_role"]
	current, found := live.Meta["kmp_role"]
	return present == found && reflect.DeepEqual(value, current)
}

func (p *rebuildKMPPass) visitProposals(ctx context.Context, yield func(*graph.Edge) error) error {
	return visitRebuildScratchKeyedEdges(ctx, p.b.pending, rebuildKMPKey('w'), 8, "KMP expect/actual proposal key", yield)
}
