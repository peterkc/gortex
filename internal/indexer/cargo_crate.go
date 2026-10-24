package indexer

import (
	"path"
	"strings"
	"sync"

	"github.com/pelletier/go-toml/v2"
	"github.com/zzet/gortex/internal/modules"
	"github.com/zzet/gortex/internal/resolver"
)

func cargoTable(m map[string]any, key string) map[string]any {
	table, _ := m[key].(map[string]any)
	return table
}

func readCargoManifest(tree manifestTree, dir string) map[string]any {
	src, ok := tree.readFile(path.Join(dir, "Cargo.toml"))
	if !ok {
		return nil
	}
	var manifest map[string]any
	if toml.Unmarshal(src, &manifest) != nil {
		return nil
	}
	return manifest
}

// cargoCrateLookup snapshots root/member manifests once per cross-repo pass.
// Only explicit workspace member globs are expanded; Cargo is never executed.
func (mi *MultiIndexer) cargoCrateLookup() resolver.CargoCrateLookup {
	type crate struct {
		dir, name, root string
		deps            map[string]string
		imports         map[string][]resolver.CargoCrate
	}
	roots := map[string]manifestTree{}
	for prefix, meta := range mi.AllMetadata() {
		if meta != nil && meta.RootPath != "" {
			tree := mi.manifestTreeFor(prefix, meta.RootPath)
			if tree.isFile("Cargo.toml") {
				roots[prefix] = tree
			}
		}
	}
	if len(roots) == 0 {
		return nil
	}
	owners := map[string]*crate{}
	packages := map[string][]*crate{}
	ownerCache := map[string]*crate{}
	var ownerMu sync.Mutex
	var once sync.Once
	return func(file, name string) []resolver.CargoCrate {
		once.Do(func() {
			for prefix, tree := range roots {
				manifest := readCargoManifest(tree, ".")
				src, _ := tree.readFile("Cargo.toml")
				members, exclude := modules.ParseCargoWorkspace(src)
				dirs := []string{"."}
				skip := map[string]bool{".": true}
				for _, pattern := range exclude {
					for _, dir := range tree.matchDirs(pattern) {
						skip[dir] = true
					}
				}
				for _, pattern := range members {
					for _, dir := range tree.matchDirs(pattern) {
						if !path.IsAbs(dir) && !hasDotDotSegment(dir) && !skip[dir] && tree.isFile(path.Join(dir, "Cargo.toml")) {
							dirs = append(dirs, dir)
							skip[dir] = true
						}
					}
				}
				inherited := cargoTable(cargoTable(manifest, "workspace"), "dependencies")
				for _, dir := range dirs {
					m := manifest
					if dir != "." {
						m = readCargoManifest(tree, dir)
					}
					if m == nil {
						continue
					}
					pkg, _ := cargoTable(m, "package")["name"].(string)
					if pkg == "" {
						continue
					}
					lib := cargoTable(m, "lib")
					crateName, _ := lib["name"].(string)
					if crateName == "" {
						crateName = strings.ReplaceAll(pkg, "-", "_")
					}
					libPath, _ := lib["path"].(string)
					if libPath == "" {
						libPath = "src/lib.rs"
					}
					if path.IsAbs(libPath) || hasDotDotSegment(libPath) {
						continue
					}
					deps := map[string]string{}
					tables := []map[string]any{m}
					for _, target := range cargoTable(m, "target") {
						if table, ok := target.(map[string]any); ok {
							tables = append(tables, table)
						}
					}
					for _, table := range tables {
						for _, key := range []string{"dependencies", "dev-dependencies", "build-dependencies"} {
							for alias, value := range cargoTable(table, key) {
								dep, isTable := value.(map[string]any)
								if isTable && dep["workspace"] == true {
									value = inherited[alias]
									dep, isTable = value.(map[string]any)
								}
								_, isVersion := value.(string)
								if !isTable && !isVersion {
									continue
								}
								target := alias
								if packageName, ok := dep["package"].(string); ok {
									target = packageName
								}
								// Conflicting target-specific aliases cannot identify one package.
								if old, exists := deps[alias]; exists && old != target {
									target = ""
								}
								deps[alias] = target
							}
						}
					}
					c := &crate{dir: path.Join(prefix, dir), name: crateName, root: path.Join(prefix, dir, libPath), deps: deps}
					if owners[c.dir] == nil {
						owners[c.dir] = c
						packages[pkg] = append(packages[pkg], c)
					}
				}
			}
			for _, owner := range owners {
				owner.imports = map[string][]resolver.CargoCrate{}
				for alias, pkg := range owner.deps {
					for _, provider := range packages[pkg] {
						name := provider.name
						if alias != pkg {
							name = strings.ReplaceAll(alias, "-", "_")
						}
						owner.imports[name] = append(owner.imports[name], resolver.CargoCrate{Root: provider.root, Name: provider.name})
					}
				}
			}
		})
		// Cache the nearest manifest boundary, including nonmembers and excludes.
		// A nested crate must never borrow an ancestor package's dependencies.
		ownerMu.Lock()
		defer ownerMu.Unlock()
		var prefix string
		for dir := path.Dir(file); dir != "."; dir = path.Dir(dir) {
			if roots[dir] != nil {
				prefix = dir
				break
			}
		}
		if prefix == "" {
			return nil
		}
		var owner *crate
		var visited []string
		for dir := path.Dir(file); ; dir = path.Dir(dir) {
			if cached, ok := ownerCache[dir]; ok {
				owner = cached
				break
			}
			visited = append(visited, dir)
			if c := owners[dir]; c != nil {
				owner = c
				break
			}
			rel := strings.TrimPrefix(dir, prefix+"/")
			if dir == prefix {
				rel = "."
			}
			if roots[prefix].isFile(path.Join(rel, "Cargo.toml")) || dir == prefix {
				break // Unknown, malformed, or unreadable manifest: no ownership evidence.
			}
		}
		for _, dir := range visited {
			ownerCache[dir] = owner
		}
		if owner == nil {
			return nil
		}
		return owner.imports[name]
	}
}
