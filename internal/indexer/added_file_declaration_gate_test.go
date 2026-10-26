package indexer

import (
	"github.com/stretchr/testify/require"
	"github.com/zzet/gortex/internal/config"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/parser/languages"
	"path/filepath"
	"strings"
	"testing"
)

func TestPythonAddedFileDeclarationGate(t *testing.T) {
	provider, consumer := t.TempDir(), t.TempDir()
	writeWorkspaceFile(t, provider, "pyproject.toml", "[project]\nname = 'library'\n")
	writeWorkspaceFile(t, consumer, "pyproject.toml", "[project]\nname = 'app'\n")
	g := &pythonLookupScanStore{Store: graph.New()}
	g.AddEdge(&graph.Edge{From: "consumer/app.py", FilePath: "consumer/app.py", To: "external::pkg.extra", Kind: graph.EdgeImports})
	mi := &MultiIndexer{graph: g, repos: map[string]*RepoMetadata{"provider": {RootPath: provider}, "consumer": {RootPath: consumer}}}
	for range 2 {
		require.Empty(t, mi.pythonAddedFileConsumers([]string{"provider/pkg/extra.py"}))
	}
	require.Zero(t, g.importScans, "no declaration means no import scan")
	writeWorkspaceFile(t, consumer, "pyproject.toml", "[project]\ndependencies = ['library']\n")
	require.Equal(t, []string{"consumer/app.py"}, mi.pythonAddedFileConsumers([]string{"provider/pkg/extra.py"}))
	require.Equal(t, 1, g.importScans)
	require.Zero(t, g.nodeReads, "admission must not build the provider lookup")
	writeWorkspaceFile(t, consumer, "pyproject.toml", "[project]\nname = 'app'\n")
	g.importScans = 0
	require.Empty(t, mi.pythonAddedFileConsumers([]string{"provider/pkg/next.py"}))
	require.Zero(t, g.importScans)
	// Existing but unreadable snapshot evidence must open admission.
	prior := readDiskFile
	readDiskFile = func(file string) ([]byte, bool) { return nil, false }
	t.Cleanup(func() { readDiskFile = prior })
	require.Equal(t, []string{"consumer/app.py"}, mi.pythonAddedFileConsumers([]string{"provider/pkg/extra.py"}))
	require.Equal(t, 1, g.importScans)
}

func TestCargoAddedFileDeclarationGate(t *testing.T) {
	provider, consumer := t.TempDir(), t.TempDir()
	writeWorkspaceFile(t, provider, "Cargo.toml", "[package]\nname = 'library'\n")
	writeWorkspaceFile(t, consumer, "Cargo.toml", "[package]\nname = 'app'\n")
	g := &cargoWarmScanStore{Store: graph.New()}
	g.AddEdge(&graph.Edge{From: "consumer/src/lib.rs", FilePath: "consumer/src/lib.rs", To: "external::library/extra", Kind: graph.EdgeImports})
	mi := &MultiIndexer{graph: g, repos: map[string]*RepoMetadata{"provider": {RootPath: provider}, "consumer": {RootPath: consumer}}}
	for range 2 {
		require.Empty(t, mi.cargoAddedFileConsumers("provider"))
	}
	require.Zero(t, g.lightScans)
	writeWorkspaceFile(t, consumer, "Cargo.toml", "[package]\nname = 'app'\n[dependencies]\nlibrary = '1'\n")
	require.Equal(t, []string{"consumer/src/lib.rs"}, mi.cargoAddedFileConsumers("provider"))
	require.Equal(t, 1, g.lightScans)
	writeWorkspaceFile(t, consumer, "Cargo.toml", "[package]\nname = 'app'\n")
	g.lightScans = 0
	require.Empty(t, mi.cargoAddedFileConsumers("provider"))
	require.Zero(t, g.lightScans)
	prior := readDiskFile
	readDiskFile = func(file string) ([]byte, bool) { return nil, false }
	t.Cleanup(func() { readDiskFile = prior })
	require.Equal(t, []string{"consumer/src/lib.rs"}, mi.cargoAddedFileConsumers("provider"))
	require.Equal(t, 1, g.lightScans)
}

