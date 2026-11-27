package indexer

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"math"
	"os"

	"github.com/zeebo/blake3"
	"github.com/zzet/gortex/internal/clones"
	"github.com/zzet/gortex/internal/config"
	"github.com/zzet/gortex/internal/graph"
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
		(name != "go.mod" && name != "package.json" && name != rebuildPnpmWorkspaceFile && name != rebuildCargoWorkspaceFile && !isRebuildModuleLock(name)) {
		return nil, fmt.Errorf("%w: missing checked root manifest authority", errRebuildProjectNameInput)
	}
	claimed := epoch.admitted[name]
	if (name == "go.mod" || name == "package.json") && ns.entries[name].kind == rebuildNamespaceUnsupported && claimed != nil &&
		claimed.Path == name && claimed.Kind == merkle.Symlink {
		target, data, row, err := readCheckedRebuildManifestLink(ctx, src, ns, *claimed, epoch.admitted, epoch.manifestTargets, limit)
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
	if kind == rebuildNamespaceRegular && claimed == nil {
		if obs, ok := epoch.configObservations[name]; ok {
			capErr := errRebuildCppCaptureLimit
			if name == "go.mod" {
				limit, capErr = min(limit, rebuildGoManifestLimit), errRebuildGoCaptureLimit
			}
			m, err := readRebuildConfigObservation(ctx, obs, min(limit, rebuildCppEntryLimit))
			if errors.Is(err, source.ErrRegularFileTooLarge) {
				err = errors.Join(capErr, err)
			}
			return m, err
		}
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

// A nonparse target carries the metadata and checked prefix of an admitted leaf.
// It gains a content identity only when a consulted root link reads its body.
type rebuildManifestTarget struct {
	info         fs.FileInfo
	prefixDigest rebuildDigest
}

// readCheckedRebuildManifestLink uses the frozen namespace and exact admitted
// identities or nonparse observations to authorize root link traversal.
// The caller shares these checked bytes across the applicable consumers and
// charges them once to the owning sidecar. No bytes or row are usable on error.
func readCheckedRebuildManifestLink(ctx context.Context, src *source.FilesystemSource, ns *rebuildResolverNamespace,
	link rebuildInput, admitted map[string]*rebuildInput, nonparse map[string]rebuildManifestTarget, limit int64,
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
			if target != nil && target.Path == key && target.Kind == merkle.RegularFile && target.Content != (rebuildDigest{}) && target.Size >= 0 {
				return 0, nil
			}
			if observed, ok := nonparse[key]; target == nil && ok && observed.info != nil && observed.info.Mode().IsRegular() && observed.prefixDigest != (rebuildDigest{}) {
				return 0, nil
			}
			return 0, fmt.Errorf("%w: target %q lacks admitted identity", errRebuildResolverInputDenied, key)
		default:
			return 0, fmt.Errorf("%w: target %q not present", errRebuildResolverNamespace, key)
		}
	})
	if err != nil {
		return rebuildInput{}, nil, rebuildSidecarRow{}, err
	}
	key := entry.Path()
	target := admitted[key]
	observed := nonparse[key]
	if !entry.Info().Mode().IsRegular() || (target == nil && observed.info == nil) {
		return rebuildInput{}, nil, rebuildSidecarRow{}, fmt.Errorf("%w: target is not an admitted file: %w", errRebuildProjectNameInput, errRebuildResolverInputDenied)
	}
	if target == nil && !sameRebuildPathMetadata(observed.info, entry.Info()) {
		return rebuildInput{}, nil, rebuildSidecarRow{}, fmt.Errorf("%w: manifest target %q metadata changed", ErrBuildInputChanged, key)
	}
	size := entry.Info().Size()
	if target != nil {
		size = target.Size
	}
	maxBytes := min(limit, rebuildCppEntryLimit)
	if maxBytes < 0 || size > maxBytes {
		return rebuildInput{}, nil, rebuildSidecarRow{}, fmt.Errorf("%w: target %q: %w", errRebuildProjectNameInput, key, errRebuildCppCaptureLimit)
	}
	data, err := entry.Read(ctx, maxBytes)
	if err != nil {
		return rebuildInput{}, nil, rebuildSidecarRow{}, rebuildManifestLinkError(link.Path, err)
	}
	digest := rebuildDigest(blake3.Sum256(data))
	if target == nil {
		if rebuildDigest(blake3.Sum256(data[:min(len(data), sniffPrefixBytes)])) != observed.prefixDigest {
			return rebuildInput{}, nil, rebuildSidecarRow{}, fmt.Errorf("%w: manifest target %q prefix changed", ErrBuildInputChanged, key)
		}
		target = &rebuildInput{Path: key, Language: "manifest", Kind: merkle.RegularFile, Size: int64(len(data)), Content: digest}
	} else if target.Size != int64(len(data)) || digest != target.Content {
		return rebuildInput{}, nil, rebuildSidecarRow{}, fmt.Errorf("%w: verified target %q differs from admitted identity", ErrBuildInputChanged, key)
	}
	return *target, data, rebuildSidecarRow{Kind: rebuildFollowedManifest, Path: []byte(link.Path), Present: true, Digest: digest}, nil
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

