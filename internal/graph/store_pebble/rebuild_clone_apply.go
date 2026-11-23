package store_pebble

import (
	"context"
	"errors"
	"fmt"

	"github.com/cockroachdb/pebble/v2"
	"github.com/zzet/gortex/internal/graph"
)

// ApplyRebuildCloneRelations stages the checked clone producer's detached rows
// in the private build. The producer must propagate sink errors and leave the
// working graph unchanged. Apply starts only after the producer and its scratch
// cleanup succeed. Discard the private build on error during apply or cleanup.
func (b *BuildStore) ApplyRebuildCloneRelations(ctx context.Context,
	produce func(func(*graph.Node) error, func(*graph.Edge) error) error,
) (err error) {
	prefix := []byte{'g', 'C'}
	occupied, err := rebuildGuardHasPrefix(ctx, b.pending, prefix)
	if err != nil {
		return err
	}
	if occupied {
		return invalid("clone output scratch range occupied")
	}
	defer func() {
		err = errors.Join(err, b.pending.DeleteRange(prefix, prefixEnd(prefix), pebble.NoSync))
	}()
	nodes, edges := []byte{'g', 'C', 'n'}, []byte{'g', 'C', 'r'}
	put := func(key, value []byte) error {
		if len(key)+len(value) > 16<<20 {
			return fmt.Errorf("%w: clone output row", ErrRebuildLinkInputSize)
		}
		return b.pending.Set(key, value, pebble.NoSync)
	}
	if err := produce(func(n *graph.Node) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		if n == nil || n.ID == "" || (n.Kind != graph.KindFunction && n.Kind != graph.KindMethod) {
			return invalid("invalid clone node stamp")
		}
		value, err := encodeNode(n)
		if err != nil {
			return err
		}
		return put(keyStrings(nodes, n.ID), value)
	}, func(e *graph.Edge) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		if e == nil || e.From == "" || e.To == "" || e.From == e.To ||
			(e.Kind != graph.EdgeSimilarTo && e.Kind != graph.EdgeSemanticallyRelated) {
			return invalid("invalid clone relation")
		}
		value, err := encodeEdge(e)
		if err != nil {
			return err
		}
		return put(append(edges, pendingEdgeKey(e)...), value)
	}); err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	return b.withWrites(func() error {
		if err := pendingVisit(ctx, b.pending, nodes, func(r row) error {
			n, err := decodeNode(r.value)
			if err != nil {
				return err
			}
			return b.writeNode(n)
		}); err != nil {
			return err
		}
		return pendingVisit(ctx, b.pending, edges, func(r row) error {
			e, err := decodeEdge(r.value)
			if err != nil {
				return err
			}
			return b.addEdge(e)
		})
	})
}
