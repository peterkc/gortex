package resolver

import (
	"context"
	"iter"

	"github.com/zzet/gortex/internal/graph/store_pebble"
)

// RunRebuildReactSetState executes framework position nine.
func RunRebuildReactSetState(ctx context.Context, b *store_pebble.BuildStore, receipts iter.Seq2[store_pebble.ParseFacts, error],
	selection FrameworkSynthesizerSelection,
) (store_pebble.RebuildReactSetStateStats, error) {
	if !selection.allows(SynthReactSetState) {
		return store_pebble.RebuildReactSetStateStats{}, nil
	}
	return b.RebuildReactSetState(ctx, receipts, isSetStateTarget, reactSetStateEdge)
}
