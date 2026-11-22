package indexer

import (
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strings"

	"github.com/zzet/gortex/internal/excludes"
	"github.com/zzet/gortex/internal/graph/store_pebble"
	"github.com/zzet/gortex/internal/indexer/merkle"
	"github.com/zzet/gortex/internal/indexer/source"
)

var errRebuildIgnorePolicy = errors.New("unproved frozen checkout ignore policy")

// rebuildCheckoutInputs binds one walk's checked listings and admitted
// identities to the same parse input root. It belongs to the owner only.
// A leaf may be resolver input without appearing in the parse manifest.
type rebuildCheckoutInputs struct {
	namespace *rebuildResolverNamespace
	admitted  map[string]*rebuildInput
	files     []rebuildInput
	goConfigs map[string]source.FrozenRootPath
	digest    store_pebble.HeadDigest
}

type rebuildCheckoutLeaf struct {
	input    *rebuildInput
	parse    bool
	typeOnly bool                   // exact regular-name evidence; no identity for reading bytes
	goConfig *source.FrozenRootPath // non-parse Go config, read only if consulted
}

type rebuildCheckoutFreeze func(string, fs.FileMode, rebuildGitMember, *int64, func(rebuildSidecarRow) error) (rebuildCheckoutLeaf, error)

// freezeRebuildCheckoutInputs separates checked names from content identities
// and parse inputs before the parent listing closes. A zero leaf means denied.
// The callback can charge and emit classification facts in the owner's lease.
// Discard all outputs and emitted rows on error.
func freezeRebuildCheckoutInputs(ctx context.Context, root *os.Root, flat *excludes.Matcher,
	git *rebuildGitInventory, rawRemaining *int64,
	freeze rebuildCheckoutFreeze, emit func(rebuildSidecarRow) error,
) (rebuildCheckoutInputs, error) {
	if freeze == nil {
		return rebuildCheckoutInputs{}, fmt.Errorf("%w: missing leaf admission", errRebuildResolverNamespace)
	}
	out := rebuildCheckoutInputs{admitted: make(map[string]*rebuildInput)}
	ns, err := captureRebuildCheckoutNamespace(ctx, root, flat, git, rawRemaining, func(key string, mode fs.FileMode, member rebuildGitMember) (bool, error) {
		leaf, err := freeze(key, mode, member, rawRemaining, emit)
		if err != nil {
			return false, err
		}
		if leaf.typeOnly {
			if !mode.IsRegular() || leaf.input != nil || leaf.parse {
				return false, fmt.Errorf("%w: contradictory regular type evidence for %q", ErrBuildInputChanged, key)
			}
			if obs := leaf.goConfig; obs != nil {
				base := path.Base(key)
				if (base != "go.mod" && base != "go.work") || obs.Path() != key || obs.Info() == nil || !obs.Info().Mode().IsRegular() {
					return false, fmt.Errorf("%w: invalid Go config observation for %q", ErrBuildInputChanged, key)
				}
				if out.goConfigs == nil {
					out.goConfigs = make(map[string]source.FrozenRootPath)
				}
				out.goConfigs[key] = *obs
			}
			return true, nil
		}
		if leaf.goConfig != nil {
			return false, fmt.Errorf("%w: unexpected Go config observation for %q", ErrBuildInputChanged, key)
		}
		if leaf.input == nil && !leaf.parse {
			return false, nil
		}
		input := leaf.input
		want := merkle.RegularFile
		if mode&fs.ModeSymlink != 0 {
			want = merkle.Symlink
		}
		if input == nil || input.Path != key || input.Kind != want || input.Size < 0 ||
			input.Content == (rebuildDigest{}) || input.Language == "" {
			return false, fmt.Errorf("%w: leaf identity differs from checked entry %q", ErrBuildInputChanged, key)
		}
		value := *input
		out.admitted[key] = &value
		if leaf.parse {
			out.files = append(out.files, value)
		}
		return true, nil
	}, emit)
	if err != nil {
		return rebuildCheckoutInputs{}, err
	}
	inputRoot, err := rebuildInputRoot(out.files)
	if err != nil {
		return rebuildCheckoutInputs{}, err
	}
	n, err := hex.Decode(out.digest[:], []byte(inputRoot))
	if err != nil || n != len(out.digest) {
		return rebuildCheckoutInputs{}, fmt.Errorf("%w: invalid input root digest", errRebuildBuildWire)
	}
	if err := ctx.Err(); err != nil {
		return rebuildCheckoutInputs{}, err
	}
	out.namespace = ns
	return out, nil
}

// captureRebuildCheckoutNamespace is the owner's checked disk walk. Use the
// configured Gortex excludes, not a matcher with Git rules already merged.
// A nil inventory is an explicitly selected non-Git walk, not a Git failure
// fallback. leaf supplies the remaining owner admission for open regular files
// and no-follow symlink identities; it may retain checked identities
// for the same build. All outputs, including emitted rows, are tentative
// until the owner has sealed this enumeration and its build inputs together.
func captureRebuildCheckoutNamespace(ctx context.Context, root *os.Root, flat *excludes.Matcher,
	git *rebuildGitInventory, rawRemaining *int64,
	leaf func(string, fs.FileMode, rebuildGitMember) (bool, error), emit func(rebuildSidecarRow) error,
) (*rebuildResolverNamespace, error) {
	if root == nil || !filepath.IsAbs(root.Name()) || leaf == nil || emit == nil {
		return nil, fmt.Errorf("%w: missing checkout walk authority", errRebuildResolverNamespace)
	}
	var n rebuildResolverNamespace
	var dir string
	var listing []rebuildResolverListedEntry
	var policyErr error
	var admitted map[string]rebuildGitMember
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
	prepare := func(entries []rebuildResolverListedEntry) error {
		listing = entries
		if dir != "." {
			marker := path.Join(dir, ".git")
			if _, found := slices.BinarySearchFunc(listing, marker, func(e rebuildResolverListedEntry, key string) int {
				return strings.Compare(e.key, key)
			}); found {
				return fmt.Errorf("%w: nested Git marker appeared in %q", ErrBuildInputChanged, dir)
			}
		}
		allowed := make([]rebuildResolverListedEntry, 0, len(entries))
		for _, e := range entries {
			if err := ctx.Err(); err != nil {
				return err
			}
			denied := rules.match(filepath.Join(absRoot, filepath.FromSlash(e.key)), absRoot, e.mode.IsDir())
			if policyErr != nil {
				return policyErr
			}
			if !denied {
				allowed = append(allowed, e)
			}
		}
		var err error
		admitted, err = prepareRebuildGitDirectory(ctx, root, git, allowed, rawRemaining, emit)
		return err
	}
	classify := func(key string, mode fs.FileMode) (bool, error) {
		member := admitted[key]
		if member == 0 {
			return false, nil
		}
		if mode.IsRegular() || mode&fs.ModeSymlink != 0 {
			return leaf(key, mode, member)
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
			prepare, classify, &n, rawRemaining,
			func(row rebuildSidecarRow) error {
				if row.Kind == rebuildNamespaceDirectoryFact {
					dirs = append(dirs, string(row.Path))
				}
				return emit(row)
			}); err != nil {
			return nil, err
		}
	}
	if git != nil {
		if err := git.checkRoot(root); err != nil {
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
