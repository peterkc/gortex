package resolver

import (
	"context"
	"iter"

	"github.com/zzet/gortex/internal/graph/store_pebble"
)

// RunRebuildMyBatis executes framework position fourteen after Fabric.
func RunRebuildMyBatis(ctx context.Context, b *store_pebble.BuildStore, receipts iter.Seq2[store_pebble.ParseFacts, error],
	selection FrameworkSynthesizerSelection,
) (store_pebble.RebuildMyBatisStats, error) {
	if !selection.allows(SynthMyBatis) {
		return store_pebble.RebuildMyBatisStats{}, nil
	}
	return b.RebuildMyBatis(ctx, receipts, javaMethodJoinKey, myBatisStub, myBatisJoinKey, rebindMyBatisCall)
}
