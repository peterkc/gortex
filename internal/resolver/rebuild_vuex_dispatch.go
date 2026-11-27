package resolver

import (
	"context"
	"iter"

	"github.com/zzet/gortex/internal/graph/store_pebble"
)

// RunRebuildVuexDispatch executes framework position twenty-one after RTK Query.
func RunRebuildVuexDispatch(ctx context.Context, b *store_pebble.BuildStore, receipts iter.Seq2[store_pebble.ParseFacts, error],
	selection FrameworkSynthesizerSelection,
) (store_pebble.RebuildVuexDispatchStats, error) {
	if !selection.allows(SynthVuexDispatch) {
		return store_pebble.RebuildVuexDispatchStats{}, nil
	}
	return b.RebuildVuexDispatch(ctx, receipts, store_pebble.RebuildVuexDispatchRules{
		Action: vuexDispatchNode,
		Stub:   vuexDispatchCall,
		Getter: isStoreFactoryGetter,
		Rebind: rebindVuexDispatchCall,
	})
}
