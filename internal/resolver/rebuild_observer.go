package resolver

import (
	"context"
	"iter"

	"github.com/zzet/gortex/internal/graph/store_pebble"
)

// RunRebuildObserver executes framework position seven after native pairing.
func RunRebuildObserver(ctx context.Context, b *store_pebble.BuildStore, receipts iter.Seq2[store_pebble.ParseFacts, error],
	selection FrameworkSynthesizerSelection,
) (store_pebble.RebuildObserverStats, error) {
	if !selection.allows(SynthObserverChannel) {
		return store_pebble.RebuildObserverStats{}, nil
	}
	return b.RebuildObserver(ctx, receipts, observerRegistrarRe.MatchString, observerDispatcherRe.MatchString,
		sameDispatchBoundary, maxObserverCallbacksPerChannel, observerChannelEdge)
}
