package resolver

import (
	"context"
	"iter"

	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/graph/store_pebble"
)

// RunRebuildGRPC executes only registry position one. The private build must
// have completed all preceding passes, including clone detection.
func RunRebuildGRPC(ctx context.Context, b *store_pebble.BuildStore, receipts iter.Seq2[store_pebble.ParseFacts, error],
	selection FrameworkSynthesizerSelection,
) (store_pebble.RebuildGRPCStats, error) {
	if !selection.allows(SynthGRPCStub) {
		return store_pebble.RebuildGRPCStats{}, nil
	}
	return b.RebuildGRPC(ctx, receipts, func(e *graph.Edge, service, method, handler, origin string) bool {
		confidence := 0.7
		if origin == graph.OriginASTResolved {
			confidence = 0.9
		}
		want := handler
		if want == "" {
			want = grpcStubPlaceholder(service, method)
		}
		if e.To == want {
			return false
		}
		var resolved int
		rebindGRPCStub(e, want, handler, origin, confidence, &resolved)
		return true
	})
}
