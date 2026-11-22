package indexer

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"

	"github.com/zzet/gortex/internal/excludes"
	"github.com/zzet/gortex/internal/graph/store_pebble"
	"github.com/zzet/gortex/internal/indexer/source"
)

type rebuildCaptureFacts func(*int64, func(rebuildSidecarRow) error) error

// withRebuildBuildInputs owns the lease through lookup, reuse or child exit.
// consume receives the bound version; it must not outlive the call or lease.
func withRebuildBuildInputs(ctx context.Context, leases string, src *source.FilesystemSource,
	manifests map[string]*rebuildInput, req rebuildBuildRequest, capture rebuildCaptureFacts,
	consume func(string, rebuildBuildRequest) error,
) error {
	if consume == nil {
		return fmt.Errorf("%w: missing build consumer", errRebuildBuildWire)
	}
	return withRebuildInputLease(leases, func(lease string) error {
		bound, err := captureRebuildBuildInputs(ctx, lease, src, manifests, req, capture)
		if err != nil {
			return err
		}
		return consume(lease, bound)
	})
}

// withRebuildCheckoutBuildInputs binds the one owner's checked walk, identity
// and resolver facts before handing anything to reuse or worker dispatch.
func withRebuildCheckoutBuildInputs(ctx context.Context, leases string, src *source.FilesystemSource,
	req rebuildBuildRequest, flat *excludes.Matcher,
	freeze func(string, fs.FileMode) (*rebuildInput, bool, error),
	capture func(rebuildCheckoutInputs, *int64, func(rebuildSidecarRow) error) (string, error),
	consume func(string, rebuildBuildRequest, []rebuildInput) error,
) error {
	if capture == nil || consume == nil {
		return fmt.Errorf("%w: missing checkout capture or consumer", errRebuildBuildWire)
	}
	return withRebuildInputLease(leases, func(lease string) error {
		var epoch rebuildCheckoutInputs
		bound, err := sealRebuildBuildInputs(ctx, lease, src, req,
			func(raw *int64, emit func(rebuildSidecarRow) error) (string, store_pebble.HeadDigest, error) {
				var err error
				epoch, err = freezeRebuildCheckoutInputs(ctx, src.FrozenRoot(), flat, raw, freeze, emit)
				if err != nil {
					return "", store_pebble.HeadDigest{}, err
				}
				if want := req.Witness.Input; want != (store_pebble.HeadDigest{}) && want != epoch.digest {
					return "", store_pebble.HeadDigest{}, fmt.Errorf("%w: checkout input root differs", ErrBuildInputChanged)
				}
				name, err := capture(epoch, raw, emit)
				return name, epoch.digest, err
			})
		if err != nil {
			return err
		}
		return consume(lease, bound, epoch.files)
	})
}

func withRebuildInputLease(leases string, use func(string) error) (err error) {
	if !filepath.IsAbs(leases) {
		return fmt.Errorf("%w: non-absolute build lease parent", errRebuildBuildWire)
	}
	lease, err := os.MkdirTemp(leases, "build-")
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, os.RemoveAll(lease)) }()
	return use(lease)
}

func captureRebuildBuildInputs(ctx context.Context, lease string, src *source.FilesystemSource,
	manifests map[string]*rebuildInput, req rebuildBuildRequest, capture rebuildCaptureFacts,
) (rebuildBuildRequest, error) {
	return sealRebuildBuildInputs(ctx, lease, src, req, func(raw *int64, emit func(rebuildSidecarRow) error) (string, store_pebble.HeadDigest, error) {
		name, err := readRebuildProjectName(ctx, src, manifests, raw, emit)
		if err == nil && capture != nil {
			err = capture(raw, emit)
		}
		return name, req.Witness.Input, err
	})
}

func sealRebuildBuildInputs(ctx context.Context, lease string, src *source.FilesystemSource, req rebuildBuildRequest,
	capture func(*int64, func(rebuildSidecarRow) error) (string, store_pebble.HeadDigest, error),
) (rebuildBuildRequest, error) {
	if req.Witness.Recipe == (store_pebble.HeadDigest{}) || req.BaseRecipe != (store_pebble.HeadDigest{}) ||
		req.Witness.FrozenInputs != (store_pebble.HeadDigest{}) || src == nil || req.Root != src.FrozenRoot().Name() {
		return rebuildBuildRequest{}, fmt.Errorf("%w: capture needs an unbound recipe and the enumerating root", errRebuildBuildWire)
	}
	var name string
	var input store_pebble.HeadDigest
	var hasRows bool
	remaining := int64(rebuildCppRawLimit)
	digest, err := writeRebuildInputSidecar(ctx, lease, func(emit func(rebuildSidecarRow) error) error {
		yield := func(row rebuildSidecarRow) error {
			hasRows = true
			return emit(row)
		}
		var err error
		name, input, err = capture(&remaining, yield)
		return err
	})
	if err != nil {
		return rebuildBuildRequest{}, err
	}
	if !hasRows {
		if err := os.Remove(filepath.Join(lease, rebuildInputSidecarName)); err != nil {
			return rebuildBuildRequest{}, err
		}
		digest = rebuildDigest{}
	}
	req.ProjectName, req.BaseRecipe = name, req.Witness.Recipe
	req.Witness.Input, req.Witness.FrozenInputs = input, store_pebble.HeadDigest(digest)
	req.Witness.Recipe = rebuildProjectRecipe(req.BaseRecipe, name, req.Witness.FrozenInputs)
	return req, nil
}