func TestPythonAddedTestPackageDoesNotScan(t *testing.T) {
	provider, consumer := t.TempDir(), t.TempDir()
	writeWorkspaceFile(t, provider, "pyproject.toml", "[project]\nname = 'library'\n")
	writeWorkspaceFile(t, consumer, "pyproject.toml", "[project]\ndependencies = ['library']\n")
	g := &pythonLookupScanStore{Store: graph.New()}
	mi := &MultiIndexer{graph: g, repos: map[string]*RepoMetadata{"provider": {RootPath: provider}, "consumer": {RootPath: consumer}}}
	require.Empty(t, mi.pythonAddedFileConsumers([]string{"provider/tests/__init__.py", "provider/src/test/helpers.py"}))
	require.Zero(t, g.importScans)
}

func TestAddedFileAfterDeclarationMatchesCold(t *testing.T) {
	for _, backend := range []string{"memory", "sqlite"} {
		for _, lang := range []string{"python", "cargo"} {
			t.Run(backend+"/"+lang, func(t *testing.T) {
				provider, consumer := t.TempDir(), t.TempDir()
				manifest, caller, added := "pyproject.toml", "app.py", "pkg/extra.py"
				providerManifest, consumerManifest := "[project]\nname = 'library'\n", "[project]\nname = 'app'\n"
				declaration := "dependencies = ['library']\n"
				providerSource, consumerSource := "", "from pkg.extra import f\ndef run(): return f()\n"
				if lang == "cargo" {
					manifest, caller, added = "Cargo.toml", "src/lib.rs", "src/extra.rs"
					providerManifest, consumerManifest = "[package]\nname = 'library'\n", "[package]\nname = 'app'\n"
					declaration = "[dependencies]\nlibrary = '1'\n"
					providerSource, consumerSource = "pub mod extra;\n", "use library::extra::f;\npub fn run() { f(); }\n"
					writeWorkspaceFile(t, provider, "src/lib.rs", providerSource)
				} else {
					writeWorkspaceFile(t, provider, "pkg/__init__.py", providerSource)
				}
				writeWorkspaceFile(t, provider, manifest, providerManifest)
				writeWorkspaceFile(t, consumer, manifest, consumerManifest)
				writeWorkspaceFile(t, consumer, caller, consumerSource)
				entries := []config.RepoEntry{{Path: consumer, Name: "consumer", Workspace: "shared"}, {Path: provider, Name: "provider", Workspace: "shared"}}
				mi, g := newPythonBridgeIdentityIndexer(t, backend, entries)
				mi.registry.Register(languages.NewRustExtractor())
				_, err := mi.IndexAll()
				require.NoError(t, err)
				for _, target := range revisionCallTargets(g, "consumer/"+caller, graph.EdgeImports) {
					require.False(t, strings.HasPrefix(target, "provider/"))
				}
				// The declaration changes without recreating the MultiIndexer or resolver registry.
				writeWorkspaceFile(t, consumer, manifest, consumerManifest+declaration)
				if lang == "cargo" {
					providerSource = "pub fn f() {}\n"
				} else {
					providerSource = "def f(): pass\n"
				}
				writeWorkspaceFile(t, provider, added, providerSource)
				_, err = mi.IncrementalReindexRepo("provider", []string{filepath.Join(provider, added)})
				require.NoError(t, err)
				targets := revisionCallTargets(g, "consumer/"+caller, graph.EdgeImports)
				require.Contains(t, targets, "provider/"+added)
				cold, coldGraph := newPythonBridgeIdentityIndexer(t, backend, entries)
				cold.registry.Register(languages.NewRustExtractor())
				_, err = cold.IndexAll()
				require.NoError(t, err)
				require.ElementsMatch(t, revisionCallTargets(coldGraph, "consumer/"+caller, graph.EdgeImports), targets)
				require.ElementsMatch(t, revisionCallTargets(coldGraph, "consumer/"+caller+"::run", graph.EdgeCalls), revisionCallTargets(g, "consumer/"+caller+"::run", graph.EdgeCalls))
				// Removal closes admission for the next addition. Full/retrack recovery is separate.
				writeWorkspaceFile(t, consumer, manifest, consumerManifest)
				if lang == "cargo" {
					require.Empty(t, mi.cargoAddedFileConsumers("provider"))
				} else {
					require.Empty(t, mi.pythonAddedFileConsumers([]string{"provider/pkg/next.py"}))
				}
				require.NoError(t, mi.GetIndexer("provider").IndexFile(filepath.Join(provider, added)))
			})
		}
	}
}
