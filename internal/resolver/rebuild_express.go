package resolver

import (
	"context"
	"iter"

	"github.com/zzet/gortex/internal/graph/store_pebble"
)

// RunRebuildExpress settles Express handler Calls before React fallback.
func RunRebuildExpress(ctx context.Context, b *store_pebble.BuildStore, receipts iter.Seq2[store_pebble.ParseFacts, error],
	selection FrameworkSynthesizerSelection,
) (store_pebble.RebuildExpressStats, error) {
	if !selection.allows(SynthExpressResolve) {
		return store_pebble.RebuildExpressStats{}, nil
	}
	return b.RebuildExpress(ctx, receipts, store_pebble.RebuildExpressRules{
		Handler:    expressHandlerKey,
		Owner:      expressMemberOwner,
		SimpleName: expressSimpleName,
		Candidate:  isConventionResolvable,
		Names:      dirConventionNames,
		Preferred: func(path, class string) bool {
			dirs := expressMiddlewareDirs
			if class != "" {
				dirs = expressMemberDirs(class)
			}
			return dirMatchesAny(path, dirs)
		},
		Directory: dirOf,
		Rebind:    rebindExpressHandler,
	})
}
