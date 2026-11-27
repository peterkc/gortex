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

var ErrRebuildClosureCollection = errors.New("pebble rebuild closure collection stage failed")

var rebuildClosurePrefix = []byte{'g', 'U'}

// RebuildClosureCollectionStats reports installed edges and physical spill bytes.
type RebuildClosureCollectionStats struct {
	Edges, SpillBytes uint64
}

// RebuildClosureCollection streams the native callable field join. Both
// raw role counts precede the workspace and self filters. Preserve SQLite
// durable keys and prove the declarations before installing new calls.
func (b *BuildStore) RebuildClosureCollection(ctx context.Context, receipts iter.Seq2[ParseFacts, error], spillAt int,
	maxFanout int, sameBoundary func(*graph.Node, *graph.Node) bool, makeEdge func(*graph.Node, *graph.Node, string) *graph.Edge,
) (stats RebuildClosureCollectionStats, err error) {
	defer func() {
		if err != nil {
			stats = RebuildClosureCollectionStats{}
			err = fmt.Errorf("%w: %w", ErrRebuildClosureCollection, err)
		}
	}()
	if spillAt < 1 || spillAt > goldBufferBytes || receipts == nil || maxFanout < 1 || sameBoundary == nil || makeEdge == nil {
		return stats, fmt.Errorf("%w: invalid closure collection stage inputs", ErrRebuildLinkInputSize)
	}
	occupied, err := rebuildGuardHasPrefix(ctx, b.pending, rebuildClosurePrefix)
	if err != nil {
		return stats, err
	}
	if occupied {
		return stats, invalid("closure collection scratch range occupied")
	}
	defer func() {
		err = errors.Join(err, b.pending.DeleteRange(rebuildClosurePrefix, prefixEnd(rebuildClosurePrefix), pebble.NoSync))
	}()
	p := rebuildClosurePass{b: b, spillAt: spillAt}
	if err := p.pair(ctx, maxFanout, sameBoundary, makeEdge); err != nil {
		return stats, err
	}
	if !p.changed {
		return p.stats, nil
	}
	if err := p.prove(ctx, receipts); err != nil {
		return stats, err
	}
	err = b.withWrites(func() error { return p.apply(ctx) })
	return p.stats, err
}

type rebuildClosurePass struct {
	b        *BuildStore
	spillAt  int
	buffered int
	changed  bool
	stats    RebuildClosureCollectionStats
}

func rebuildClosureKey(col byte, fields ...string) []byte {
	return keyStrings([]byte{'g', 'U', col}, fields...)
}

