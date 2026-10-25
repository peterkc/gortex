package indexer

import (
	"context"
	"fmt"
	"iter"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	"github.com/zzet/gortex/internal/config"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/graph/store_sqlite"
	"github.com/zzet/gortex/internal/parser/languages"
	"github.com/zzet/gortex/internal/search"
)

func TestMultiIndexer_TrackedPythonImport(t *testing.T) {
	for _, backend := range []string{"memory", "sqlite"} {
		for _, tc := range []struct {
			name, manifest, requirements, entry, statement, providerName, workspace, localFile, extraLocal string
			duplicate, cloneMissing, resolves                                                              bool
		}{
			{name: "src module distribution differs", manifest: "[project]\ndependencies = ['pillow >= 1']", entry: "src/PIL/image.py", statement: "import PIL.image", resolves: true},
			{name: "package init", manifest: "[project]\ndependencies = ['pillow']", entry: "PIL/image/__init__.py", statement: "import PIL.image", resolves: true},
			{name: "requirements", requirements: "PILLOW[extra]>=1; python_version > '3'\n", entry: "src/PIL/image.py", statement: "import PIL.image", resolves: true},
			{name: "from package", manifest: "[project.optional-dependencies]\nimages = ['pillow']", entry: "src/PIL/__init__.py", statement: "from PIL import image", resolves: true},
			{name: "poetry dependency", manifest: "[tool.poetry.dependencies]\npillow = {path = '../provider'}", entry: "PIL/image.py", statement: "from PIL.image import f", resolves: true},
			{name: "poetry group normalized", manifest: "[tool.poetry.group.dev.dependencies]\nimage-dist = '*'", providerName: "Image._Dist", entry: "PIL/image.py", statement: "import PIL.image", resolves: true},
			{name: "top level module", manifest: "[project]\ndependencies = ['pillow']", entry: "image.py", statement: "import image", resolves: true},
			{name: "consumer namespace root", manifest: "[project]\ndependencies = ['pillow']", entry: "tests/helpers.py", statement: "import tests.helpers", localFile: "tests/helpers.py"},
			{name: "consumer namespace src", manifest: "[project]\ndependencies = ['pillow']", entry: "tests/helpers.py", statement: "import tests.helpers", localFile: "src/tests/helpers.py"},
			{name: "consumer root src package", manifest: "[project]\ndependencies = ['pillow']", entry: "src/foo.py", statement: "import src.foo", localFile: "src/__init__.py"},
			{name: "consumer src init with src namespace", manifest: "[project]\ndependencies = ['pillow']", entry: "tests/helpers.py", statement: "import tests.helpers", localFile: "src/__init__.py", extraLocal: "src/tests/helpers.py"},
			{name: "stdlib package", manifest: "[project]\ndependencies = ['pillow']", entry: "json/__init__.py", statement: "import json"},
			{name: "root setup script", manifest: "[project]\ndependencies = ['pillow']", entry: "setup.py", statement: "import setup"},
			{name: "root conftest script", manifest: "[project]\ndependencies = ['pillow']", entry: "conftest.py", statement: "import conftest"},
			{name: "root noxfile script", manifest: "[project]\ndependencies = ['pillow']", entry: "noxfile.py", statement: "import noxfile"},
			{name: "root fabfile script", manifest: "[project]\ndependencies = ['pillow']", entry: "fabfile.py", statement: "import fabfile"},
			{name: "root manage script", manifest: "[project]\ndependencies = ['pillow']", entry: "manage.py", statement: "import manage"},
			{name: "malformed consumer", manifest: "[project", entry: "src/PIL/image.py", statement: "import PIL.image"},
			{name: "undeclared", entry: "src/PIL/image.py", statement: "import PIL.image"},
			{name: "same named undeclared repo", manifest: "[project]\ndependencies = ['other']", providerName: "PIL", entry: "src/PIL/image.py", statement: "import PIL.image"},
			{name: "ambiguous", manifest: "[project]\ndependencies = ['pillow']", duplicate: true, entry: "src/PIL/image.py", statement: "import PIL.image"},
			{name: "ambiguous before module probe", manifest: "[project]\ndependencies = ['pillow']", duplicate: true, cloneMissing: true, entry: "src/PIL/image.py", statement: "import PIL.image"},
			{name: "workspace refusal", manifest: "[project]\ndependencies = ['pillow']", workspace: "private", entry: "src/PIL/image.py", statement: "import PIL.image"},
		} {
			t.Run(backend+"/"+tc.name, func(t *testing.T) {
				provider, consumer := t.TempDir(), t.TempDir()
				name := tc.providerName
				if name == "" {
					name = "Pillow"
				}
				writeWorkspaceFile(t, provider, "pyproject.toml", "[project]\nname = '"+name+"'\n")
				packageRoot := "PIL/__init__.py"
				if strings.HasPrefix(tc.entry, "src/") {
					packageRoot = "src/" + packageRoot
				}
				writeWorkspaceFile(t, provider, packageRoot, "")
				if strings.HasPrefix(tc.entry, "tests/") {
					writeWorkspaceFile(t, provider, "tests/__init__.py", "")
				}
				if tc.statement == "import src.foo" {
					writeWorkspaceFile(t, provider, "src/__init__.py", "")
				}
				if tc.localFile != "" {
					writeWorkspaceFile(t, consumer, tc.localFile, "def local(): pass\n")
				}
				if tc.extraLocal != "" {
					writeWorkspaceFile(t, consumer, tc.extraLocal, "def local(): pass\n")
				}
				writeWorkspaceFile(t, provider, tc.entry, "def f(): pass\n")
				writeWorkspaceFile(t, consumer, "pyproject.toml", tc.manifest)
				writeWorkspaceFile(t, consumer, "requirements-dev.txt", tc.requirements)
				writeWorkspaceFile(t, consumer, "app.py", tc.statement+"\n")
				ws := tc.workspace
				if ws == "" {
					ws = "shared"
				}
				repos := []config.RepoEntry{{Path: consumer, Name: "consumer", Workspace: "shared"}, {Path: provider, Name: "provider", Workspace: ws}}
				if tc.duplicate {
					clone := t.TempDir()
					writeWorkspaceFile(t, clone, "pyproject.toml", "[tool.poetry]\nname = 'pillow'\n")
					writeWorkspaceFile(t, clone, "src/PIL/__init__.py", "")
					if !tc.cloneMissing {
						writeWorkspaceFile(t, clone, tc.entry, "def f(): pass\n")
					}
					repos = append(repos, config.RepoEntry{Path: clone, Name: "clone", Workspace: "shared"})
				}
				cfgPath := filepath.Join(t.TempDir(), "config.yaml")
				gc := &config.GlobalConfig{Repos: repos}
				gc.SetConfigPath(cfgPath)
				require.NoError(t, gc.Save())
				cm, err := config.NewConfigManager(cfgPath)
				require.NoError(t, err)
				var g graph.Store = graph.New()
				if backend == "sqlite" {
					db, err := store_sqlite.Open(filepath.Join(t.TempDir(), "graph.sqlite"))
					require.NoError(t, err)
					t.Cleanup(func() { require.NoError(t, db.Close()) })
					g = db
				}
				reg := newTestRegistry()
				reg.Register(languages.NewPythonExtractor())
				mi := NewMultiIndexer(g, reg, search.NewNull(), cm, zap.NewNop())
				_, err = mi.IndexAll()
				require.NoError(t, err)
				var imports []*graph.Edge
				for _, e := range g.GetOutEdges("consumer/app.py") {
					if e.Kind == graph.EdgeImports {
						imports = append(imports, e)
					}
				}
				require.Len(t, imports, 1)
				require.Equal(t, tc.resolves, imports[0].CrossRepo, "target: %s", imports[0].To)
				if tc.resolves {
					require.Equal(t, "provider/"+tc.entry, imports[0].To)
				} else {
					require.NotEqual(t, "provider/"+tc.entry, imports[0].To)
					require.NotEqual(t, "clone/"+tc.entry, imports[0].To)
				}
			})
		}
	}
}

