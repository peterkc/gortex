package resolver

import (
	"context"
	"iter"

	"github.com/zzet/gortex/internal/graph/store_pebble"
)

// RunRebuildClosureCollection runs framework position eight after Observer.
func RunRebuildClosureCollection(ctx context.Context, b *store_pebble.BuildStore, receipts iter.Seq2[store_pebble.ParseFacts, error],
	selection FrameworkSynthesizerSelection,
) (store_pebble.RebuildClosureCollectionStats, error) {
	if !selection.allows(SynthClosureCollection) {
		return store_pebble.RebuildClosureCollectionStats{}, nil
	}
	return b.RebuildClosureCollection(ctx, selection.rebuildEarlyContractIDs, receipts, ccFanoutCap, sameDispatchBoundary, closureCollectionEdge,
		func(language string) bool { return frameworkLanguageFamily(language) == "apple" })
}
