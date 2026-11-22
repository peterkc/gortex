package indexer

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"

	"github.com/zeebo/blake3"
	"github.com/zzet/gortex/internal/graph/store_pebble"
	"github.com/zzet/gortex/internal/indexer/merkle"
	"github.com/zzet/gortex/internal/indexer/source"
	"github.com/zzet/gortex/internal/search"
)

var errRebuildProjectNameInput = errors.New("unverified frozen project-name manifest")

// readRebuildProjectName reads only the manifests consulted by native
// selection. A nil map value is known absent; a missing key is no evidence.
func readRebuildProjectName(ctx context.Context, src *source.FilesystemSource, manifests map[string]*rebuildInput,
	rawRemaining *int64, emit func(rebuildSidecarRow) error,
) (string, error) {
	return search.PortableProjectName(func(path string) ([]byte, error) {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		input, known := manifests[path]
		if !known {
			return nil, fmt.Errorf("%w: no evidence for %s", errRebuildProjectNameInput, path)
		}
		if input == nil {
			return nil, nil
		}
		if input.Path != path || input.Size < 0 {
			return nil, fmt.Errorf("%w: wrong identity for %s", errRebuildProjectNameInput, path)
		}
		switch input.Kind {
		case merkle.RegularFile:
			if input.Size > *rawRemaining {
				return nil, fmt.Errorf("%s: %w", path, errRebuildCppCaptureLimit)
			}
			data, err := readFrozenRebuildInput(ctx, src, *input)
			if err != nil {
				return nil, err
			}
			*rawRemaining -= int64(len(data))
			return data, nil
		case merkle.Symlink:
			return readRebuildProjectNameLink(ctx, src, *input, rawRemaining, emit)
		default:
			return nil, fmt.Errorf("%w: %s is not a file or symlink", errRebuildProjectNameInput, path)
		}
	})
}

func readRebuildProjectNameLink(ctx context.Context, src *source.FilesystemSource, input rebuildInput,
	rawRemaining *int64, emit func(rebuildSidecarRow) error,
) ([]byte, error) {
	entry, err := observeRebuildManifestLink(ctx, src, input, nil)
	if err != nil {
		return nil, err
	}
	data, err := entry.Read(ctx, min(*rawRemaining, rebuildCppEntryLimit))
	if err != nil {
		return nil, rebuildManifestLinkError(input.Path, err)
	}
	*rawRemaining -= int64(len(data))
	if err := emit(rebuildSidecarRow{Kind: rebuildFollowedManifest, Path: []byte(input.Path), Present: true, Digest: blake3.Sum256(data)}); err != nil {
		return nil, err
	}
	return data, nil
}

// readCheckedRebuildRootManifest observes only one logical name from the
// admitted epoch. Regular targets and root links use the same digest proof.
// A nil result is a checked nonfile. The caller charges any returned bytes.
func readCheckedRebuildRootManifest(ctx context.Context, src *source.FilesystemSource, epoch rebuildCheckoutInputs,
	name string, limit int64,
) (*rebuildRootManifest, error) {
	ns := epoch.namespace
	if src == nil || ns == nil || !ns.frozen || ns.entries["."].close != rebuildNamespaceCloseExact ||
		(name != "go.mod" && name != "package.json") {
		return nil, fmt.Errorf("%w: missing checked root manifest authority", errRebuildProjectNameInput)
	}
	claimed := epoch.admitted[name]
	if ns.entries[name].kind == rebuildNamespaceUnsupported && claimed != nil &&
		claimed.Path == name && claimed.Kind == merkle.Symlink {
		target, data, row, err := readCheckedRebuildManifestLink(ctx, src, ns, *claimed, epoch.admitted, limit)
		if err != nil {
			return nil, err
		}
		return &rebuildRootManifest{link: *claimed, target: target, row: row, data: data}, nil
	}
	kind, err := ns.lookup(name)
	if err != nil {
		return nil, fmt.Errorf("%w: %s: %w", errRebuildProjectNameInput, name, err)
	}
	if (kind == rebuildNamespaceAbsent || kind == rebuildNamespaceDirectory) && claimed == nil {
		return nil, nil
	}
	if kind != rebuildNamespaceRegular || claimed == nil || claimed.Path != name || claimed.Kind != merkle.RegularFile {
		return nil, fmt.Errorf("%w: mismatched admitted identity for %s", errRebuildProjectNameInput, name)
	}
	data, err := readCheckedRebuildRegularManifest(ctx, src, *claimed, limit)
	if err != nil {
		return nil, err
	}
	return &rebuildRootManifest{target: *claimed, data: data}, nil
}

