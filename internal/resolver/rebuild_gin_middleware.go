package resolver

import (
	"context"
	"iter"

	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/graph/store_pebble"
)

// RunRebuildGinMiddleware runs the Gin turn after C/C++ macro expansion.
func RunRebuildGinMiddleware(ctx context.Context, build *store_pebble.BuildStore, receipts iter.Seq2[store_pebble.ParseFacts, error],
	selection FrameworkSynthesizerSelection,
) (uint64, error) {
	if !selection.allows(SynthGinMiddleware) {
		return 0, nil
	}
	names := func(n *graph.Node) []string {
		return ginHandlerNames(n.Meta["gin_handlers"])
	}
	return build.RebuildGinMiddleware(ctx, receipts, store_pebble.RebuildGinRules{
		Dispatcher: ginMiddlewareDispatcher,
		Names:      names,
		Target:     ginMiddlewareTarget,
		Edge:       ginMiddlewareEdge,
		Threshold:  ginFanoutCap,
	})
}
