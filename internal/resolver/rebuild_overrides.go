package resolver

import (
	"context"
	"iter"

	"github.com/zzet/gortex/internal/graph/store_pebble"
)

// RunRebuildOverrides follows the checked cold implements pass without
// changing the public resolver or inferOverrides ordering.
func RunRebuildOverrides(ctx context.Context, build *store_pebble.BuildStore, receipts iter.Seq2[store_pebble.ParseFacts, error]) error {
	return build.RebuildOverrides(ctx, receipts, overrideOrigin, methodOverrideEdge, methodSetImplementationEdge)
}
