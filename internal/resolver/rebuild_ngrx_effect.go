package resolver

import (
	"context"
	"iter"

	"github.com/zzet/gortex/internal/graph/store_pebble"
)

// RunRebuildNgRxEffect executes framework position eighteen after Redux Thunk.
func RunRebuildNgRxEffect(ctx context.Context, b *store_pebble.BuildStore, receipts iter.Seq2[store_pebble.ParseFacts, error],
	selection FrameworkSynthesizerSelection,
) (store_pebble.RebuildNgRxEffectStats, error) {
	if !selection.allows(SynthNgRxEffect) {
		return store_pebble.RebuildNgRxEffectStats{}, nil
	}
	return b.RebuildNgRxEffect(ctx, receipts, store_pebble.RebuildNgRxEffectRules{
		Stub:   ngRxEffectAction,
		Getter: isStoreFactoryGetter,
		Rebind: rebindNgRxEffectCall,
	})
}
