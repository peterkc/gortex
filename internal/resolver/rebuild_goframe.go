package resolver

import (
	"context"
	"iter"

	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/graph/store_pebble"
)

// RunRebuildGoFrame reconciles live GoFrame route Calls after Vapor.
func RunRebuildGoFrame(ctx context.Context, build *store_pebble.BuildStore, receipts iter.Seq2[store_pebble.ParseFacts, error],
	selection FrameworkSynthesizerSelection,
) (uint64, error) {
	if !selection.allows(SynthGoFrameRoute) {
		return 0, nil
	}
	return build.RebuildGoFrame(ctx, receipts, store_pebble.RebuildGoFrameRules{
		Route:   goframeRouteShape,
		Handler: goframeMethodShape,
		Dir:     goframeDir,
		Rebind: func(e *graph.Edge, req string, target *graph.Node) (graph.EdgeReindex, *graph.Edge) {
			return goframeRouteRebind(cloneFrameworkEdge(e), req, target)
		},
	})
}
