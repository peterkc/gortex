package indexer

import (
	"context"

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
			return yieldRebuildClonePairEdges(ctx, view, graph.EdgeSemanticallyRelated, pair, yield)
		})
}
