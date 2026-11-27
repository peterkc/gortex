package resolver

import (
	"context"
	"iter"

	"github.com/zzet/gortex/internal/graph/store_pebble"
)

// RunRebuildMediatR executes framework position twenty-four after Spring events.
func RunRebuildMediatR(ctx context.Context, b *store_pebble.BuildStore, receipts iter.Seq2[store_pebble.ParseFacts, error],
	selection FrameworkSynthesizerSelection,
) (store_pebble.RebuildMediatRStats, error) {
	if !selection.allows(SynthMediatR) {
		return store_pebble.RebuildMediatRStats{}, nil
	}
	return b.RebuildMediatR(ctx, receipts, store_pebble.RebuildMediatRRules{
		Handler: mediatrHandlerKey,
		Call:    mediatrCallKey,
		Getter:  isStoreFactoryGetter,
		Rebind:  rebindMediatRCall,
		Fanout:  mediatrFanoutEdge,
	})
}
