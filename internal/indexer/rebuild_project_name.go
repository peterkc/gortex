package indexer

import (
	"context"
	"errors"
	"fmt"
	"io/fs"

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
	entry, err := src.ResolveFrozenRootLink(ctx, input.Path, func(target string) error {
		if int64(len(target)) != input.Size || rebuildDigest(blake3.Sum256([]byte(target))) != input.Content {
			return ErrBuildInputChanged
		}
		return nil
	})
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) || errors.Is(err, source.ErrFrozenRootChanged) {
			err = errors.Join(ErrBuildInputChanged, err)
		}
		return nil, fmt.Errorf("%w: %s: %w", errRebuildProjectNameInput, input.Path, err)
	}
	data, err := entry.Read(ctx, min(*rawRemaining, rebuildCppEntryLimit))
	if err != nil {
		if errors.Is(err, source.ErrFrozenRootChanged) {
			err = errors.Join(ErrBuildInputChanged, err)
		} else if errors.Is(err, source.ErrRegularFileTooLarge) {
			err = errors.Join(errRebuildCppCaptureLimit, err)
		}
		return nil, fmt.Errorf("%w: %s: %w", errRebuildProjectNameInput, input.Path, err)
	}
	*rawRemaining -= int64(len(data))
	if err := emit(rebuildSidecarRow{Kind: rebuildFollowedManifest, Path: []byte(input.Path), Present: true, Digest: blake3.Sum256(data)}); err != nil {
		return nil, err
	}
	return data, nil
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
