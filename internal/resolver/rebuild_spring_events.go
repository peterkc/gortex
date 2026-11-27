package resolver

import (
	"context"
	"iter"

	"github.com/zzet/gortex/internal/graph/store_pebble"
)

// RunRebuildSpringEvents executes framework position twenty-three after Celery.
func RunRebuildSpringEvents(ctx context.Context, b *store_pebble.BuildStore, receipts iter.Seq2[store_pebble.ParseFacts, error],
	selection FrameworkSynthesizerSelection,
) (store_pebble.RebuildSpringEventStats, error) {
	if !selection.allows(SynthSpringEvent) {
		return store_pebble.RebuildSpringEventStats{}, nil
	}
	return b.RebuildSpringEvents(ctx, receipts, store_pebble.RebuildSpringEventRules{
		Listener: springListenerType,
		Event:    springEventType,
		Simple:   springSimpleType,
		Rebind:   rebindSpringEvent,
		Fanout:   springEventFanout,
	})
}
