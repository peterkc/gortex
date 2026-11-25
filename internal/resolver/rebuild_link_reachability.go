package resolver

import (
	"context"
	"fmt"
	"strings"

	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/graph/store_pebble"
)

const (
	rebuildDirectImportRows           = 4 << 10
	rebuildDirectImportBucketBytes    = 16 << 20
	rebuildDirectImportDirectoryBytes = 1 << 20
)

// directImportDirectories projects the native main pass's direct reachability
// for one caller file. The page owner caches this completed result only until
// that page's edge moves are applied. Directory and npm evidence are frozen;
// direct adjacency and actual target placement are read from current rows.
func (f *rebuildLinkFrontier) directImportDirectories(ctx context.Context, file string) (map[string]struct{}, error) {
	if err := ctx.Err(); err != nil || file == "" {
		return nil, err
	}
	if len(file) > rebuildDirectImportDirectoryBytes {
		return nil, fmt.Errorf("%w: direct import caller path", store_pebble.ErrRebuildLinkInputSize)
	}
	reachable := make(map[string]struct{})
	remaining := rebuildDirectImportDirectoryBytes
	add := func(dir string) error {
		if _, ok := reachable[dir]; ok {
			return nil
		}
		if len(reachable) == rebuildDirectImportRows || len(dir) > remaining {
			return fmt.Errorf("%w: direct import directory result", store_pebble.ErrRebuildLinkInputSize)
		}
		remaining -= len(dir)
		reachable[strings.Clone(dir)] = struct{}{}
		return nil
	}
	if err := add(filePathDir(file)); err != nil {
		return nil, err
	}

	// Count every visited directory identity, including rejected bare-JS/TS
	// files and repeated bucket probes. No candidate list is retained.
	bucketRows, bucketBytes := 0, 0
	importedDir := func(spec string) (string, error) {
		bare := isJSTSBareSpecifier(file, spec)
		if bare {
			if f.imports.externalNpmDependency == nil {
				return "", fmt.Errorf("%w: direct import %q in %q needs checked external npm dependency result", errRebuildMainRule, spec, file)
			}
			module, _ := splitImportSpecSymbol(spec)
			external, err := f.imports.externalNpmDependency(file, module)
			if err != nil || external {
				return "", err
			}
		}
		dirs, err := f.importDirectories(ctx)
		if err != nil {
			return "", err
		}
		var dir string
		pick := func(n graph.FileNodeIdentity) (bool, error) {
			bucketRows++
			bucketBytes += len(n.ID) + len(n.FilePath) + len(n.RepoPrefix) + len(n.WorkspaceID) + len(n.Language)
			if bucketRows > rebuildDirectImportRows || bucketBytes > rebuildDirectImportBucketBytes {
				return false, fmt.Errorf("%w: direct import directory bucket", store_pebble.ErrRebuildLinkInputSize)
			}
			if bare && !isJSTSDirEntryPoint(n.FilePath) {
				return true, nil
			}
			dir = filePathDir(n.FilePath)
			return false, nil
		}
		if err := dirs.VisitExactDirectory(ctx, spec, pick); err != nil || dir != "" {
			return dir, err
		}
		if last := spec[strings.LastIndexByte(spec, '/')+1:]; last != "" {
			if err := dirs.VisitLastDirectoryComponent(ctx, last, pick); err != nil {
				return "", err
			}
		}
		return dir, nil
	}
	err := f.view.VisitFileImportTargets(ctx, file, func(targetID, targetFile string) error {
		var dir string
		switch {
		case graph.IsUnresolvedTarget(targetID):
			if spec, ok := strings.CutPrefix(graph.UnresolvedName(targetID), "import::"); ok {
				var err error
				dir, err = importedDir(spec)
				if err != nil {
					return err
				}
			}
		case strings.HasPrefix(targetID, "external::"):
		default:
			if targetFile != "" {
				dir = filePathDir(targetFile)
			}
		}
		if dir != "" {
			return add(dir)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return reachable, nil
}
