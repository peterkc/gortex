package resolver

import (
	"context"
	"iter"

	"github.com/zzet/gortex/internal/graph/store_pebble"
)

// RunRebuildSidekiq executes framework position twenty-eight.
func RunRebuildSidekiq(ctx context.Context, b *store_pebble.BuildStore, receipts iter.Seq2[store_pebble.ParseFacts, error],
	selection FrameworkSynthesizerSelection,
) (store_pebble.RebuildSidekiqStats, error) {
	if !selection.allows(SynthSidekiq) {
		return store_pebble.RebuildSidekiqStats{}, nil
	}
	return b.RebuildSidekiq(ctx, receipts, store_pebble.RebuildSidekiqRules{
		Worker: sidekiqWorkerName,
		Stub:   sidekiqCallName,
		Simple: sidekiqSimpleName,
		Getter: isStoreFactoryGetter,
		Rebind: rebindSidekiqCall,
	})
}