func readCheckedRebuildRegularManifest(ctx context.Context, src *source.FilesystemSource, in rebuildInput, limit int64) ([]byte, error) {
	if in.Size > min(limit, rebuildCppEntryLimit) {
		return nil, fmt.Errorf("%w: manifest %q", errRebuildCppCaptureLimit, in.Path)
	}
	data, err := readRebuildGoObservedManifest(ctx, src.FrozenRoot(), in)
	if err != nil {
		return nil, err
	}
	if int64(len(data)) != in.Size || rebuildDigest(blake3.Sum256(data)) != in.Content {
		return nil, fmt.Errorf("%w: %q", ErrBuildInputChanged, in.Path)
	}
	return data, nil
}

// readCheckedRebuildManifestLink uses only a frozen namespace and the owner's
// admitted regular target identities to authorize the root link traversal.
// The caller shares these checked bytes across the applicable consumers and
// charges them once to the owning sidecar. No bytes or row are usable on error.
func readCheckedRebuildManifestLink(ctx context.Context, src *source.FilesystemSource, ns *rebuildResolverNamespace,
	link rebuildInput, admitted map[string]*rebuildInput, limit int64,
) (rebuildInput, []byte, rebuildSidecarRow, error) {
	if src == nil || ns == nil || !ns.frozen || ns.entries["."].close != rebuildNamespaceCloseExact ||
		(link.Path != "go.mod" && link.Path != "package.json") || link.Kind != merkle.Symlink ||
		ns.entries[link.Path].kind != rebuildNamespaceUnsupported || link.Content == (rebuildDigest{}) || link.Size <= 0 {
		return rebuildInput{}, nil, rebuildSidecarRow{}, fmt.Errorf("%w: no checked root manifest link", errRebuildProjectNameInput)
	}
	entry, err := observeRebuildManifestLink(ctx, src, link, func(key string) (fs.FileMode, error) {
		kind, err := ns.lookup(key)
		if err != nil {
			return 0, err
		}
		switch kind {
		case rebuildNamespaceDirectory:
			if ns.entries[key].close != rebuildNamespaceCloseExact {
				return 0, fmt.Errorf("%w: unclosed target directory %q", errRebuildResolverNamespace, key)
			}
			return fs.ModeDir, nil
		case rebuildNamespaceRegular:
			target := admitted[key]
			if target == nil || target.Path != key || target.Kind != merkle.RegularFile || target.Content == (rebuildDigest{}) || target.Size < 0 {
				return 0, fmt.Errorf("%w: target lacks admitted identity", errRebuildResolverInputDenied)
			}
			return 0, nil
		default:
			return 0, fmt.Errorf("%w: target %q not present", errRebuildResolverNamespace, key)
		}
	})
	if err != nil {
		return rebuildInput{}, nil, rebuildSidecarRow{}, err
	}
	target := admitted[entry.Path()]
	if target == nil || !entry.Info().Mode().IsRegular() {
		return rebuildInput{}, nil, rebuildSidecarRow{}, fmt.Errorf("%w: target is not an admitted file: %w", errRebuildProjectNameInput, errRebuildResolverInputDenied)
	}
	if limit < 0 || target.Size > min(limit, rebuildCppEntryLimit) {
		return rebuildInput{}, nil, rebuildSidecarRow{}, fmt.Errorf("%w: target %q: %w", errRebuildProjectNameInput, entry.Path(), errRebuildCppCaptureLimit)
	}
	data, err := entry.Read(ctx, min(limit, rebuildCppEntryLimit))
	if err != nil {
		return rebuildInput{}, nil, rebuildSidecarRow{}, rebuildManifestLinkError(link.Path, err)
	}
	if target.Size != int64(len(data)) || rebuildDigest(blake3.Sum256(data)) != target.Content {
		return rebuildInput{}, nil, rebuildSidecarRow{}, fmt.Errorf("%w: verified target differs from admitted identity", ErrBuildInputChanged)
	}
	return *target, data, rebuildSidecarRow{Kind: rebuildFollowedManifest, Path: []byte(link.Path), Present: true, Digest: target.Content}, nil
}

