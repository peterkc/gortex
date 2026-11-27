package resolver

import (
	"context"
	"iter"

	"github.com/zzet/gortex/internal/graph/store_pebble"
)

// RunRebuildCelery executes framework position twenty-two after Vuex Dispatch.
func RunRebuildCelery(ctx context.Context, b *store_pebble.BuildStore, receipts iter.Seq2[store_pebble.ParseFacts, error],
	selection FrameworkSynthesizerSelection,
) (store_pebble.RebuildCeleryStats, error) {
	if !selection.allows(SynthCelery) {
		return store_pebble.RebuildCeleryStats{}, nil
	}
	return b.RebuildCelery(ctx, receipts, store_pebble.RebuildCeleryRules{
		Node: celeryTaskNode, Call: celeryTaskCall, Getter: isStoreFactoryGetter, Rebind: rebindCeleryCall,
		FanoutCap: celeryFanoutCap,
	})
}