func (req rebuildBuildRequest) projectRecipe(cloneThreshold float64) store_pebble.HeadDigest {
	h := blake3.New()
	// The tracked set is exactly empty, so no dependency export was read.
	// Bind that policy alongside the inputs, not an unverified locator.
	_, _ = h.Write([]byte("gortex/rebuild/project-name/v7\x00tracked-set:0\x00dependency-exports:ABSENT\x00"))
	if req.SkipPlaceholderSources {
		_, _ = h.Write([]byte{1})
	} else {
		_, _ = h.Write([]byte{0})
	}
	if req.ExternalCallsEnabled {
		_, _ = h.Write([]byte{1})
	} else {
		_, _ = h.Write([]byte{0})
	}
	if req.ProvidersOff {
		_, _ = h.Write([]byte("semantic:off\x00resolver-lsp:off\x00"))
	}
	// A threshold change can alter direct and diffused relations without
	// changing a parse receipt. Bind the effective native cutoff here.
	if cloneThreshold <= 0 {
		cloneThreshold = clones.DefaultThreshold
	}
	var score [8]byte
	binary.BigEndian.PutUint64(score[:], math.Float64bits(cloneThreshold))
	_, _ = h.Write(score[:])
	_, _ = h.Write(req.Witness.FrozenInputs[:])
	_, _ = h.Write(req.BaseRecipe[:])
	_, _ = h.Write([]byte(req.ProjectName))
	return store_pebble.HeadDigest(h.Sum(nil))
}

func (req rebuildBuildRequest) checkTrackedRepos() error {
	if len(req.TrackedRepos) != 0 || len(req.Witness.Dependencies) != 0 {
		return errRebuildCrossRepoUnavailable
	}
	return nil
}

func (req rebuildBuildRequest) checkProjectRecipe() error {
	if err := req.checkTrackedRepos(); err != nil {
		return err
	}
	if req.Witness.PlatformCatalogRevision != graph.PlatformCatalogRevision {
		return fmt.Errorf("%w: build requires platform catalog revision %d, got %d", errRebuildBuildWire,
			graph.PlatformCatalogRevision, req.Witness.PlatformCatalogRevision)
	}
	if !req.ProvidersOff {
		return fmt.Errorf("%w: missing frozen semantic and resolver LSP off policy", errRebuildBuildWire)
	}
	threshold, err := req.recipeCloneThreshold()
	if err != nil {
		return err
	}
	if req.BaseRecipe == (store_pebble.HeadDigest{}) || req.Witness.Recipe != req.projectRecipe(threshold) {
		return fmt.Errorf("%w: project name, frozen inputs, build settings and witness recipe do not match", errRebuildBuildWire)
	}
	return nil
}

// recipeCloneThreshold reads only the config field this part of the recipe
// owns. An unconfigured capture uses the native default.
func (req rebuildBuildRequest) recipeCloneThreshold() (float64, error) {
	if len(req.Config) == 0 {
		return 0, nil
	}
	var frozen struct {
		Coverage struct{ Clones config.ClonesConfig }
	}
	if err := json.Unmarshal(req.Config, &frozen); err != nil {
		return 0, fmt.Errorf("%w: read frozen clone settings: %w", errRebuildBuildWire, err)
	}
	return (config.CoverageConfig{Clones: frozen.Coverage.Clones}).ClonesThreshold(), nil
}