// freezeRebuildRootManifestLink records a root link as a leaf without opening
// its target. Following it later needs a separate checked target identity.
func freezeRebuildRootManifestLink(ctx context.Context, root *os.Root, key string, raw *int64) (rebuildCheckoutLeaf, error) {
	if err := ctx.Err(); err != nil {
		return rebuildCheckoutLeaf{}, err
	}
	if root == nil || raw == nil || *raw < 0 || *raw > rebuildCppRawLimit || (key != "go.mod" && key != "package.json") {
		return rebuildCheckoutLeaf{}, errRebuildProjectNameInput
	}
	before, err := root.Lstat(key)
	if err != nil || before.Mode()&fs.ModeSymlink == 0 {
		return rebuildCheckoutLeaf{}, fmt.Errorf("%w: expected literal root manifest link %q: %v", ErrBuildInputChanged, key, err)
	}
	limit := min(*raw, rebuildCppEntryLimit)
	if before.Size() > limit {
		return rebuildCheckoutLeaf{}, fmt.Errorf("%w: root manifest link %q", errRebuildCppCaptureLimit, key)
	}
	target, err := root.Readlink(key)
	if err != nil {
		return rebuildCheckoutLeaf{}, fmt.Errorf("%w: read literal manifest link %q: %w", ErrBuildInputChanged, key, err)
	}
	after, err := root.Lstat(key)
	if err != nil || !sameRebuildPathMetadata(before, after) || int64(len(target)) != before.Size() || target == "" {
		return rebuildCheckoutLeaf{}, fmt.Errorf("%w: root manifest link changed: %q: %v", ErrBuildInputChanged, key, err)
	}
	if err := ctx.Err(); err != nil {
		return rebuildCheckoutLeaf{}, err
	}
	*raw -= int64(len(target))
	return rebuildCheckoutLeaf{input: &rebuildInput{
		Path: key, Language: "manifest", Kind: merkle.Symlink, Size: int64(len(target)), Content: blake3.Sum256([]byte(target)),
	}}, nil
}

func observeRebuildManifestLink(ctx context.Context, src *source.FilesystemSource, link rebuildInput,
	admitTarget func(string) (fs.FileMode, error),
) (source.FrozenRootPath, error) {
	entry, err := src.ResolveFrozenRootLink(ctx, link.Path, func(target string) error {
		if int64(len(target)) != link.Size || rebuildDigest(blake3.Sum256([]byte(target))) != link.Content {
			return ErrBuildInputChanged
		}
		return nil
	}, admitTarget)
	if err != nil {
		return source.FrozenRootPath{}, rebuildManifestLinkError(link.Path, err)
	}
	return entry, nil
}

func rebuildManifestLinkError(path string, err error) error {
	if errors.Is(err, fs.ErrNotExist) || errors.Is(err, source.ErrFrozenRootChanged) {
		err = errors.Join(ErrBuildInputChanged, err)
	} else if errors.Is(err, source.ErrRegularFileTooLarge) {
		err = errors.Join(errRebuildCppCaptureLimit, err)
	}
	return fmt.Errorf("%w: %s: %w", errRebuildProjectNameInput, path, err)
}

func rebuildProjectRecipe(base store_pebble.HeadDigest, name string, inputs store_pebble.HeadDigest) store_pebble.HeadDigest {
	h := blake3.New()
	if inputs == (store_pebble.HeadDigest{}) {
		_, _ = h.Write([]byte("gortex/rebuild/project-name/v1\x00"))
	} else {
		_, _ = h.Write([]byte("gortex/rebuild/project-name/v2\x00"))
		_, _ = h.Write(inputs[:])
	}
	_, _ = h.Write(base[:])
	_, _ = h.Write([]byte(name))
	return store_pebble.HeadDigest(h.Sum(nil))
}

func (req rebuildBuildRequest) checkProjectRecipe() error {
	if req.BaseRecipe == (store_pebble.HeadDigest{}) || req.Witness.Recipe != rebuildProjectRecipe(req.BaseRecipe, req.ProjectName, req.Witness.FrozenInputs) {
		return fmt.Errorf("%w: project name, frozen inputs and witness recipe do not match", errRebuildBuildWire)
	}
	return nil
}
