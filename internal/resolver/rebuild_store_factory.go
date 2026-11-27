package resolver

import (
	"context"
	"iter"

	"github.com/zzet/gortex/internal/graph/store_pebble"
)

// RunRebuildStoreFactory executes framework position sixteen after SQL callsites.
func RunRebuildStoreFactory(ctx context.Context, b *store_pebble.BuildStore, receipts iter.Seq2[store_pebble.ParseFacts, error],
	selection FrameworkSynthesizerSelection,
) (store_pebble.RebuildStoreFactoryStats, error) {
	if !selection.allows(SynthStoreFactory) {
		return store_pebble.RebuildStoreFactoryStats{}, nil
	}
	return b.RebuildStoreFactory(ctx, receipts, store_pebble.RebuildStoreFactoryRules{
		Action: storeFactoryAction,
		Stub:   storeFactoryCall,
		Getter: isStoreFactoryGetter,
		Rebind: rebindStoreFactoryCall,
	})
}