func TestPythonPackageLookupNoProviders(t *testing.T) {
	for _, count := range []int{1, 2} {
		t.Run(fmt.Sprint(count), func(t *testing.T) {
			mi := &MultiIndexer{graph: graph.New(), repos: map[string]*RepoMetadata{}}
			for i := 0; i < count; i++ {
				mi.repos[fmt.Sprint(i)] = &RepoMetadata{RootPath: t.TempDir()}
			}
			require.Nil(t, mi.pythonPackageLookup())
		})
	}
}

type pythonLookupScanStore struct {
	graph.Store
	globalScans int
	importScans int
}

func (s *pythonLookupScanStore) FileNodeIdentitiesSeq(repos []string) iter.Seq[graph.FileNodeIdentity] {
	if repos == nil {
		s.globalScans++
	}
	return graph.FileNodeIdentitiesSeq(s.Store, repos)
}

func TestPythonPackageLookupAvoidsGlobalFileScan(t *testing.T) {
	provider, consumer := t.TempDir(), t.TempDir()
	writeWorkspaceFile(t, provider, "pyproject.toml", "[project]\nname = 'pillow'\n")
	writeWorkspaceFile(t, provider, "src/PIL/__init__.py", "")
	writeWorkspaceFile(t, consumer, "pyproject.toml", "[project]\ndependencies = ['pillow']\n")
	g := &pythonLookupScanStore{Store: graph.New()}
	g.AddNode(&graph.Node{ID: "provider/src/PIL/__init__.py", FilePath: "provider/src/PIL/__init__.py", Kind: graph.KindFile, RepoPrefix: "provider"})
	mi := &MultiIndexer{graph: g, repos: map[string]*RepoMetadata{
		"consumer": {RootPath: consumer}, "provider": {RootPath: provider},
	}}
	lookup := mi.pythonPackageLookup()
	require.NotNil(t, lookup)
	require.Len(t, lookup.Providers("consumer", "PIL"), 1)
	require.Zero(t, g.globalScans)
}

