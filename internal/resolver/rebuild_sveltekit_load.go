package resolver

import (
	"context"
	"iter"

	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/graph/store_pebble"
)

// RunRebuildSvelteKitLoad applies the native SvelteKit turn after GoFrame.
func RunRebuildSvelteKitLoad(ctx context.Context, build *store_pebble.BuildStore, receipts iter.Seq2[store_pebble.ParseFacts, error],
	selection FrameworkSynthesizerSelection, skips iter.Seq[*graph.Node],
) (uint64, error) {
	if !selection.allows(SynthSvelteKitLoad) {
		return 0, nil
	}
	return build.RebuildSvelteKitLoad(ctx, receipts, skips, store_pebble.RebuildSvelteKitLoadRules{
		RouteNode: svelteKitRouteNode,
		ServerDir: svelteKitServerDir,
		PageFile: func(path string) bool {
			base, _ := pathBaseDir(path)
			return isSvelteKitPageFile(base)
		},
		Target:    svelteKitLoadTarget,
		Reference: svelteKitLoadReference,
	})
}
