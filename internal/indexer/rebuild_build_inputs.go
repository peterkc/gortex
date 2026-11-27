package indexer

import (
	"context"
	"encoding/json"
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
	req rebuildBuildRequest, flat *excludes.Matcher, gitCheckout bool,
	freeze rebuildCheckoutFreeze,
	capture func(rebuildCheckoutInputs, *int64, func(rebuildSidecarRow) error) (string, error),
	consume func(string, rebuildBuildRequest, []rebuildInput) error,
) error {
	if err := req.checkTrackedRepos(); err != nil {
		return err
	}
	if capture == nil || consume == nil {
		return fmt.Errorf("%w: missing checkout capture or consumer", errRebuildBuildWire)
	}
	return withRebuildInputLease(leases, func(lease string) error {
		var epoch rebuildCheckoutInputs
		bound, err := sealRebuildBuildInputs(ctx, lease, src, req,
			func(raw *int64, emit func(rebuildSidecarRow) error) (string, store_pebble.HeadDigest, error) {
				var git *rebuildGitInventory
				if gitCheckout {
					inventory, err := readRebuildGitInventory(ctx, src.FrozenRoot(), raw)
					if err != nil {
						return "", store_pebble.HeadDigest{}, err
					}
					if err := emit(inventory.sidecarRow()); err != nil {
						return "", store_pebble.HeadDigest{}, err
					}
					git = &inventory
				}
				var err error
				epoch, err = freezeRebuildCheckoutInputs(ctx, src.FrozenRoot(), flat, git, raw, freeze, emit)
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
		bound.Witness.UsesGitIgnorePolicy = gitCheckout
		return consume(lease, bound, epoch.files)
	})
}

// runRebuildCheckout composes the checked owner epoch and private worker.
// The caller supplies the checkout classification and Gortex-only excludes.
// It owns the candidate head and decides whether to accept each stage.
func (idx *Indexer) runRebuildCheckout(ctx context.Context, src *source.FilesystemSource, req rebuildBuildRequest,
	flat *excludes.Matcher, gitCheckout bool, exe string, args []string, leases, headDir string,
	publish func(context.Context, store_pebble.StageTables) error,
) error {
	if err := req.checkTrackedRepos(); err != nil {
		return err
	}
	if src == nil || req.Bounds.FileBytes <= 0 || publish == nil {
		return fmt.Errorf("%w: missing checkout build authority or file bound", errRebuildBuildWire)
	}
	root := src.FrozenRoot()
	if err := checkRebuildGitWorktreeRoot(root); err != nil {
		return err
	}
	if err := idx.freezeRebuildBuildConfig(&req); err != nil {
		return err
	}
	gate := idx.newContentAdmissionGate()
	return withRebuildCheckoutBuildInputs(ctx, leases, src, req, flat, gitCheckout,
		func(key string, mode fs.FileMode, member rebuildGitMember, raw *int64, emit func(rebuildSidecarRow) error) (rebuildCheckoutLeaf, error) {
			return idx.freezeRebuildCheckoutLeaf(ctx, root, key, mode, member, gate, int64(req.Bounds.FileBytes), raw, emit)
		}, func(epoch rebuildCheckoutInputs, raw *int64, emit func(rebuildSidecarRow) error) (string, error) {
			return captureRebuildCheckoutFacts(ctx, src, epoch, true, idx.config.Coverage.IsEnabled("modules"), raw, emit)
		}, func(lease string, bound rebuildBuildRequest, inputs []rebuildInput) error {
			return runRebuildBuild(ctx, exe, args, lease, headDir, bound, idx.config, inputs, publish)
		})
}

// freezeRebuildBuildConfig captures the config and effective synthesis setting
// before the owner binds the recipe. The child never re-evaluates the override.
func (idx *Indexer) freezeRebuildBuildConfig(req *rebuildBuildRequest) error {
	cfg, err := json.Marshal(idx.config)
	if err != nil {
		return err
	}
	req.Config = cfg
	req.ExternalCallsEnabled = idx.externalCallSynthesisEnabled()
	return nil
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
	if err := req.checkTrackedRepos(); err != nil {
		return rebuildBuildRequest{}, err
	}
	if req.Witness.Recipe == (store_pebble.HeadDigest{}) || req.BaseRecipe != (store_pebble.HeadDigest{}) ||
		req.Witness.FrozenInputs != (store_pebble.HeadDigest{}) || req.Witness.UsesGitIgnorePolicy || src == nil || req.Root != src.FrozenRoot().Name() {
		return rebuildBuildRequest{}, fmt.Errorf("%w: capture needs an unbound recipe and the enumerating root", errRebuildBuildWire)
	}
	threshold, err := req.recipeCloneThreshold()
	if err != nil {
		return rebuildBuildRequest{}, err
	}
	req.SkipPlaceholderSources = os.Getenv("GORTEX_RESOLVE_FROM_RECONCILE") == "0"
	// D2 keeps providers out of the build even if the owner uses them later.
	req.ProvidersOff = true
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
		if err == nil && (remaining < 0 || remaining > rebuildCppRawLimit) {
			return fmt.Errorf("%w: raw capture allowance for %q: %w", errRebuildInputSidecar, rebuildInputSidecarName, errRebuildCppCaptureLimit)
		}
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
	req.Witness.Recipe = req.projectRecipe(threshold)
	return req, nil
}
