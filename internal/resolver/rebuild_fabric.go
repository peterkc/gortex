package resolver

import (
	"context"
	"iter"

	"github.com/zzet/gortex/internal/graph/store_pebble"
)

// RunRebuildFabric executes framework position thirteen after Expo.
func RunRebuildFabric(ctx context.Context, b *store_pebble.BuildStore, receipts iter.Seq2[store_pebble.ParseFacts, error],
	selection FrameworkSynthesizerSelection,
) (store_pebble.RebuildFabricStats, error) {
	if !selection.allows(SynthFabric) {
		return store_pebble.RebuildFabricStats{}, nil
	}
	return b.RebuildFabric(ctx, receipts, fabricComponentCandidate, fabricBridgeEdge)
}
