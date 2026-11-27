package resolver

import (
	"context"
	"iter"

	"github.com/zzet/gortex/internal/graph/store_pebble"
)

// RunRebuildFlutterSetState executes framework position ten after React.
func RunRebuildFlutterSetState(ctx context.Context, b *store_pebble.BuildStore, receipts iter.Seq2[store_pebble.ParseFacts, error],
	selection FrameworkSynthesizerSelection, spillAt int,
) (store_pebble.RebuildFlutterSetStateStats, error) {
	if !selection.allows(SynthFlutterSetState) {
		return store_pebble.RebuildFlutterSetStateStats{}, nil
	}
	return b.RebuildFlutterSetState(ctx, receipts, spillAt, isSetStateTarget, flutterSetStateEdge)
}
