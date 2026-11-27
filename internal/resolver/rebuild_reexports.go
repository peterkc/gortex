package resolver

import (
	"context"

	"github.com/zzet/gortex/internal/graph/store_pebble"
)

// withRebuildImportClosure is the post-resolution reachability pass. Freeze
// the private graph after import resolution and before the guard applies any
// changes. The guard can query the complete closure during consume. This
// is not the direct-import index used during candidate selection.
func withRebuildImportClosure(
	ctx context.Context, view store_pebble.RebuildView, repos map[string]struct{},
	consume func(*store_pebble.RebuildImportClosure) error,
) (store_pebble.RebuildReexportStats, error) {
	return view.WithImportClosure(ctx, repos, importClosureSkipsTarget, filePathDir, consume)
}
