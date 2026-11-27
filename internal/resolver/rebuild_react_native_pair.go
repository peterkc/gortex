package resolver

import (
	"context"
	"iter"

	"github.com/zzet/gortex/internal/graph/store_pebble"
)

// RunRebuildReactNativeNativePairing executes framework position six after
// the React Native call bridge. Both directions remain visible to Observer.
func RunRebuildReactNativeNativePairing(ctx context.Context, b *store_pebble.BuildStore, receipts iter.Seq2[store_pebble.ParseFacts, error],
	selection FrameworkSynthesizerSelection, spillAt int,
) (store_pebble.RebuildReactNativeNativePairingStats, error) {
	if !selection.allows(SynthReactNativePair) {
		return store_pebble.RebuildReactNativeNativePairingStats{}, nil
	}
	return b.RebuildReactNativeNativePairing(ctx, receipts, spillAt, rnPlatform, rnNativePairEdge)
}