func (p *rebuildClosurePass) put(key, value []byte) error {
	if len(key)+len(value) > 16<<20 {
		return fmt.Errorf("%w: closure collection scratch row", ErrRebuildLinkInputSize)
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

func (p *rebuildClosurePass) pair(ctx context.Context, maxFanout int,
	sameBoundary func(*graph.Node, *graph.Node) bool, makeEdge func(*graph.Node, *graph.Node, string) *graph.Edge,
) error {
	view := p.b.RebuildView()
	if err := view.visit(ctx, []byte{familyNode}, func(_ *pebble.Snapshot, r row) error {
		light, err := decodeNodeLight(r.value)
		if err != nil {
			return err
		}
		if !bytes.Equal(r.key, pendingNodeKey(light.ID)) {
			return invalid("closure collection node identity")
		}
		if light.Kind != graph.KindMethod && light.Kind != graph.KindFunction {
			return nil
		}
		if len(r.key)+len(r.value) > 16<<20 {
			return fmt.Errorf("%w: closure collection input row", ErrRebuildLinkInputSize)
		}
		n, err := decodeNode(r.value)
		if err != nil {
			return err
		}
		dispatch, appendField := closureFields(n.Meta)
		if dispatch != "" {
			if err := p.put(rebuildClosureKey('f', dispatch), nil); err != nil {
				return err
			}
			if err := p.put(rebuildClosureKey('d', dispatch, n.ID), nil); err != nil {
				return err
			}
		}
		if appendField != "" {
			return p.put(rebuildClosureKey('r', appendField, n.ID), nil)
		}
		return nil
	}); err != nil {
		return err
	}
	// A callable occupies one field per role, so each ordered pair has a
	// unique field and payload. No receipt ordinal elects a winner here.
	return pendingVisit(ctx, p.b.pending, rebuildClosureKey('f'), func(r row) error {
		field, _, err := readKeyString(r.key[3:])
		if err != nil {
			return err
		}
		for _, col := range [...]byte{'d', 'r'} {
			count := 0
			if err := pendingVisit(ctx, p.b.pending, rebuildClosureKey(col, field), func(row) error {
				count++
				return nil
			}); err != nil || count == 0 || count > maxFanout {
				return err
			}
		}
		return p.visitRole(ctx, 'd', field, func(d *graph.Node) error {
			return p.visitRole(ctx, 'r', field, func(r *graph.Node) error {
				if d.ID == r.ID || !sameBoundary(d, r) {
					return nil
				}
				return p.propose(makeEdge(d, r, field))
			})
		})
	})
}

func closureFields(meta map[string]any) (dispatch, appendField string) {
	dispatch, _ = meta["cc_dispatch_field"].(string)
	appendField, _ = meta["cc_append_field"].(string)
	return dispatch, appendField
}

func (p *rebuildClosurePass) visitRole(ctx context.Context, col byte, field string, yield func(*graph.Node) error) error {
	prefix := rebuildClosureKey(col, field)
	return pendingVisit(ctx, p.b.pending, prefix, func(r row) error {
		id, tail, err := readKeyString(r.key[len(prefix):])
		if err != nil {
			return err
		}
		if len(tail) != 0 {
			return invalid("closure collection role key")
		}
		n, err := p.b.RebuildView().Node(ctx, id)
		if err != nil {
			return err
		}
		if n == nil {
			return invalid("closure collection lost a role")
		}
		return yield(n)
	})
}

func (p *rebuildClosurePass) propose(e *graph.Edge) error {
	key := pendingEdgeKey(e)
	prior, err := p.b.pendingEdge(key)
	if err != nil || prior != nil {
		return err
	}
	proposal := append(rebuildClosureKey('e'), key...)
	exists, err := pendingGet(p.b.pending, proposal, func([]byte) (bool, error) { return true, nil })
	if err != nil || exists {
		// Repeated complete keys have identical payload.
		return err
	}
	raw, err := encodeEdge(e)
	if err != nil {
		return err
	}
	if err := p.put(rebuildClosureKey('n', e.From), []byte{rebuildCapabilityWanted}); err != nil {
		return err
	}
	if err := p.put(rebuildClosureKey('n', e.To), []byte{rebuildCapabilityWanted}); err != nil {
		return err
	}
	p.changed = true
	return p.put(proposal, raw)
}

func (p *rebuildClosurePass) state(key []byte) (byte, error) {
	return pendingGet(p.b.pending, key, func(value []byte) (byte, error) {
		if len(value) != 1 {
			return 0, invalid("closure collection proof state")
		}
		return value[0], nil
	})
}

func (p *rebuildClosurePass) prove(ctx context.Context, receipts iter.Seq2[ParseFacts, error]) error {
	for facts, err := range receipts {
		if err != nil {
			return err
		}
		file := pathkey.Normalize(facts.File.FilePath)
		for _, n := range facts.Nodes {
			if err := ctx.Err(); err != nil {
				return err
			}
			key := rebuildClosureKey('n', n.ID)
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
			bit := byte(rebuildCapabilityUnproved)
			if facts.EffectiveLanguage == n.Language && n.FilePath == file && sameRebuildClosureInput(n, live) {
				bit = rebuildCapabilityProved
			}
			if state&bit == 0 {
				if err := p.put(key, []byte{state | bit}); err != nil {
					return err
				}
			}
		}
	}
	var refused RebuildUnknownProvenanceError
	if err := p.visit(ctx, func(e *graph.Edge) error {
		for _, id := range [...]string{e.From, e.To} {
			state, err := p.state(rebuildClosureKey('n', id))
			if err != nil {
				return err
			}
			if state != rebuildCapabilityWanted|rebuildCapabilityProved {
				if refused.Count == 0 {
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

func sameRebuildClosureInput(raw, live *graph.Node) bool {
	if live == nil || live.ID != raw.ID || live.Kind != raw.Kind || live.Language != raw.Language ||
		live.Name != raw.Name || live.FilePath != raw.FilePath || live.StartLine != raw.StartLine || live.RepoPrefix != raw.RepoPrefix {
		return false
	}
	rawDispatch, rawAppend := closureFields(raw.Meta)
	liveDispatch, liveAppend := closureFields(live.Meta)
	return rawDispatch == liveDispatch && rawAppend == liveAppend
}

func (p *rebuildClosurePass) visit(ctx context.Context, yield func(*graph.Edge) error) error {
	prefix := rebuildClosureKey('e')
	return pendingVisit(ctx, p.b.pending, prefix, func(r row) error {
		e, err := decodeEdge(r.value)
		if err != nil {
			return err
		}
		if !bytes.Equal(r.key[len(prefix):], pendingEdgeKey(e)) {
			return invalid("closure collection proposal key")
		}
		return yield(e)
	})
}

func (p *rebuildClosurePass) apply(ctx context.Context) error {
	return p.visit(ctx, func(e *graph.Edge) error {
		if err := p.b.putEdgeWithCustody(e, rebuildEdgeCustody{proof: rebuildProofClosureCollection}); err != nil {
			return err
		}
		p.stats.Edges++
		return nil
	})
}
