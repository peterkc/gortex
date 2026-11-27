package store_pebble

import (
	"context"
	"errors"
	"fmt"
	"iter"
	"reflect"

	"github.com/cockroachdb/pebble/v2"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/pathkey"
)

var ErrRebuildSwiftObjC = errors.New("pebble rebuild Swift/ObjC stage failed")

var rebuildSwiftObjCPrefix = []byte{'g', 'J'}

// RebuildSwiftObjCStats reports installed edges and physical spill bytes.
type RebuildSwiftObjCStats struct {
	Edges, SpillBytes uint64
}

// RebuildSwiftObjC stages selector and protocol join results from the
// resolver in spillable scratch. Preserve existing durable keys for SQLite
// parity and prove every new edge before applying any of them. All language
// declarations remain available.
func (b *BuildStore) RebuildSwiftObjC(ctx context.Context, receipts iter.Seq2[ParseFacts, error], spillAt int,
	reduce func(RebuildView, func(*graph.Edge) error) error,
) (stats RebuildSwiftObjCStats, err error) {
	defer func() {
		if err != nil {
			stats = RebuildSwiftObjCStats{}
			err = fmt.Errorf("%w: %w", ErrRebuildSwiftObjC, err)
		}
	}()
	if spillAt < 1 || spillAt > goldBufferBytes || receipts == nil || reduce == nil {
		return stats, fmt.Errorf("%w: invalid Swift/ObjC stage inputs", ErrRebuildLinkInputSize)
	}
	occupied, err := rebuildGuardHasPrefix(ctx, b.pending, rebuildSwiftObjCPrefix)
	if err != nil {
		return stats, err
	}
	if occupied {
		return stats, invalid("Swift/ObjC scratch range occupied")
	}
	defer func() {
		err = errors.Join(err, b.pending.DeleteRange(rebuildSwiftObjCPrefix, prefixEnd(rebuildSwiftObjCPrefix), pebble.NoSync))
	}()
	p := rebuildSwiftObjCPass{b: b, spillAt: spillAt}
	if err := reduce(b.RebuildView(), p.propose); err != nil {
		return stats, err
	}
	if !p.changed {
		return stats, nil
	}
	if err := p.prove(ctx, receipts); err != nil {
		return stats, err
	}
	err = b.withWrites(func() error { return p.apply(ctx) })
	return p.stats, err
}

type rebuildSwiftObjCPass struct {
	b        *BuildStore
	spillAt  int
	buffered int
	changed  bool
	stats    RebuildSwiftObjCStats
}

func rebuildSwiftObjCKey(col byte, fields ...string) []byte {
	return keyStrings([]byte{'g', 'J', col}, fields...)
}

func (p *rebuildSwiftObjCPass) put(key, value []byte) error {
	return p.b.putRebuildScratch(key, value, 16<<20, p.spillAt, &p.buffered, &p.stats.SpillBytes, "Swift/ObjC")
}

func (p *rebuildSwiftObjCPass) propose(e *graph.Edge) error {
	key := pendingEdgeKey(e)
	prior, err := p.b.pendingEdge(key)
	if err != nil || prior != nil {
		return err
	}
	proposal := append(rebuildSwiftObjCKey('e'), key...)
	exists, err := pendingGet(p.b.pending, proposal, func([]byte) (bool, error) { return true, nil })
	if err != nil || exists {
		// Duplicate explicit and heuristic matches have identical payload.
		return err
	}
	raw, err := encodeEdge(e)
	if err != nil {
		return err
	}
	if err := p.put(rebuildSwiftObjCKey('n', e.From), []byte{rebuildCapabilityWanted}); err != nil {
		return err
	}
	if err := p.put(rebuildSwiftObjCKey('n', e.To), []byte{rebuildCapabilityWanted}); err != nil {
		return err
	}
	p.changed = true
	return p.put(proposal, raw)
}

func (p *rebuildSwiftObjCPass) state(key []byte) (byte, error) {
	return rebuildScratchState(p.b.pending, key, "Swift/ObjC proof state")
}

func (p *rebuildSwiftObjCPass) prove(ctx context.Context, receipts iter.Seq2[ParseFacts, error]) error {
	for facts, err := range receipts {
		if err != nil {
			return err
		}
		file := pathkey.Normalize(facts.File.FilePath)
		for _, n := range facts.Nodes {
			if err := ctx.Err(); err != nil {
				return err
			}
			key := rebuildSwiftObjCKey('n', n.ID)
			state, err := p.state(key)
			if err != nil {
				return err
			}
			if state == 0 {
				continue
			}
			live, err := p.b.RebuildView().Node(ctx, n.ID)
			if err != nil {
				return err
			}
			proved := facts.EffectiveLanguage == n.Language && n.FilePath == file && sameRebuildSwiftObjCInput(n, live)
			if err := noteRebuildScratchState(p.put, key, state, rebuildScratchProofBit(proved)); err != nil {
				return err
			}
		}
	}
	return proveRebuildScratchEndpoints(ctx, p.visit, func(id string) (bool, error) {
		state, err := p.state(rebuildSwiftObjCKey('n', id))
		return state == rebuildScratchExact, err
	}, nil)
}

func sameRebuildSwiftObjCInput(raw, live *graph.Node) bool {
	if live == nil || live.ID != raw.ID || live.Kind != raw.Kind || live.Language != raw.Language ||
		live.Name != raw.Name || live.FilePath != raw.FilePath || live.StartLine != raw.StartLine || live.RepoPrefix != raw.RepoPrefix {
		return false
	}
	for _, key := range [...]string{"objc_selector", "objc_setter_selector", "objc_protocols", "objc"} {
		value, present := raw.Meta[key]
		current, found := live.Meta[key]
		if present != found || !reflect.DeepEqual(value, current) {
			return false
		}
	}
	return true
}

func (p *rebuildSwiftObjCPass) visit(ctx context.Context, yield func(*graph.Edge) error) error {
	return visitRebuildScratchKeyedEdges(ctx, p.b.pending, rebuildSwiftObjCKey('e'), 0, "Swift/ObjC proposal key", yield)
}

func (p *rebuildSwiftObjCPass) apply(ctx context.Context) error {
	return applyRebuildScratchEdges(ctx, p.visit, p.b, rebuildProofSwiftObjC, &p.stats.Edges)
}
