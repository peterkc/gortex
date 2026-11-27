package resolver

import (
	"context"
	"iter"

	"github.com/zzet/gortex/internal/graph/store_pebble"
)

// RunRebuildReduxThunk executes framework position seventeen after Store Factory.
func RunRebuildReduxThunk(ctx context.Context, b *store_pebble.BuildStore, receipts iter.Seq2[store_pebble.ParseFacts, error],
	selection FrameworkSynthesizerSelection,
) (store_pebble.RebuildReduxThunkStats, error) {
	if !selection.allows(SynthReduxThunk) {
		return store_pebble.RebuildReduxThunkStats{}, nil
	}
	return b.RebuildReduxThunk(ctx, receipts, store_pebble.RebuildReduxThunkRules{
		Thunk:  reduxThunkName,
		Action: storeFactoryAction,
		Stub:   reduxThunkCallee,
		Getter: isStoreFactoryGetter,
		Rebind: rebindReduxThunkCall,
	})
}
