package indexer

import (
	"bytes"
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
		dir        string
		exports    *packageExports
		declared   map[string]string
		hasExports bool
		types      string
		main       string
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
					Name    string          `json:"name"`
					Exports json.RawMessage `json:"exports"`
					Types   string          `json:"types"`
					Main    string          `json:"main"`
				}
				if json.Unmarshal(src, &manifest) != nil || manifest.Name == "" {
					continue
				}
				exports := parsePackageExports(src)
				declared := declaredNpmExports(manifest.Exports)
				// Keep unsupported and blocked keys in the target map so an
				// exact key cannot fall through to a broader wildcard target.
				if exports != nil {
					for key := range declared {
						if _, ok := exports.subpaths[key]; !ok {
							exports.subpaths[key] = ""
						}
					}
				}
				packages[manifest.Name] = append(packages[manifest.Name], entry{
					dir: path.Join(prefix, dir), exports: exports, declared: declared, hasExports: hasPackageExports(manifest.Exports), types: manifest.Types, main: manifest.Main,
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
			var target string
			if pkg.exports != nil {
				target = resolveExportsSubpath(pkg.exports.subpaths, sub)
			}
			declaration := resolveExportsSubpath(pkg.declared, sub)
			if pkg.hasExports && declaration != "declared" {
				continue
			}
			// A declared entry with unsupported conditions keeps legacy
			// candidates; a supported target remains authoritative.
			paths := []string{target}
			if !pkg.hasExports || target == "" {
				if sub == "" {
					paths = append(paths, pkg.types, pkg.main, "index")
				} else {
					paths = append(paths, sub)
				}
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

// declaredNpmExports retains entry keys independently of supported conditions.
// Values are matcher markers, not paths, so wildcard substitution leaves them intact.
func declaredNpmExports(raw json.RawMessage) map[string]string {
	var asString string
	if json.Unmarshal(raw, &asString) == nil && hasPackageExports(raw) {
		return map[string]string{".": "declared"}
	}
	var entries map[string]json.RawMessage
	if json.Unmarshal(raw, &entries) != nil {
		return nil
	}
	if !hasSubpathKeys(entries) {
		return map[string]string{".": "declared"}
	}
	declared := make(map[string]string, len(entries))
	for key, target := range entries {
		declared[key] = "declared"
		if bytes.Equal(bytes.TrimSpace(target), []byte("null")) {
			declared[key] = "blocked"
		}
	}
	return declared
}

// hasPackageExports reports whether package.json declares exports. Node treats
// a null exports field like an absent one and falls back to main.
func hasPackageExports(raw json.RawMessage) bool {
	s := bytes.TrimSpace(raw)
	return len(s) > 0 && !bytes.Equal(s, []byte("null"))
}