func TestPythonPackageLookupUsesInstalledManifestTree(t *testing.T) {
	for _, entry := range []string{"PIL.py", "PIL/__init__.py", "src/PIL.py", "src/PIL/__init__.py"} {
		for _, mode := range []string{"pyproject", "requirements", "layout", "undeclared", "missing_layout"} {
			t.Run(entry+"/"+mode, func(t *testing.T) {
				provider, consumer, providerSnapshot, consumerSnapshot := t.TempDir(), t.TempDir(), t.TempDir(), t.TempDir()
				providerName, checkoutDeps := "pillow", "[project]\ndependencies = ['pillow']\n"
				if mode == "pyproject" {
					providerName, checkoutDeps = "wrong-checkout", ""
				}
				snapshotDeps := "[project]\ndependencies = ['pillow']\n"
				if mode == "requirements" || mode == "undeclared" {
					snapshotDeps = ""
				}
				if mode == "requirements" {
					checkoutDeps = ""
					writeWorkspaceFile(t, consumer, "requirements-dev.txt", "other\n")
					writeWorkspaceFile(t, consumerSnapshot, "requirements-dev.txt", "pillow >= 1\n")
				}
				writeWorkspaceFile(t, provider, "pyproject.toml", "[project]\nname = '"+providerName+"'\n")
				writeWorkspaceFile(t, consumer, "pyproject.toml", checkoutDeps)
				writeWorkspaceFile(t, providerSnapshot, "pyproject.toml", "[project]\nname = 'pillow'\n")
				writeWorkspaceFile(t, consumerSnapshot, "pyproject.toml", snapshotDeps)
				if mode != "layout" {
					writeWorkspaceFile(t, provider, entry, "")
				}
				if mode != "missing_layout" {
					writeWorkspaceFile(t, providerSnapshot, entry, "")
				}
				g := graph.New()
				id := "provider/" + entry
				g.AddNode(&graph.Node{ID: id, FilePath: id, Kind: graph.KindFile, RepoPrefix: "provider"})
				providerIdx, consumerIdx := sideChannelIndexer(t, g, provider), sideChannelIndexer(t, g, consumer)
				providerIdx.SetContentSource(snapshotOf(t, providerSnapshot))
				consumerIdx.SetContentSource(snapshotOf(t, consumerSnapshot))
				mi := &MultiIndexer{graph: g,
					repos:    map[string]*RepoMetadata{"provider": {RootPath: provider}, "consumer": {RootPath: consumer}},
					indexers: map[string]*Indexer{"provider": providerIdx, "consumer": consumerIdx}}
				lookup := mi.pythonPackageLookup()
				if mode == "undeclared" || mode == "missing_layout" {
					require.Nil(t, lookup, "checkout-only declaration/layout must not supply a provider")
					return
				}
				require.NotNil(t, lookup)
				require.Equal(t, []graph.FileNodeIdentity{{ID: id, FilePath: id, RepoPrefix: "provider"}}, lookup.Providers("consumer", "PIL")["provider"])
			})
		}
	}
}

func (s *pythonLookupScanStore) EdgesLightSeq(kinds ...graph.EdgeKind) iter.Seq[*graph.Edge] {
	if len(kinds) == 1 && kinds[0] == graph.EdgeImports {
		s.importScans++
	}
	return graph.EdgesLightSeq(s.Store, kinds...)
}

