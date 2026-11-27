package resolver

import (
	"context"
	"iter"

	"github.com/zzet/gortex/internal/graph/store_pebble"
)

// RunRebuildObjectRegistry executes framework position nineteen.
func RunRebuildObjectRegistry(ctx context.Context, b *store_pebble.BuildStore, receipts iter.Seq2[store_pebble.ParseFacts, error],
	selection FrameworkSynthesizerSelection,
) (store_pebble.RebuildObjectRegistryStats, error) {
	if !selection.allows(SynthObjectRegistry) {
		return store_pebble.RebuildObjectRegistryStats{}, nil
	}
	return b.RebuildObjectRegistry(ctx, receipts, store_pebble.RebuildObjectRegistryRules{
		Call:        objectRegistryCall,
		ClassName:   registryClassName,
		MethodOrder: objectRegistryMethodOrder,
		Getter:      isStoreFactoryGetter,
		Rebind:      rebindObjectRegistryCall,
	})
}
