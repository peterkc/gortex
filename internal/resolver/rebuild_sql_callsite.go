package resolver

import (
	"context"
	"iter"

	"github.com/zzet/gortex/internal/graph/store_pebble"
)

// RunRebuildSQLCallsites executes framework position fifteen after MyBatis.
func RunRebuildSQLCallsites(ctx context.Context, b *store_pebble.BuildStore, receipts iter.Seq2[store_pebble.ParseFacts, error],
	selection FrameworkSynthesizerSelection,
) (store_pebble.RebuildSQLCallsiteStats, error) {
	if !selection.allows(SynthSQLCallsite) {
		return store_pebble.RebuildSQLCallsiteStats{}, nil
	}
	return b.RebuildSQLCallsites(ctx, receipts, sqlCallsiteFunction, rebindSQLCallsite)
}
