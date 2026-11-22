package resolver

import (
	"context"
	"iter"

	"github.com/zzet/gortex/internal/graph/store_pebble"
)

// RunRebuildImplements is the private cold method-set pass. The native
// requirement decoder and edge constructor keep the saved payload identical.
func RunRebuildImplements(ctx context.Context, build *store_pebble.BuildStore, receipts iter.Seq2[store_pebble.ParseFacts, error]) error {
	return build.RebuildImplements(ctx, receipts, interfaceMethodNames, methodSetImplementationEdge)
}
