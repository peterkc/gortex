package indexer

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/zzet/gortex/internal/graph"
	"go.uber.org/zap"
	"go.uber.org/zap/zaptest/observer"
)

// Given cached source facts, when equal-size bytes change with restored mtime,
// then provider recovery must re-extract the current calls and match cold.
func TestGoModuleSameStampSourceRecoveryColdEquivalent(t *testing.T) {
	for _, backend := range []string{"memory", "sqlite"} {
		t.Run(backend, func(t *testing.T) {
			mi, g, repos := goModuleLifecycleFixture(t, backend)
			main := filepath.Join(repos[0].Path, "main.go")
			lib := filepath.Join(repos[1].Path, "lib.go")
			before := "package caller\nimport library \"example.org/library\"\nfunc Caller(){ library.Hello(library.Missing()) }\n"
			after := strings.Replace(before, "Missing", "Present", 1)
			require.Equal(t, len(before), len(after))
			writeFile(t, main, before)
			writeFile(t, lib, "package library\nfunc Other() {}\n")
			for _, entry := range repos[:2] {
				_, err := mi.TrackRepoCtx(t.Context(), entry)
				require.NoError(t, err)
			}
			mi.runCrossRepoResolveFiles([]string{"b/lib.go"})
			require.Contains(t, mi.GetIndexer("a").goModuleSourceCache, "main.go")
			info, err := os.Stat(main)
			require.NoError(t, err)
			writeFile(t, main, after)
			stamp := info.ModTime()
			require.NoError(t, os.Chtimes(main, stamp, stamp))
			changed, err := os.Stat(main)
			require.NoError(t, err)
			require.Equal(t, info.Size(), changed.Size())
			require.Equal(t, info.ModTime().UnixNano(), changed.ModTime().UnixNano())
			writeFile(t, lib, "package library\nfunc Present() int{return 1}\n")
			require.NoError(t, mi.GetIndexer("b").IndexFile(lib))
			cold := revisionColdGraph(t, backend, repos[:2])
			require.ElementsMatch(t, []string{"external-call::dep::example.org/library", "b/lib.go::Present"}, revisionCallTargets(cold, "a/main.go::Caller", graph.EdgeCalls))
			assert.ElementsMatch(t, revisionCallTargets(cold, "a/main.go::Caller", graph.EdgeCalls), revisionCallTargets(g, "a/main.go::Caller", graph.EdgeCalls))
		})
	}
}

func TestGoModuleSnapshotRejectsFailedInputs(t *testing.T) {
	for _, failure := range []string{"stat", "read"} {
		t.Run(failure, func(t *testing.T) {
			root := t.TempDir()
			if failure == "read" {
				require.NoError(t, os.Mkdir(filepath.Join(root, "go.mod"), 0o755))
			} else {
				root = "\x00"
			}
			core, logs := observer.New(zap.WarnLevel)
			mi := &MultiIndexer{repos: map[string]*RepoMetadata{"b": {RootPath: root}}, logger: zap.New(core)}
			// Equal failed stat/read results must not be valid cache evidence.
			modules, _ := mi.goModuleSnapshot()
			require.Empty(t, modules)
			require.Len(t, logs.All(), 1)
			mi.goModuleChecked = time.Time{}
			modules, _ = mi.goModuleSnapshot()
			assert.Empty(t, modules)
			assert.Len(t, logs.All(), 2, "failed inputs must rebuild/refuse on each eligible check")
		})
	}
}

func TestGoModuleSourceFailureDoesNotRestoreCachedCalls(t *testing.T) {
	for _, backend := range []string{"memory", "sqlite"} {
		for _, failure := range []string{"stat", "read"} {
			t.Run(backend+"/"+failure, func(t *testing.T) {
				mi, g, repos := goModuleLifecycleFixture(t, backend)
				writeFile(t, filepath.Join(repos[1].Path, "lib.go"), "package library\nfunc Other(){}\n")
				for _, entry := range repos[:2] {
					_, err := mi.TrackRepoCtx(t.Context(), entry)
					require.NoError(t, err)
				}
				mi.runCrossRepoResolveFiles([]string{"b/lib.go"})
				require.Contains(t, mi.GetIndexer("a").goModuleSourceCache, "main.go")
				// Publish a recoverable target without retrying the terminal.
				g.AddNode(&graph.Node{ID: "b/lib.go::Hello", Name: "Hello", Kind: graph.KindFunction, RepoPrefix: "b", FilePath: "b/lib.go", WorkspaceID: "shared", Language: "go"})
				cr := mi.newCrossRepoResolver()
				before := revisionCallTargets(g, "a/main.go::Caller", graph.EdgeCalls)
				main := filepath.Join(repos[0].Path, "main.go")
				original := readGoModuleCallerFile
				reads := 0
				readGoModuleCallerFile = func(string) ([]byte, error) { reads++; return nil, os.ErrPermission }
				t.Cleanup(func() { readGoModuleCallerFile = original })
				if failure == "stat" {
					require.NoError(t, os.Remove(main))
				}
				// A cached fact is not evidence for unreadable current source.
				mi.restoreGoModuleSourceFile(cr, "a/main.go")
				if failure == "read" {
					assert.Equal(t, 1, reads)
				} else {
					assert.Zero(t, reads)
				}
				assert.ElementsMatch(t, before, revisionCallTargets(g, "a/main.go::Caller", graph.EdgeCalls))
			})
		}
	}
}

func TestGoModuleContentStampReusesMixedRootSnapshot(t *testing.T) {
	goRoot, nonGoRoot := t.TempDir(), t.TempDir()
	manifest := filepath.Join(goRoot, "go.mod")
	writeFile(t, manifest, "module example.org/library\n")
	original := readGoModuleFile
	reads := map[string]int{}
	readGoModuleFile = func(path string) ([]byte, error) { reads[path]++; return original(path) }
	t.Cleanup(func() { readGoModuleFile = original })
	mi := &MultiIndexer{repos: map[string]*RepoMetadata{"go": {RootPath: goRoot}, "js": {RootPath: nonGoRoot}}, logger: zap.NewNop()}
	// Given one module and one valid absence, when an eligible unchanged
	// snapshot check runs, then only the content-verification read is needed.
	first, generation := mi.goModuleSnapshot()
	require.Equal(t, map[string]string{"go": "example.org/library"}, first)
	require.Equal(t, 3, reads[manifest], "initial verification, module identity, and replacement reads")
	clear(reads)
	mi.goModuleChecked = time.Time{}
	second, nextGeneration := mi.goModuleSnapshot()
	assert.Equal(t, first, second)
	assert.Equal(t, generation, nextGeneration)
	assert.Equal(t, map[string]int{manifest: 1}, reads, "valid absence must not force module/replacement rebuild reads")
}
