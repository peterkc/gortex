package resolver

import (
	"context"
	"iter"

	"github.com/zzet/gortex/internal/graph/store_pebble"
)

// RunRebuildKMPExpectActual executes framework position eleven after Flutter.
func RunRebuildKMPExpectActual(ctx context.Context, b *store_pebble.BuildStore, receipts iter.Seq2[store_pebble.ParseFacts, error],
	selection FrameworkSynthesizerSelection, spillAt int,
) (store_pebble.RebuildKMPExpectActualStats, error) {
	if !selection.allows(SynthKMPExpectActual) {
		return store_pebble.RebuildKMPExpectActualStats{}, nil
	}
	return b.RebuildKMPExpectActual(ctx, receipts, spillAt, kmpEdge)
}
