package indexer

import (
	"context"
	"fmt"
	"iter"
	"strings"

	"github.com/zzet/gortex/internal/contracts"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/graph/store_pebble"
)

// shapeBatches is the shape portion of the late contract batch producer. The
// caller supplies selected, strictly ordered full type references from the
// finished records and reads these facts at the native shape cut. Each batch
// holds one shape; neither receipts nor linked nodes accumulate across IDs.
func (r rebuildContractReceiptReader) shapeBatches(ctx context.Context, view store_pebble.RebuildView,
	ids iter.Seq2[string, error],
) iter.Seq2[store_pebble.RebuildContractBatch, error] {
	return func(yield func(store_pebble.RebuildContractBatch, error) bool) {
		if ids == nil {
			yield(store_pebble.RebuildContractBatch{}, fmt.Errorf("%w: no contract shape reference stream", graph.ErrContractProjectionStale))
			return
		}
		var previous string
		for id, err := range ids {
			if err != nil {
				yield(store_pebble.RebuildContractBatch{}, err)
				return
			}
			if !strings.Contains(id, "::") || id <= previous {
				yield(store_pebble.RebuildContractBatch{}, fmt.Errorf("%w: unordered or invalid contract shape reference %q", graph.ErrContractProjectionStale, id))
				return
			}
			previous = id
			shape, err := r.shapeAt(ctx, view, id)
			if err != nil {
				yield(store_pebble.RebuildContractBatch{}, err)
				return
			}
			if shape != nil && !yield(store_pebble.RebuildContractBatch{Shapes: map[string]*contracts.Shape{id: shape}}, nil) {
				return
			}
		}
		if err := ctx.Err(); err != nil {
			yield(store_pebble.RebuildContractBatch{}, err)
		}
	}
}

func (r rebuildContractReceiptReader) shapeAt(ctx context.Context, view store_pebble.RebuildView, id string) (*contracts.Shape, error) {
	n, err := view.Node(ctx, id)
	if err != nil || n == nil || (n.Kind != graph.KindType && n.Kind != graph.KindInterface) {
		return nil, err
	}
	if _, present := n.Meta["shape"]; present {
		return nil, fmt.Errorf("%w: contract shape already stored in ordinary node %q", graph.ErrContractProjectionStale, id)
	}
	facts, err := r.forLinkedNode(ctx, n)
	if err != nil || n.StartLine <= 0 {
		return nil, err
	}
	return facts.Shapes[n.ID], nil
}
