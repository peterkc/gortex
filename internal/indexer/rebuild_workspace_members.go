package indexer

import (
	"context"
	"fmt"
	"path"
	"path/filepath"
	"strings"

	"github.com/zzet/gortex/internal/modules"
)

// newRebuildWorkspaceMembers freezes exact member roots for the private
// unprefixed graph. A manifest's existence suffices; never read its body.
func newRebuildWorkspaceMembers(ctx context.Context, g *rebuildWorkspaceGlob, manifest *modules.WorkspaceManifest) (map[string]struct{}, error) {
	return collectRebuildWorkspaceMembers(ctx, g, manifest, nil)
}

func collectRebuildWorkspaceMembers(ctx context.Context, g *rebuildWorkspaceGlob, manifest *modules.WorkspaceManifest,
	visit func(modules.WorkspaceMember),
) (map[string]struct{}, error) {
	members := make(map[string]struct{})
	if manifest == nil {
		return members, ctx.Err()
	}
	work, left := rebuildNpmFactBytes, rebuildNpmFactBytes
	expand := func(pattern string) ([]string, error) {
		if len(pattern) >= work {
			return nil, fmt.Errorf("%w: workspace pattern work cap: %w", errRebuildResolverNamespace, errRebuildCppCaptureLimit)
		}
		work -= len(pattern) + 1
		pattern = strings.TrimSuffix(filepath.ToSlash(strings.TrimSpace(pattern)), "/")
		if pattern == "" || pattern == "." {
			return nil, nil
		}
		literal := !strings.ContainsAny(pattern, "*?[")
		dirs, err := g.matchDirsWithWork(ctx, pattern, left, &work, literal)
		if err != nil {
			return nil, err
		}
		if literal && len(dirs) == 1 {
			// Native returns the literal pattern's spelling after probing its
			// cleaned name. Exclusions compare that spelling, including '..'.
			dirs[0] = pattern
		}
		for _, dir := range dirs {
			if len(dir) >= left {
				return nil, fmt.Errorf("%w: workspace member cap: %w", errRebuildResolverNamespace, errRebuildCppCaptureLimit)
			}
			left -= len(dir) + 1
		}
		return dirs, nil
	}
	excluded := make(map[string]struct{})
	for _, pattern := range manifest.Exclude {
		dirs, err := expand(pattern)
		if err != nil {
			return nil, err
		}
		for _, dir := range dirs {
			excluded[dir] = struct{}{}
		}
	}
	name := "package.json"
	if manifest.Ecosystem == modules.WorkspaceCargo {
		name = rebuildCargoWorkspaceFile
	}
	for _, pattern := range manifest.Patterns {
		dirs, err := expand(pattern)
		if err != nil {
			return nil, err
		}
		for _, dir := range dirs {
			if _, skip := excluded[dir]; skip || dir == "." {
				continue
			}
			if _, seen := members[dir]; seen {
				continue
			}
			key := path.Join(dir, name)
			ok, err := g.ns.probeFile(key)
			if err != nil {
				return nil, fmt.Errorf("workspace member manifest %q: %w", key, err)
			}
			if ok {
				members[dir] = struct{}{}
				if visit != nil {
					visit(modules.WorkspaceMember{Dir: dir, ManifestPath: key})
				}
			}
		}
	}
	return members, ctx.Err()
}

func rebuildWorkspaceMember(members map[string]struct{}, file string) string {
	// Native compares its cleaned *graph* path to the literal member dirs.
	// Test only ancestors to pick the longest match without a member scan.
	for dir := path.Clean(file); dir != "." && dir != "/"; dir = path.Dir(dir) {
		if _, ok := members[dir]; ok {
			return dir
		}
	}
	return ""
}
