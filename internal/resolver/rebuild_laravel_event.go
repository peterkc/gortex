package resolver

import (
	"context"
	"iter"

	"github.com/zzet/gortex/internal/graph/store_pebble"
)

// RunRebuildLaravelEvent executes framework position twenty-nine after Sidekiq.
func RunRebuildLaravelEvent(ctx context.Context, b *store_pebble.BuildStore, receipts iter.Seq2[store_pebble.ParseFacts, error],
	selection FrameworkSynthesizerSelection,
) (store_pebble.RebuildLaravelEventStats, error) {
	if !selection.allows(SynthLaravelEvent) {
		return store_pebble.RebuildLaravelEventStats{}, nil
	}
	return b.RebuildLaravelEvent(ctx, receipts, store_pebble.RebuildLaravelEventRules{
		ListenMap:    laravelListenMap,
		ListenerType: laravelListenerType,
		Entries:      laravelListenEntries,
		SimpleName:   laravelSimpleName,
		Call:         laravelCallType,
		Rebind:       rebindLaravelEventCall,
		Fanout:       laravelFanoutEdge,
	})
}
