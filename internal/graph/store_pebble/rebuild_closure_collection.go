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

// RebuildClosureCollectionStats reports installed edges.
// SpillBytes counts physical bytes of closed threshold SST runs consumed by
// the pass. It is zero because this pass uses only the pending overlay.
type RebuildClosureCollectionStats struct {
	Edges, SpillBytes uint64
}

// RebuildClosureCollection streams the native callable field join. Both
// raw role counts precede receipt proof and the workspace and self filters.
// Preserve SQLite durable keys when installing calls.
func (b *BuildStore) RebuildClosureCollection(ctx context.Context, receipts iter.Seq2[ParseFacts, error], spillAt int,
	maxFanout int, sameBoundary func(*graph.Node, *graph.Node) bool, makeEdge func(*graph.Node, *graph.Node, string) *graph.Edge,
	appleLanguage func(string) bool,
) (stats RebuildClosureCollectionStats, err error) {
	defer func() {
		if err != nil {
			stats = RebuildClosureCollectionStats{}
			err = fmt.Errorf("%w: %w", ErrRebuildClosureCollection, err)
		}
	}()
	if spillAt < 1 || spillAt > goldBufferBytes || receipts == nil || maxFanout < 1 || sameBoundary == nil || makeEdge == nil || appleLanguage == nil {
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
	p := rebuildClosurePass{b: b}
	if err := p.pair(ctx, maxFanout, sameBoundary, makeEdge); err != nil {
		return stats, err
	}
	if p.wants {
		if err := p.prove(ctx, receipts, appleLanguage); err != nil {
			return stats, err
		}
	}
	if !p.changed {
		return p.stats, nil
	}
	err = b.withWrites(func() error { return p.apply(ctx) })
	return p.stats, err
}

type rebuildClosurePass struct {
	b       *BuildStore
	changed bool
	wants   bool
	stats   RebuildClosureCollectionStats
}

func rebuildClosureKey(col byte, fields ...string) []byte {
	return keyStrings([]byte{'g', 'U', col}, fields...)
}

func (p *rebuildClosurePass) put(key, value []byte) error {
	return p.b.putRebuildScratch(key, value, 16<<20, "closure collection")
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
		roles := [...]byte{'d', 'r'}
		var counts [len(roles)]int
		for i, col := range roles {
			if err := p.visitRoleIDs(ctx, col, field, func(string) error {
				counts[i]++
				return nil
			}); err != nil {
				return err
			}
		}
		if counts[1] == 0 || counts[0] > maxFanout || counts[1] > maxFanout {
			return nil
		}
		for _, col := range roles {
			if err := p.visitRoleIDs(ctx, col, field, p.wantNode); err != nil {
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

func (p *rebuildClosurePass) visitRoleIDs(ctx context.Context, col byte, field string, yield func(string) error) error {
	prefix := rebuildClosureKey(col, field)
	return pendingVisit(ctx, p.b.pending, prefix, func(r row) error {
		id, tail, err := readKeyString(r.key[len(prefix):])
		if err != nil {
			return err
		}
		if len(tail) != 0 {
			return invalid("closure collection role key")
		}
		return yield(id)
	})
}

func (p *rebuildClosurePass) visitRole(ctx context.Context, col byte, field string, yield func(*graph.Node) error) error {
	return p.visitRoleIDs(ctx, col, field, func(id string) error {
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
	// Both proposal endpoints were wanted after the role counts.
	p.changed = true
	return p.put(proposal, raw)
}

func (p *rebuildClosurePass) wantNode(id string) error {
	p.wants = true
	return p.put(rebuildClosureKey('n', id), []byte{rebuildCapabilityWanted})
}

func (p *rebuildClosurePass) state(key []byte) (byte, error) {
	return rebuildScratchState(p.b.pending, key, "closure collection proof state")
}

func (p *rebuildClosurePass) prove(ctx context.Context, receipts iter.Seq2[ParseFacts, error], appleLanguage func(string) bool) error {
	// Turns before Closure may stamp node metadata but keep identity,
	// language and file ownership. Reuse the frozen gate's family rule
	// on those rows, including non-callables.
	if p.changed {
		err := p.b.RebuildView().VisitFrameworkGateNodes(ctx, func(n *graph.Node) error {
			if !appleLanguage(n.Language) {
				return nil
			}
			return p.put(rebuildClosureKey('a', n.ID), []byte{rebuildCapabilityWanted})
		})
		if err != nil {
			return err
		}
	}
	for facts, err := range receipts {
		if err != nil {
			return err
		}
		file := pathkey.Normalize(facts.File.FilePath)
		for _, n := range facts.Nodes {
			if err := ctx.Err(); err != nil {
				return err
			}
			if p.changed {
				if err := p.noteApple(n, file); err != nil {
					return err
				}
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
			proved := facts.EffectiveLanguage == n.Language && n.FilePath == file && sameRebuildClosureInput(n, live)
			if err := noteRebuildScratchState(p.put, key, state, rebuildScratchProofBit(proved)); err != nil {
				return err
			}
		}
	}
	appleProved := false
	if p.changed {
		if err := pendingVisit(ctx, p.b.pending, rebuildClosureKey('a'), func(r row) error {
			if len(r.value) != 1 {
				return invalid("closure collection Apple proof state")
			}
			appleProved = appleProved || r.value[0] == rebuildScratchExact
			return nil
		}); err != nil {
			return err
		}
	}
	if err := proveRebuildScratchEndpoints(ctx, p.visit, func(id string) (bool, error) {
		if !appleProved {
			return false, nil
		}
		state, err := p.state(rebuildClosureKey('n', id))
		return state == rebuildScratchExact, err
	}, nil); err != nil {
		return err
	}
	// An eligible declaration may have no proposed call to name, for example
	// when the pair crosses a workspace boundary.
	prefix := rebuildClosureKey('n')
	var refused rebuildScratchRefusals
	if err := pendingVisit(ctx, p.b.pending, prefix, func(r row) error {
		if len(r.value) != 1 {
			return invalid("closure collection proof state")
		}
		if r.value[0] == rebuildScratchExact {
			return nil
		}
		id, _, err := readKeyString(r.key[len(prefix):])
		if err != nil {
			return err
		}
		n, err := p.b.RebuildView().Node(ctx, id)
		if err != nil {
			return err
		}
		if n == nil {
			return invalid("closure collection lost a role")
		}
		refused.add(graph.EdgeIdentity{From: n.ID, FilePath: n.FilePath}, nil)
		return nil
	}); err != nil {
		return err
	}
	return refused.err()
}

func (p *rebuildClosurePass) noteApple(raw *graph.Node, file string) error {
	key := rebuildClosureKey('a', raw.ID)
	state, err := p.state(key)
	if err != nil || state == 0 {
		return err
	}
	live, err := pendingGet(p.b.pending, pendingNodeKey(raw.ID), rebuildFrameworkGateNode)
	if err != nil {
		return err
	}
	// The gate reads Language, not the marker's role, Kind, or Name.
	proved := live != nil && live.ID == raw.ID && raw.FilePath == file && live.FilePath == file && live.Language == raw.Language
	return noteRebuildScratchState(p.put, key, state, rebuildScratchProofBit(proved))
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
	return visitRebuildScratchKeyedEdges(ctx, p.b.pending, rebuildClosureKey('e'), 0, "closure collection proposal key", yield)
}

func (p *rebuildClosurePass) apply(ctx context.Context) error {
	return applyRebuildScratchEdges(ctx, p.visit, p.b, rebuildProofClosureCollection, &p.stats.Edges)
}
