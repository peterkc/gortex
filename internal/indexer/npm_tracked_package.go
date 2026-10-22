package indexer

import (
	"encoding/json"
	"path"
	"sync"

	"github.com/zzet/gortex/internal/modules"
	"github.com/zzet/gortex/internal/resolver"
)

// npmPackageLookup lazily snapshots manifest evidence once per resolver. A new
// resolver captures fresh roots and reads fresh manifests after edits or track/untrack.
func (mi *MultiIndexer) npmPackageLookup() resolver.NpmPackageLookup {
	type entry struct {
		dir     string
		exports *packageExports
		types   string
		main    string
	}
	roots := map[string]string{}
	for prefix, meta := range mi.AllMetadata() {
		if meta != nil && meta.RootPath != "" {
			roots[prefix] = meta.RootPath
		}
	}
	if len(roots) == 0 {
		return nil
	}
	packages := map[string][]entry{}
	var once sync.Once
	build := func() {
		for prefix, root := range roots {
			rootSrc, _ := readDiskFile(joinPath(root, "package.json"))
			workspace := &modules.WorkspaceManifest{Ecosystem: modules.WorkspaceNPM, Patterns: modules.ParsePackageJSONWorkspaces(rootSrc)}
			if len(workspace.Patterns) == 0 {
				src, _ := readDiskFile(joinPath(root, "pnpm-workspace.yaml"))
				workspace.Ecosystem = modules.WorkspacePnpm
				workspace.Patterns = modules.ParsePnpmWorkspaceYAML(src)
			}
			dirs := []string{"."}
			for _, member := range modules.ResolveWorkspaceMembers(root, workspace) {
				dirs = append(dirs, member.Dir)
			}
			for _, dir := range dirs {
				if path.IsAbs(dir) || hasDotDotSegment(dir) {
					continue
				}
				src := rootSrc
				if dir != "." {
					src, _ = readDiskFile(joinPath(root, joinRel(dir, "package.json")))
				}
				var manifest struct {
					Name  string `json:"name"`
					Types string `json:"types"`
					Main  string `json:"main"`
				}
				if json.Unmarshal(src, &manifest) != nil || manifest.Name == "" {
					continue
				}
				packages[manifest.Name] = append(packages[manifest.Name], entry{
					dir: path.Join(prefix, dir), exports: parsePackageExports(src), types: manifest.Types, main: manifest.Main,
				})
			}
		}
	}
	return func(specifier string) map[string][]string {
		once.Do(build)
		name, sub := splitPackageSpecifier(specifier)
		if name == "" || hasDotDotSegment(sub) || len(packages[name]) == 0 {
			return nil
		}
		targets := map[string][]string{}
		for _, pkg := range packages[name] {
			targets[pkg.dir] = nil
			var paths []string
			if pkg.exports != nil {
				paths = append(paths, resolveExportsSubpath(pkg.exports.subpaths, sub))
			}
			if sub == "" {
				paths = append(paths, pkg.types, pkg.main, "index")
			} else if pkg.exports == nil {
				paths = append(paths, sub)
			}
			for _, target := range paths {
				// Never let manifest paths bind another package/repository.
				if target == "" || path.IsAbs(target) || hasDotDotSegment(target) {
					continue
				}
				targets[pkg.dir] = append(targets[pkg.dir], path.Join(pkg.dir, target))
			}
		}
		return targets
	}
}
