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
// are opaque. exactNames must come from an independent native-name proof.
// Keep no rows or ledger if capture, classification or lease binding fails.
func captureRebuildResolverDirectory(ctx context.Context, root *os.Root, dir string, exactNames bool,
	allowed func(string, fs.FileMode) (bool, error), n *rebuildResolverNamespace, rawRemaining *int64,
	emit func(rebuildSidecarRow) error,
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
	var rows []rebuildNamespaceRow
	err := source.VisitFrozenDirectory(ctx, root, dir, func(name string, mode fs.FileMode) error {
		key := path.Join(dir, name)
		if !validRebuildNamespaceKey(key) || len(rows) >= room || len(key) > keys || int64(len(key))+1 > left {
			return fmt.Errorf("%w: directory fact key or capture cap", errRebuildResolverNamespace)
		}
		ok, err := allowed(key, mode)
		if err != nil {
			return err
		}
		kind := rebuildNamespaceDenied
		if ok {
			switch {
			case mode.IsDir():
				kind = rebuildNamespaceDirectory
			case mode.IsRegular():
				kind = rebuildNamespaceRegular
			default:
				kind = rebuildNamespaceUnsupported
			}
		}
		keys -= len(key)
		left -= int64(len(key)) + 1
		rows = append(rows, rebuildNamespaceRow{key: key, kind: kind})
		return nil
	})
	if err != nil {
		return err
	}
	slices.SortFunc(rows, func(a, b rebuildNamespaceRow) int { return strings.Compare(a.key, b.key) })
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