func TestPythonAddedFileConsumerFrontier(t *testing.T) {
	g := &pythonLookupScanStore{Store: graph.New()}
	for _, row := range []struct{ file, target string }{
		{"consumer/a.py", "module::pypi:PIL"},
		{"consumer/b.py", "external::PIL.extra"},
		{"consumer/c.py", "unresolved::import::PIL.extra"},
		{"consumer/d.py", "module::pypi:other"},
		{"consumer/e.py", "provider/src/PIL/image.py"},
		{"consumer/f.rs", "external::PIL.extra"},
	} {
		g.AddEdge(&graph.Edge{From: row.file, FilePath: row.file, To: row.target, Kind: graph.EdgeImports})
	}
	require.Empty(t, pythonConsumerFilesForAddedFiles(g, nil))
	require.Zero(t, g.importScans)
	require.Equal(t, []string{"consumer/a.py", "consumer/b.py", "consumer/c.py"}, pythonConsumerFilesForAddedFiles(g, []string{"provider/src/PIL/extra.py"}))
	require.Equal(t, 1, g.importScans)
}

// A one-connection store cannot hydrate package identities under an open cursor.
type pythonCursorGuardStore struct {
	graph.Store
	t          *testing.T
	cursorOpen bool
}

func (s *pythonCursorGuardStore) EdgesLightSeq(kinds ...graph.EdgeKind) iter.Seq[*graph.Edge] {
	return func(yield func(*graph.Edge) bool) {
		s.cursorOpen = true
		defer func() { s.cursorOpen = false }()
		for edge := range graph.EdgesLightSeq(s.Store, kinds...) {
			if !yield(edge) {
				return
			}
		}
	}
}

func (s *pythonCursorGuardStore) FileNodeIdentitiesSeq(repos []string) iter.Seq[graph.FileNodeIdentity] {
	require.False(s.t, s.cursorOpen, "package lookup must not query while the import cursor is open")
	return graph.FileNodeIdentitiesSeq(s.Store, repos)
}

func TestPythonConsumerLookupClosesImportCursor(t *testing.T) {
	for _, backend := range []string{"memory", "sqlite"} {
		t.Run(backend, func(t *testing.T) {
			var store graph.Store = graph.New()
			if backend == "sqlite" {
				db, err := store_sqlite.Open(filepath.Join(t.TempDir(), "graph.sqlite"))
				require.NoError(t, err)
				t.Cleanup(func() { require.NoError(t, db.Close()) })
				store = db
			}
			g := &pythonCursorGuardStore{Store: store, t: t}
			provider, consumer := t.TempDir(), t.TempDir()
			writeWorkspaceFile(t, provider, "pyproject.toml", "[project]\nname = 'library'\n")
			writeWorkspaceFile(t, provider, "mylib.py", "def f(): pass\n")
			writeWorkspaceFile(t, consumer, "pyproject.toml", "[project]\ndependencies = ['library']\n")
			for _, file := range []string{"provider/mylib.py", "consumer/app.py"} {
				g.AddNode(&graph.Node{ID: file, FilePath: file, Kind: graph.KindFile, RepoPrefix: graph.RepoPrefixOfID(file), Language: "python"})
			}
			g.AddEdge(&graph.Edge{From: "consumer/app.py", FilePath: "consumer/app.py", To: "external::mylib", Kind: graph.EdgeImports})
			mi := &MultiIndexer{graph: g, repos: map[string]*RepoMetadata{"provider": {RootPath: provider}, "consumer": {RootPath: consumer}}}
			require.Equal(t, []string{"consumer/app.py"}, mi.pythonConsumerFilesForRepo("provider"))
		})
	}
}

func TestSingleRepoPythonAddedFileSkipsCrossRepoScan(t *testing.T) {
	root := t.TempDir()
	writeWorkspaceFile(t, root, "app.py", "import missing\n")
	g := &pythonLookupScanStore{Store: graph.New()}
	reg := newTestRegistry()
	reg.Register(languages.NewPythonExtractor())
	mi := NewMultiIndexer(g, reg, search.NewNull(), newTestConfigManager(t), zap.NewNop())
	t.Cleanup(func() { require.NoError(t, mi.Close(context.Background())) })
	_, err := mi.TrackRepoCtx(t.Context(), config.RepoEntry{Path: root, Name: "solo", Workspace: "shared"})
	require.NoError(t, err)
	g.importScans = 0
	writeWorkspaceFile(t, root, "missing.py", "pass\n")
	_, err = mi.IncrementalReindexRepo("solo", []string{filepath.Join(root, "missing.py")})
	require.NoError(t, err)
	require.Zero(t, g.importScans, "a lone Python repo has no foreign provider to recover")
}
