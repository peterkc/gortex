package store_pebble

import (
	"bytes"
	"context"
	"fmt"

	"github.com/zzet/gortex/internal/graph"
)

// withWrites records the first mutation error so Publish can return it.
func (b *BuildStore) withWrites(write func() error) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.first == nil {
		if err := write(); err != nil {
			b.first = fmt.Errorf("pebble build mutation: %w", err)
		}
	}
	return b.first
}

func (b *BuildStore) AddNode(n *graph.Node) {
	// withWrites saves the first mutation error for Publish.
	_ = b.withWrites(func() error { return b.writeNode(n) })
}

func (b *BuildStore) AddBatch(nodes []*graph.Node, edges []*graph.Edge) {
	// withWrites saves the first mutation error for Publish.
	_ = b.withWrites(func() error {
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
	// withWrites saves the first mutation error for Publish.
	_ = b.withWrites(func() error { return b.addEdge(e) })
}

func (b *BuildStore) addEdge(e *graph.Edge) error {
	return b.addEdgeWithCustody(e, rebuildEdgeCustody{})
}

func (b *BuildStore) addEdgeWithCustody(e *graph.Edge, custody rebuildEdgeCustody) error {
	if e == nil || graph.StructuralEdgeTargetInvalid(e.Kind, e.To) {
		return nil
	}
	old, err := b.pendingEdge(pendingEdgeKey(e))
	if err != nil {
		return err
	}
	if err := b.writePendingEdge(e, custody, old != nil); err != nil {
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
	_ = b.withWrites(func() error {
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
	if err := b.refreshPendingEdge(old); err != nil {
		return false, err
	}
	if err := b.noteRevision(); err != nil {
		return false, err
	}
	e.Origin, e.Tier = old.Origin, old.Tier
	return true, nil
}

func (b *BuildStore) SetEdgeProvenanceBatch(batch []graph.EdgeProvenanceUpdate) (changed int) {
	_ = b.withWrites(func() error {
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
	_ = b.withWrites(func() error {
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

// RemoveEdgesExact deletes only the requested full identities. The existing
// pending point read supplies the saved row for its paired reverse deletion;
// nil, absent and repeated identities are no-ops. Mutation errors are latched
// by withWrites and surfaced by the checked build readers and stage seals.
func (b *BuildStore) RemoveEdgesExact(edges []*graph.Edge) (removed int) {
	_ = b.withWrites(func() error {
		for _, e := range edges {
			if e == nil {
				continue
			}
			key := pendingEdgeKey(e)
			stored, err := b.pendingEdge(key)
			if err != nil {
				return err
			}
			if stored == nil {
				continue
			}
			if !bytes.Equal(key, pendingEdgeKey(stored)) {
				return invalid("working removal edge identity does not match key")
			}
			if err := b.deleteEdge(key, stored); err != nil {
				return err
			}
			removed++
		}
		return nil
	})
	return removed
}

func (b *BuildStore) evictBy(prefix []byte) (nodes, edges int) {
	_ = b.withWrites(func() error {
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
	_ = b.withWrites(func() error {
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
	return b.reindexChunkWithRecorders(batch, false)
}

// Checked target or kind reindexes (ApplyLinkReindexes) capture an existing
// old key as raw receipt lineage; dataflow identity refreshes (ArgOf,
// ReturnsTo) do not.
// Generic reindexes, including placeholder source moves and function-pointer
// moves, do not capture an old key. They keep inherited raw lineage or mark
// a receipt-input move as untracked. Go builtin and Go dataflow source proofs
// can survive target-only moves. A missing old row cannot establish raw lineage.
func (b *BuildStore) reindexChunkWithRecorders(batch []graph.EdgeReindex, capture bool) error {
	return b.reindexChunkWithProof(batch, capture, false)
}

func (b *BuildStore) reindexChunkWithProof(batch []graph.EdgeReindex, capture, goBuiltinRepoint bool) error {
	// First free every moving stored identity. A stationary destination
	// preserves its row, and the first claimant wins any free destination.
	type claim struct {
		edge    *graph.Edge
		custody rebuildEdgeCustody
	}
	var candidates []claim
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
		var custody rebuildEdgeCustody
		old, err := b.pendingEdge(oldKey)
		if err != nil {
			return err
		}
		if old == nil {
			if moved[string(oldKey)] {
				continue
			}
			if index, ok := created[string(oldKey)]; ok {
				prior := candidates[index]
				custody = prior.custody
				if custody.proof == rebuildProofGoBuiltinSource || custody.proof == rebuildProofGoDataflowSource {
					custody = custody.reindexed(nil, prior.edge, e, false, false)
				}
				candidates[index] = claim{}
				delete(created, string(oldKey))
			} else if r.RefreshIdentity {
				continue
			} else {
				custody = custody.moved(nil, e.Kind, false)
			}
		} else if bytes.Equal(oldKey, key) {
			if err := b.refreshPendingEdge(e); err != nil {
				return err
			}
			continue
		} else {
			custody, err = readRebuildEdgeCustody(b.pending, oldKey)
			if err != nil {
				return err
			}
			custody = custody.reindexed(oldKey, old, e, capture, goBuiltinRepoint)
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
		candidates = append(candidates, claim{edge: e, custody: custody})
	}
	for _, c := range candidates {
		e := c.edge
		if e == nil || graph.StructuralEdgeTargetInvalid(e.Kind, e.To) {
			continue
		}
		old, err := b.pendingEdge(pendingEdgeKey(e))
		if err != nil {
			return err
		}
		if old == nil {
			if err := b.putEdgeWithCustody(e, c.custody); err != nil {
				return err
			}
		}
	}
	return nil
}

var (
	_ graph.EdgePersister          = (*BuildStore)(nil)
	_ graph.EdgeMetaBatchPersister = (*BuildStore)(nil)
	_ graph.ExactEdgeBatchRemover  = (*BuildStore)(nil)
)

func (b *BuildStore) PersistEdgeAttributes(e *graph.Edge) {
	b.PersistEdgeAttributesBatch([]*graph.Edge{e})
}

func (b *BuildStore) PersistEdgeAttributesBatch(edges []*graph.Edge) {
	_ = b.withWrites(func() error {
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
			if err := b.refreshPendingEdge(old); err != nil {
				return err
			}
		}
		return nil
	})
}
