package indexer

import (
	"context"
	"fmt"

	"github.com/zzet/gortex/internal/clones"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/graph/store_pebble"
)

// visitRebuildDiffusedCloneEdges keeps the full checked direct pair stream
// through ranking. The source must supply materializable pairs on a quiet view.
// The emitted semantic edges are tentative until the visitor returns nil.
func visitRebuildDiffusedCloneEdges(ctx context.Context, build *store_pebble.BuildStore,
	visitDirect func(func(clones.Pair) error) error, yield func(*graph.Edge) error,
) (pairs int, capped bool, err error) {
	view := build.RebuildView()
	return build.VisitRebuildDiffusedClonePairs(ctx, visitDirect, diffusionMaxNeighbors, diffusionMaxPairs,
		func(left, right float64) float64 {
			score := diffusionDamping * left * right
			if score < diffusionThreshold {
				return 0
			}
			return score
		}, func(pair clones.Pair) error {
			a, err := view.Node(ctx, pair.A)
			if err != nil {
				return err
			}
			b, err := view.Node(ctx, pair.B)
			if err != nil {
				return err
			}
			if a == nil || b == nil {
				return fmt.Errorf("%w: missing checked clone endpoint in pair %q, %q", store_pebble.ErrRebuildView, pair.A, pair.B)
			}
			if err := yield(cloneRelationEdge(graph.EdgeSemanticallyRelated, a, b, pair.Similarity)); err != nil {
				return err
			}
			return yield(cloneRelationEdge(graph.EdgeSemanticallyRelated, b, a, pair.Similarity))
		})
}
