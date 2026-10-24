package indexer

import (
	"path"
	"regexp"
	"strings"

	"github.com/pelletier/go-toml/v2"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/modules"
	"github.com/zzet/gortex/internal/resolver"
)

var pythonDistributionSeparators = regexp.MustCompile(`[-_.]+`)

func normalizePythonDistribution(name string) string {
	return pythonDistributionSeparators.ReplaceAllString(strings.ToLower(name), "-")
}

// pythonPackageLookup is built only after a non-stdlib Python import is found.
// Providers require indexed root/src modules or regular packages; consumers also
// block foreign bindings with their own indexed namespace directories.
func (mi *MultiIndexer) pythonPackageLookup() *resolver.PythonPackageLookup {
	roots := mi.AllMetadata()
	if len(roots) < 2 {
		return nil
	}
	type entry struct {
		name string
		deps map[string]bool
	}
	entries := map[string]*entry{}
	for repo, meta := range roots {
		if meta == nil || meta.RootPath == "" {
			continue
		}
		tree := mi.manifestTreeFor(repo, meta.RootPath)
		src, _ := tree.readFile("pyproject.toml")
		var manifest struct {
			Project struct{ Name string } `toml:"project"`
			Tool    struct {
				Poetry struct {
					Name  string
					Group map[string]struct{ Dependencies map[string]any } `toml:"group"`
				} `toml:"poetry"`
			} `toml:"tool"`
		}
		// A malformed manifest supplies no identity or dependencies.
		if len(src) > 0 && toml.Unmarshal(src, &manifest) != nil {
			continue
		}
		name := manifest.Project.Name
		if name == "" {
			name = manifest.Tool.Poetry.Name
		}
		deps := map[string]bool{}
		specs := modules.ParsePyProject(src)
		for _, group := range manifest.Tool.Poetry.Group {
			for dep := range group.Dependencies {
				if dep != "python" {
					deps[normalizePythonDistribution(dep)] = true
				}
			}
		}
		for _, file := range tree.matchFiles("requirements*.txt") {
			src, _ := tree.readFile(file)
			specs = append(specs, modules.ParseRequirementsTxt(src)...)
		}
		for _, dep := range specs {
			deps[normalizePythonDistribution(dep.Path)] = true
		}
		entries[repo] = &entry{name: normalizePythonDistribution(name), deps: deps}
	}
	// Only declared distributions can supply providers. Discover top-level names
	// from their root/src directory entries, then probe exact indexed node IDs.
	packages := map[string]map[string][]graph.FileNodeIdentity{}
	for repo, pkg := range entries {
		declared := false
		for caller, consumer := range entries {
			if caller != repo && pkg.name != "" && consumer.deps[pkg.name] {
				declared = true
				break
			}
		}
		if !declared {
			continue
		}
		tree := mi.manifestTreeFor(repo, roots[repo].RootPath)
		for _, dir := range []string{"", "src"} {
			files := tree.matchFiles(path.Join(dir, "*.py"))
			for _, pkg := range tree.matchDirs(path.Join(dir, "*")) {
				files = append(files, pkg+"/__init__.py")
			}
			for _, file := range files {
				name := strings.TrimSuffix(strings.TrimPrefix(file, dir+"/"), "/__init__.py")
				if dir == "" && pythonRootToolingFile(name) {
					continue
				}
				top := strings.TrimSuffix(name, ".py")
				node := mi.graph.GetNode(repo + "/" + file)
				if node == nil || node.Kind != graph.KindFile || node.RepoPrefix != repo {
					continue
				}
				if packages[top] == nil {
					packages[top] = map[string][]graph.FileNodeIdentity{}
				}
				packages[top][repo] = append(packages[top][repo], graph.FileNodeIdentity{
					ID: node.ID, FilePath: node.FilePath, RepoPrefix: repo, WorkspaceID: node.WorkspaceID,
				})
			}
		}
	}
	if len(packages) == 0 {
		return nil
	}
	localTops := map[string]map[string]bool{}
	return &resolver.PythonPackageLookup{
		HasTop: func(top string) bool { return len(packages[top]) > 0 },
		Providers: func(caller, top string) map[string][]graph.FileNodeIdentity {
			consumer := entries[caller]
			if consumer == nil || len(packages[top]) == 0 {
				return nil
			}
			local, seen := localTops[caller]
			if !seen {
				local = map[string]bool{}
				// Record both the literal top-level name and the src-layout name:
				// a root `src` package and src/<pkg> must each stay local, and an
				// extra local name only blocks a cross-repo link.
				for file := range graph.FileNodeIdentitiesSeq(mi.graph, []string{caller}) {
					if !strings.HasSuffix(file.FilePath, ".py") {
						continue
					}
					rel := strings.TrimPrefix(file.FilePath, caller+"/")
					for _, path := range []string{rel, strings.TrimPrefix(rel, "src/")} {
						name, _, _ := strings.Cut(path, "/")
						local[strings.TrimSuffix(name, ".py")] = true
					}
				}
				localTops[caller] = local
			}
			if local[top] {
				return nil
			}
			providers := map[string][]graph.FileNodeIdentity{}
			for repo, files := range packages[top] {
				if repo != caller && consumer.deps[entries[repo].name] {
					providers[repo] = files
				}
			}
			return providers
		},
	}
}

func pythonRootToolingFile(name string) bool {
	switch name {
	case "setup.py", "conftest.py", "noxfile.py", "fabfile.py", "manage.py":
		return true
	}
	return false
}
