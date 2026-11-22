package indexer

import (
	"context"

	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/graph/store_pebble"
)

// prepareRebuildSearch takes the frozen effective project name from the caller;
// token rendering must not read the live checkout. Like the native shadow
// drain, it includes eligible unowned nodes in a named repo's final union.
func (idx *Indexer) prepareRebuildSearch(
	ctx context.Context, build *store_pebble.BuildStore, dir string,
	derived store_pebble.Witness, incarnation uint64, projectName string,
) (store_pebble.StageTables, error) {
	return build.PrepareSearch(ctx, dir, derived, incarnation, func(n *graph.Node) (string, bool) {
		if !idx.shouldIndexForSearch(n) {
			return "", false
		}
		return ftsTokensFor(n, projectName), true
	})
}
