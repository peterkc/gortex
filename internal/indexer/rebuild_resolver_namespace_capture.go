package indexer

import (
	"context"
	"fmt"
	"io/fs"
	"os"
	"path"
	"slices"
	"strings"

	"github.com/zzet/gortex/internal/indexer/source"
)

// captureRebuildResolverDirectory records one literal directory at the
// owner's enumeration boundary. The owner alone classifies policy acceptance
// (including Git, nested checkout and input admission). For a non-root
// directory, that decision must already be present in n; denied directories
// are opaque. exactNames binds this listing for the canonical byte-exact
// stat-style lookups (AC-54), not the host filesystem's spelling aliases.
// Keep no rows or ledger if capture, classification or lease binding fails.
func captureRebuildResolverDirectory(ctx context.Context, root *os.Root, dir string, exactNames bool,
	allowed func(string, fs.FileMode) (bool, error), n *rebuildResolverNamespace, rawRemaining *int64,
	emit func(rebuildSidecarRow) error,
) error {
	return captureRebuildResolverDirectoryWithListing(ctx, root, dir, exactNames, nil, allowed, n, rawRemaining, emit)
}

type rebuildResolverListedEntry struct {
	key  string
	mode fs.FileMode
}

// listed receives a checked, sorted immediate listing before classification.
// Its entries are tentative until every classification and emit succeeds.
func captureRebuildResolverDirectoryWithListing(ctx context.Context, root *os.Root, dir string, exactNames bool,
	listed func([]rebuildResolverListedEntry), allowed func(string, fs.FileMode) (bool, error),
	n *rebuildResolverNamespace, rawRemaining *int64, emit func(rebuildSidecarRow) error,
) error {
	if n == nil || n.frozen || n.invalid != nil || allowed == nil || rawRemaining == nil ||
		*rawRemaining < 0 || *rawRemaining > rebuildCppRawLimit || emit == nil {
		return fmt.Errorf("%w: incomplete directory capture authority", errRebuildResolverNamespace)
	}
	entry := n.entries[dir]
	if (dir != "." && entry.kind != rebuildNamespaceDirectory) || entry.close != rebuildNamespaceUnknown {
		return fmt.Errorf("%w: directory is not open and admitted", errRebuildResolverNamespace)
	}
	closing := rebuildNamespaceClose
	if exactNames {
		closing = rebuildNamespaceCloseExact
	}
	left := *rawRemaining - int64(len(dir)) - 1
	if left < 0 {
		return fmt.Errorf("%w: namespace raw input cap: %w", errRebuildInputSidecar, errRebuildCppCaptureLimit)
	}
	room := rebuildNamespaceEntryLimit - len(n.entries)
	if n.entries["."].close != rebuildNamespaceUnknown {
		room++ // root closure is not a child entry
	}
	keys := rebuildNamespaceKeyBytesLimit - n.keyBytes
	var entries []rebuildResolverListedEntry
	var rows []rebuildNamespaceRow
	err := source.VisitFrozenDirectory(ctx, root, dir, func(name string, mode fs.FileMode) error {
		key := path.Join(dir, name)
		if !validRebuildNamespaceKey(key) || len(entries) >= room || len(key) > keys || int64(len(key))+1 > left {
			return fmt.Errorf("%w: directory fact key or capture cap", errRebuildResolverNamespace)
		}
		keys -= len(key)
		left -= int64(len(key)) + 1
		entries = append(entries, rebuildResolverListedEntry{key: key, mode: mode})
		return nil
	}, func() error {
		slices.SortFunc(entries, func(a, b rebuildResolverListedEntry) int { return strings.Compare(a.key, b.key) })
		if listed != nil {
			listed(entries)
		}
		rows = make([]rebuildNamespaceRow, 0, len(entries)+1)
		for _, e := range entries {
			if err := ctx.Err(); err != nil {
				return err
			}
			ok, err := allowed(e.key, e.mode)
			if err != nil {
				return err
			}
			kind := rebuildNamespaceDenied
			if ok {
				switch {
				case e.mode.IsDir():
					kind = rebuildNamespaceDirectory
				case e.mode.IsRegular():
					kind = rebuildNamespaceRegular
				default:
					kind = rebuildNamespaceUnsupported
				}
			}
			rows = append(rows, rebuildNamespaceRow{key: e.key, kind: kind})
		}
		return nil
	})
	if err != nil {
		return err
	}
	rows = append(rows, rebuildNamespaceRow{key: dir, kind: closing})
	for _, row := range rows {
		if err := ctx.Err(); err != nil {
			return err
		}
		if err := n.add(row); err != nil {
			return err
		}
		if err := writeRebuildNamespaceFact(rawRemaining, row, emit); err != nil {
			return err
		}
	}
	return nil
}
