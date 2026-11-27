package indexer

import (
	"context"

	"github.com/zzet/gortex/internal/graph/store_pebble"
)

// openRebuildCppReplay keeps sidecar validation in the indexer and leaves the
// scratch keyspace and its lifetime to the store.
func openRebuildCppReplay(ctx context.Context, lease string, expected rebuildDigest) (*store_pebble.RebuildCppReplay, error) {
	return store_pebble.OpenRebuildCppReplay(ctx, lease, rebuildCppEntryLimit, func(tu func([]byte, [][]byte) error, heuristic func([]byte) error) error {
		return visitRebuildInputSidecar(ctx, lease, expected, func(row rebuildSidecarRow) error {
			switch row.Kind {
			case rebuildCppTU:
				return tu(row.Path, row.Dirs)
			case rebuildCppHeuristicDir:
				return heuristic(row.Path)
			default:
				return nil // Database receipts and followed manifests do not supply paths.
			}
		})
	})
}
