package indexer

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/zzet/gortex/internal/excludes"
	"github.com/zzet/gortex/internal/indexer/source"
)

var errRebuildIgnorePolicy = errors.New("unproved frozen checkout ignore policy")

// captureRebuildCheckoutNamespace is the owner's checked disk walk. Use the
// effective root exclusions from the same Git-aware setup as the ordinary
// walk. leaf supplies the remaining owner admission for open regular files
// and the two special root manifest links; it may retain checked identities
// for the same build. All outputs, including emitted rows, are tentative
// until the owner has sealed this enumeration and its build inputs together.
func captureRebuildCheckoutNamespace(ctx context.Context, root *os.Root, flat *excludes.Matcher,
	rawRemaining *int64, leaf func(string, fs.FileMode) (bool, error), emit func(rebuildSidecarRow) error,
) (*rebuildResolverNamespace, error) {
	if root == nil || !filepath.IsAbs(root.Name()) || leaf == nil || emit == nil {
		return nil, fmt.Errorf("%w: missing checkout walk authority", errRebuildResolverNamespace)
	}
	var n rebuildResolverNamespace
	var dir string
	var listing []rebuildResolverListedEntry
	var policyErr error
	absRoot := root.Name()
	checkedRead := func(abs string) ([]byte, error) {
		if policyErr != nil {
			return nil, policyErr
		}
		rel, err := filepath.Rel(absRoot, abs)
		rel = filepath.ToSlash(rel)
		if err != nil || !validRebuildNamespaceKey(rel) || filepath.ToSlash(filepath.Dir(filepath.FromSlash(rel))) != dir {
			policyErr = fmt.Errorf("%w: policy lookup is not in the checked directory", errRebuildIgnorePolicy)
			return nil, policyErr
		}
		i, present := slices.BinarySearchFunc(listing, rel, func(e rebuildResolverListedEntry, key string) int {
			return strings.Compare(e.key, key)
		})
		if !present {
			return nil, fs.ErrNotExist // byte-exact absence in the current listing
		}
		data, err := readRebuildIgnorePolicy(ctx, root, listing[i], rawRemaining)
		if err != nil {
			policyErr = err // Hierarchical treats errors as empty; the owner cannot.
		}
		return data, err
	}
	// Fresh cache per enumeration. Admitting a child directory necessarily
	// checked and cached the ignore rules for its parent and ancestors.
	rules := pathExclusionRules{
		flat: flat, perFolder: excludes.NewHierarchicalWithReader(absRoot, dirIgnoreFiles, checkedRead, nil), parentsOpen: true,
	}
	classify := func(key string, mode fs.FileMode) (bool, error) {
		denied := rules.match(filepath.Join(absRoot, filepath.FromSlash(key)), absRoot, mode.IsDir())
		if policyErr != nil || denied {
			return false, policyErr
		}
		if mode.IsRegular() || mode&fs.ModeSymlink != 0 && (key == "go.mod" || key == "package.json") {
			return leaf(key, mode)
		}
		return true, nil // directories and unsupported entry types; no leaf read
	}
	if (pathExclusionRules{flat: flat}).match(absRoot, absRoot, true) {
		return nil, fmt.Errorf("%w: checkout root excluded: %w", errRebuildResolverNamespace, errRebuildResolverInputDenied)
	}
	dirs := []string{"."}
	for i := 0; i < len(dirs); i++ {
		dir, listing = dirs[i], nil
		dirs[i] = ""
		if err := captureRebuildResolverDirectoryWithListing(ctx, root, dir, true,
			func(entries []rebuildResolverListedEntry) { listing = entries }, classify, &n, rawRemaining,
			func(row rebuildSidecarRow) error {
				if row.Kind == rebuildNamespaceDirectoryFact {
					dirs = append(dirs, string(row.Path))
				}
				return emit(row)
			}); err != nil {
			return nil, err
		}
	}
	if err := n.freeze(); err != nil {
		return nil, err
	}
	return &n, nil
}

func readRebuildIgnorePolicy(ctx context.Context, root *os.Root, entry rebuildResolverListedEntry, remaining *int64) ([]byte, error) {
	if !entry.mode.IsRegular() {
		return nil, fmt.Errorf("%w: %q: %w", errRebuildIgnorePolicy, entry.key, source.ErrNotRegularFile)
	}
	p, err := source.ObserveFrozenLiteralRootPath(ctx, root, entry.key)
	if err != nil {
		return nil, fmt.Errorf("%w: observe %q: %w", errRebuildIgnorePolicy, entry.key, errors.Join(ErrBuildInputChanged, err))
	}
	data, err := p.Read(ctx, min(*remaining, rebuildCppEntryLimit))
	if err != nil {
		if errors.Is(err, source.ErrRegularFileTooLarge) {
			err = errors.Join(errRebuildCppCaptureLimit, err)
		}
		return nil, fmt.Errorf("%w: read %q: %w", errRebuildIgnorePolicy, entry.key, err)
	}
	*remaining -= int64(len(data))
	return data, nil
}
