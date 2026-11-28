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
// holds one shape and the lookup keeps the shape inputs for only the last file read.
func (r rebuildContractReceiptReader) shapeBatches(ctx context.Context, view store_pebble.RebuildView,
	ids iter.Seq2[string, error],
) iter.Seq2[store_pebble.RebuildContractBatch, error] {
	return func(yield func(store_pebble.RebuildContractBatch, error) bool) {
		if ids == nil {
			yield(store_pebble.RebuildContractBatch{}, fmt.Errorf("%w: no contract shape reference stream", graph.ErrContractProjectionStale))
			return
		}
		var previous string
		shapeAt := r.shapeLookup(ctx, view)
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
			shape, err := shapeAt(id)
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

// shapeLookup retains only the last file's shape answers and declarations.
// A new replay starts without cached evidence. The parse-store lease owns
// the checked receipt bytes for the duration of a replay.
func (r rebuildContractReceiptReader) shapeLookup(ctx context.Context, view store_pebble.RebuildView) func(id string) (*contracts.Shape, error) {
	r.linkView = view
	var path string
	var declarations map[string]*graph.Node
	var shapes map[string]*contracts.Shape
	return func(id string) (*contracts.Shape, error) {
		n, err := view.Node(ctx, id)
		if err != nil || n == nil || (n.Kind != graph.KindType && n.Kind != graph.KindInterface) {
			return nil, err
		}
		if _, present := n.Meta["shape"]; present {
			return nil, fmt.Errorf("%w: contract shape already stored in ordinary node %q", graph.ErrContractProjectionStale, id)
		}
		if n.StartLine <= 0 || contracts.ShapeExtractorForPath(n.FilePath) == nil {
			return nil, nil // native ExtractShape cannot select this type
		}
		if n.FilePath != path || n.RepoPrefix != "" {
			path, declarations, shapes = "", nil, nil
			base, facts, err := r.loadLinkedFile(ctx, n)
			if err != nil {
				return nil, err
			}
			declarations = make(map[string]*graph.Node, len(facts.Shapes))
			for _, saved := range base.Nodes {
				if _, wanted := facts.Shapes[saved.ID]; wanted {
					declarations[saved.ID] = saved // last upsert wins
				}
			}
			path, shapes = n.FilePath, facts.Shapes
		}
		if err := checkLinkedContractDeclaration(n, declarations[id]); err != nil {
			return nil, err
		}
		return shapes[id], nil
	}
}
