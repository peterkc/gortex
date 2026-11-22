package indexer

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/zzet/gortex/internal/graph/store_pebble"
	"github.com/zzet/gortex/internal/indexer/source"
)

type rebuildCaptureFacts func(*int64, func(rebuildSidecarRow) error) error

// withRebuildBuildInputs owns the lease through lookup, reuse or child exit.
// consume receives the bound version; it must not outlive the call or lease.
func withRebuildBuildInputs(ctx context.Context, leases string, src *source.FilesystemSource,
	manifests map[string]*rebuildInput, req rebuildBuildRequest, capture rebuildCaptureFacts,
	consume func(string, rebuildBuildRequest) error,
) (err error) {
	if !filepath.IsAbs(leases) || consume == nil {
		return fmt.Errorf("%w: build lease parent or consumer", errRebuildBuildWire)
	}
	lease, err := os.MkdirTemp(leases, "build-")
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, os.RemoveAll(lease)) }()
	bound, err := captureRebuildBuildInputs(ctx, lease, src, manifests, req, capture)
	if err != nil {
		return err
	}
	return consume(lease, bound)
}

func captureRebuildBuildInputs(ctx context.Context, lease string, src *source.FilesystemSource,
	manifests map[string]*rebuildInput, req rebuildBuildRequest, capture rebuildCaptureFacts,
) (rebuildBuildRequest, error) {
	if req.Witness.Recipe == (store_pebble.HeadDigest{}) || req.BaseRecipe != (store_pebble.HeadDigest{}) ||
		req.Witness.FrozenInputs != (store_pebble.HeadDigest{}) || src == nil || req.Root != src.FrozenRoot().Name() {
		return rebuildBuildRequest{}, fmt.Errorf("%w: capture needs an unbound recipe and the enumerating root", errRebuildBuildWire)
	}
	var name string
	var hasRows bool
	remaining := int64(rebuildCppRawLimit)
	digest, err := writeRebuildInputSidecar(ctx, lease, func(emit func(rebuildSidecarRow) error) error {
		yield := func(row rebuildSidecarRow) error {
			hasRows = true
			return emit(row)
		}
		var err error
		name, err = readRebuildProjectName(ctx, src, manifests, &remaining, yield)
		if err != nil || capture == nil {
			return err
		}
		return capture(&remaining, yield)
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
	req.Witness.FrozenInputs = store_pebble.HeadDigest(digest)
	req.Witness.Recipe = rebuildProjectRecipe(req.BaseRecipe, name, req.Witness.FrozenInputs)
	return req, nil
}
