package store_pebble

import (
	"bytes"
	"context"
	"fmt"

	"github.com/zzet/gortex/internal/graph"
)

func (b *BuildStore) withWrites(write func() error) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.first == nil {
		if err := write(); err != nil {
			b.first = fmt.Errorf("Pebble build mutation: %w", err)
		}
	}
	return b.first
}

func (b *BuildStore) AddNode(n *graph.Node) {
	b.withWrites(func() error { return b.writeNode(n) })
}

func (b *BuildStore) AddBatch(nodes []*graph.Node, edges []*graph.Edge) {
	b.withWrites(func() error {
		for _, n := range nodes {
			if err := b.writeNode(n); err != nil {
				return err
			}
		}
		for _, e := range edges {
			if err := b.addEdge(e); err != nil {
				return err
			}
		}
		return nil
	})
}

func (b *BuildStore) AddEdge(e *graph.Edge) {
	b.withWrites(func() error { return b.addEdge(e) })
}

func (b *BuildStore) addEdge(e *graph.Edge) error {
	if e == nil || graph.StructuralEdgeTargetInvalid(e.Kind, e.To) {
		return nil
	}
	old, err := b.pendingEdge(pendingEdgeKey(e))
	if err != nil {
		return err
	}
	if err := b.putEdge(e); err != nil {
		return err
	}
	if old != nil && old.Origin != e.Origin {
		return b.noteRevision()
	}
	return nil
}

var revisionKey = []byte{'V'}

func (b *BuildStore) noteRevision() error {
	return addPendingCount(b.pending, revisionKey, 1)
}

func (b *BuildStore) SetEdgeProvenance(e *graph.Edge, origin string) (changed bool) {
	b.withWrites(func() error {
		var err error
		changed, err = b.setEdgeProvenance(e, origin)
		return err
	})
	return changed
}

func (b *BuildStore) setEdgeProvenance(e *graph.Edge, origin string) (bool, error) {
	if e == nil {
		return false, nil
	}
	old, err := b.pendingEdge(pendingEdgeKey(e))
	if err != nil || old == nil {
		return false, err
	}
	if old.Origin == origin {
		return false, nil
	}
	old.Origin, old.Tier = origin, e.Tier
	if old.Tier != "" {
		old.Tier = graph.ResolvedBy(origin)
	}
	if err := b.putEdge(old); err != nil {
		return false, err
	}
	if err := b.noteRevision(); err != nil {
		return false, err
	}
	e.Origin, e.Tier = old.Origin, old.Tier
	return true, nil
}

func (b *BuildStore) SetEdgeProvenanceBatch(batch []graph.EdgeProvenanceUpdate) (changed int) {
	b.withWrites(func() error {
		for _, item := range batch {
			ok, err := b.setEdgeProvenance(item.Edge, item.NewOrigin)
			if err != nil {
				return err
			}
			if ok {
				changed++
			}
		}
		return nil
	})
	return changed
}

func (b *BuildStore) RemoveEdge(from, to string, kind graph.EdgeKind) (removed bool) {
	b.withWrites(func() error {
		prefix := keyStrings([]byte{familyEdge, edgeOut}, from, string(kind), to)
		return pendingVisit(context.Background(), b.pending, prefix, func(r row) error {
			e, err := decodeEdge(r.value)
			if err != nil {
				return err
			}
			if err := b.deleteEdge(r.key, e); err != nil {
				return err
			}
			removed = true
			return nil
		})
	})
	return removed
}

func (b *BuildStore) evictBy(prefix []byte) (nodes, edges int) {
	b.withWrites(func() error {
		var err error
		nodes, edges, err = b.evict(prefix)
		return err
	})
	return nodes, edges
}

func (b *BuildStore) EvictFile(path string) (int, int) {
	return b.evictBy(keyStrings([]byte{familyFile}, path))
}

func (b *BuildStore) EvictRepo(repo string) (int, int) {
	return b.evictBy(keyStrings([]byte{familyRepo}, repo))
}

func (b *BuildStore) ReindexEdge(e *graph.Edge, oldTo string) {
	if e == nil || e.To == oldTo {
		return
	}
	b.ReindexEdges([]graph.EdgeReindex{{Edge: e, OldTo: oldTo}})
}

func (b *BuildStore) ReindexEdges(batch []graph.EdgeReindex) {
	b.withWrites(func() error {
		// Store's convergence guarantee is per bounded chunk, as in SQLite.
		for i := 0; i < len(batch); i += 256 {
			if err := b.reindexChunk(batch[i:min(i+256, len(batch))]); err != nil {
				return err
			}
		}
		return nil
	})
}

func (b *BuildStore) reindexChunk(batch []graph.EdgeReindex) error {
	// First free every moving stored identity. A stationary destination
	// preserves its row, and the first claimant wins any free destination.
	var candidates []*graph.Edge
	created := make(map[string]int, len(batch))
	moved := make(map[string]bool, len(batch))
	for _, r := range batch {
		e := r.Edge
		if e == nil {
			continue
		}
		from := *e
		from.To = r.OldTo
		if r.OldKind != "" {
			from.Kind = r.OldKind
		}
		if r.RefreshIdentity {
			if r.OldFrom != "" {
				from.From = r.OldFrom
			}
			from.FilePath, from.Line = r.OldFilePath, r.OldLine
		}
		oldKey, key := pendingEdgeKey(&from), pendingEdgeKey(e)
		old, err := b.pendingEdge(oldKey)
		if err != nil {
			return err
		}
		if old == nil {
			if moved[string(oldKey)] {
				continue
			}
			if index, ok := created[string(oldKey)]; ok {
				candidates[index] = nil
				delete(created, string(oldKey))
			} else if r.RefreshIdentity {
				continue
			}
		} else if bytes.Equal(oldKey, key) {
			if err := b.putEdge(e); err != nil {
				return err
			}
			continue
		} else {
			if err := b.deleteEdge(oldKey, old); err != nil {
				return err
			}
			if r.RefreshIdentity {
				if err := b.noteRevision(); err != nil {
					return err
				}
			}
			moved[string(oldKey)] = true
		}
		if _, ok := created[string(key)]; !ok {
			created[string(key)] = len(candidates)
		}
		candidates = append(candidates, e)
	}
	for _, e := range candidates {
		if e == nil || graph.StructuralEdgeTargetInvalid(e.Kind, e.To) {
			continue
		}
		old, err := b.pendingEdge(pendingEdgeKey(e))
		if err != nil {
			return err
		}
		if old == nil {
			if err := b.putEdge(e); err != nil {
				return err
			}
		}
	}
	return nil
}

var (
	_ graph.EdgePersister          = (*BuildStore)(nil)
	_ graph.EdgeMetaBatchPersister = (*BuildStore)(nil)
)

func (b *BuildStore) PersistEdgeAttributes(e *graph.Edge) {
	b.PersistEdgeAttributesBatch([]*graph.Edge{e})
}

func (b *BuildStore) PersistEdgeAttributesBatch(edges []*graph.Edge) {
	b.withWrites(func() error {
		for _, e := range edges {
			if e == nil {
				continue
			}
			old, err := b.pendingEdge(pendingEdgeKey(e))
			if err != nil {
				return err
			}
			if old == nil {
				continue
			}
			old.Confidence, old.ConfidenceLabel = e.Confidence, e.ConfidenceLabel
			old.Origin, old.Tier, old.Meta = e.Origin, e.Tier, e.Meta
			if err := b.putEdge(old); err != nil {
				return err
			}
		}
		return nil
	})
}
