package resolver

import (
	"context"
	"iter"

	"github.com/zzet/gortex/internal/graph/store_pebble"
)

// RunRebuildRTKQuery executes framework position twenty after Object Registry.
func RunRebuildRTKQuery(ctx context.Context, b *store_pebble.BuildStore, receipts iter.Seq2[store_pebble.ParseFacts, error],
	selection FrameworkSynthesizerSelection,
) (store_pebble.RebuildRTKQueryStats, error) {
	if !selection.allows(SynthRTKQuery) {
		return store_pebble.RebuildRTKQueryStats{}, nil
	}
	return b.RebuildRTKQuery(ctx, receipts, store_pebble.RebuildRTKQueryRules{
		Node:   rtkQueryNode,
		Call:   rtkQueryCall,
		Getter: isStoreFactoryGetter,
		Rebind: rebindRTKQueryCall,
	})
}
