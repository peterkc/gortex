package indexer

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path"

	"github.com/zeebo/blake3"
	"github.com/zzet/gortex/internal/indexer/merkle"
	"github.com/zzet/gortex/internal/indexer/source"
)

// rebuildFollowedGoMod is the owner's same-epoch proof for the one root
// go.mod link. link hashes the link text; target hashes the admitted regular
// file. row is the existing followed-manifest row in the same transaction.
// The owner lends data to this call and leaves its raw charge to Go capture.
type rebuildFollowedGoMod struct {
	link, target rebuildInput
	data         []byte
	row          rebuildSidecarRow
}

// captureRebuildGoObservations binds the owner's admitted Go inputs and
// manifest identities to its frozen namespace. All three must come from the
// same enumeration of root. The manifest map supplies positives only; only
// the namespace can prove a miss. Emitted rows belong to the owning lease.
func captureRebuildGoObservations(ctx context.Context, root *os.Root, n *rebuildResolverNamespace,
	inputs []rebuildInput, manifests map[string]*rebuildInput, followed *rebuildFollowedGoMod, rawRemaining *int64, emit func(rebuildGoManifest) error,
) error {
	if root == nil || n == nil || !n.frozen || n.entries["."].close == rebuildNamespaceUnknown {
		return fmt.Errorf("%w: Go capture requires a held root and frozen, closed root namespace", errRebuildResolverNamespace)
	}
	read := func(ctx context.Context, in rebuildInput) ([]byte, error) {
		if in.Path == "go.mod" && followed != nil {
			return followed.data, nil
		}
		return readRebuildGoObservedManifest(ctx, root, in)
	}
	observe := func(ctx context.Context, name string) (*rebuildInput, error) {
		entry := n.entries[name]
		switch entry.kind {
		case rebuildNamespaceDenied:
			return nil, fmt.Errorf("%w: %w", errRebuildResolverNamespace, errRebuildResolverInputDenied)
		case rebuildNamespaceUnsupported:
			if name != "go.mod" || followed == nil {
				return nil, fmt.Errorf("%w: symlink or special manifest", errRebuildResolverNamespace)
			}
		}
		if n.entries[path.Dir(name)].close == rebuildNamespaceUnknown {
			return nil, fmt.Errorf("%w: manifest parent is not a closed admitted directory", errRebuildResolverNamespace)
		}
		claimed := manifests[name]
		if name == "go.mod" && followed != nil {
			target, row := followed.target, followed.row
			if entry.kind != rebuildNamespaceUnsupported || claimed == nil || *claimed != followed.link ||
				claimed.Kind != merkle.Symlink || claimed.Path != name || claimed.Language == "" || claimed.Size <= 0 ||
				claimed.Content == (rebuildDigest{}) || !validRebuildNamespaceKey(target.Path) || target.Path == name ||
				target.Kind != merkle.RegularFile || target.Size < 0 || row.Kind != rebuildFollowedManifest || string(row.Path) != name {
				return nil, fmt.Errorf("%w: invalid followed root go.mod binding", errRebuildGoEvidence)
			}
			if err := row.check(); err != nil {
				return nil, err
			}
			if target.Content != row.Digest {
				return nil, fmt.Errorf("%w: followed go.mod target digest differs from sidecar row", ErrBuildInputChanged)
			}
			// Normalize as go.mod, without changing either owner's identity.
			target.Path = name
			return &target, nil
		}
		kind, err := n.lookup(name)
		if err != nil {
			return nil, err // In particular, ordinary closure proves no exact miss.
		}
		if kind == rebuildNamespaceAbsent && claimed == nil {
			return nil, nil
		}
		if kind != rebuildNamespaceRegular || claimed == nil || claimed.Path != name || claimed.Language == "" ||
			claimed.Kind != merkle.RegularFile || claimed.Size < 0 || claimed.Content == (rebuildDigest{}) {
			return nil, fmt.Errorf("%w: namespace and admitted regular manifest identity do not match", errRebuildGoEvidence)
		}
		in := *claimed
		if path.Base(name) == "go.work" {
			// The manifest collector does not call read for a workspace
			// boundary. Verify and charge its bytes without parsing them.
			if in.Size > min(rebuildGoManifestLimit, *rawRemaining) {
				return nil, errRebuildGoCaptureLimit
			}
			data, err := read(ctx, in)
			if err != nil {
				return nil, err
			}
			if rebuildDigest(blake3.Sum256(data)) != in.Content {
				return nil, ErrBuildInputChanged
			}
			*rawRemaining -= in.Size
		}
		return &in, nil
	}
	if emit != nil && followed != nil {
		yield := emit
		emit = func(row rebuildGoManifest) error {
			if row.Path == "go.mod" {
				// The native loose root claim follows links. Its strict
				// nearest-owner read does not, but still stops at the link.
				row.OwnerModule = ""
			}
			return yield(row)
		}
	}
	return captureRebuildGoManifests(ctx, inputs, rawRemaining, observe, read, emit)
}

func readRebuildGoObservedManifest(ctx context.Context, root *os.Root, frozen rebuildInput) ([]byte, error) {
	observed, err := source.ObserveFrozenLiteralRootPath(ctx, root, frozen.Path)
	if err != nil {
		if !errors.Is(err, context.Canceled) && !errors.Is(err, context.DeadlineExceeded) {
			err = fmt.Errorf("%w: %w", ErrBuildInputChanged, err)
		}
		return nil, err
	}
	if observed.Path() != frozen.Path || !observed.Info().Mode().IsRegular() || observed.Info().Size() != frozen.Size {
		return nil, fmt.Errorf("%w: %q no longer names a regular manifest of the frozen size", ErrBuildInputChanged, frozen.Path)
	}
	data, err := observed.Read(ctx, frozen.Size)
	if errors.Is(err, source.ErrFrozenRootChanged) {
		err = fmt.Errorf("%w: %w", ErrBuildInputChanged, err)
	}
	// The bounded descriptor read checks live metadata. The caller still
	// compares the bytes with the earlier admitted digest, not a new identity.
	return data, err
}
