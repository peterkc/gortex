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
			// The cache records its read at the file's own mtime, so the
			// stat stays inside the racy window however slow the setup is.
			recent := time.Unix(1700000000, 0)
			require.NoError(t, os.Chtimes(main, recent, recent))
			pinGoModuleSourceClock(t, recent)
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
		for _, failure := range []string{"stat", "read", "size"} {
			t.Run(backend+"/"+failure, func(t *testing.T) {
				mi, g, repos := goModuleLifecycleFixture(t, backend)
				main := filepath.Join(repos[0].Path, "main.go")
				// Keep the caller's stat racy, so a cached fact can only be
				// reused after a read, independent of runner speed.
				recent := time.Unix(1700000000, 0)
				require.NoError(t, os.Chtimes(main, recent, recent))
				pinGoModuleSourceClock(t, recent)
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
				original := readGoModuleCallerFile
				reads := 0
				readGoModuleCallerFile = func(string) ([]byte, error) { reads++; return nil, os.ErrPermission }
				t.Cleanup(func() { readGoModuleCallerFile = original })
				switch failure {
				case "stat":
					require.NoError(t, os.Remove(main))
				case "size":
					mi.GetIndexer("a").config.MaxFileSize = 1
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
	require.Equal(t, 1, reads[manifest], "verification, module identity, and replacements share one read")
	clear(reads)
	mi.goModuleChecked = time.Time{}
	second, nextGeneration := mi.goModuleSnapshot()
	assert.Equal(t, first, second)
	assert.Equal(t, generation, nextGeneration)
	assert.Equal(t, map[string]int{manifest: 1, filepath.Join(nonGoRoot, "go.mod"): 1}, reads, "one read attempt per root per eligible check")
}

func TestGoModuleContentStampAndParsedEvidenceShareBytes(t *testing.T) {
	root := t.TempDir()
	x := []byte("module example.org/library\nreplace example.org/dep => example.org/other v1.2.3\n")
	y := []byte("module example.org/renamed\n")
	writeFile(t, filepath.Join(root, "go.mod"), string(x))
	original := readGoModuleFile
	reads := 0
	readGoModuleFile = func(string) ([]byte, error) {
		reads++
		if reads == 1 {
			return x, nil
		}
		return y, nil
	}
	t.Cleanup(func() { readGoModuleFile = original })
	mi := &MultiIndexer{repos: map[string]*RepoMetadata{"lib": {RootPath: root}}, logger: zap.NewNop()}
	modules, _ := mi.goModuleSnapshot()
	assert.Equal(t, 1, reads, "one captured read must supply hash, identity and replacements")
	assert.Equal(t, "example.org/library", modules["lib"])
	assert.Equal(t, contentHashForSource(x), mi.goModuleFiles["lib"].content)
	require.Len(t, mi.goModuleReplacements["lib"], 1)
	assert.Equal(t, "example.org/dep", mi.goModuleReplacements["lib"][0].Module)
}

// pinGoModuleSourceClock records caller-cache reads at a fixed time, so each
// test takes one branch of the racy-file rule regardless of runner speed.
func pinGoModuleSourceClock(t *testing.T, at time.Time) {
	t.Helper()
	original := goModuleSourceNow
	goModuleSourceNow = func() time.Time { return at }
	t.Cleanup(func() { goModuleSourceNow = original })
}

// Given a recovery that reads a caller before a save of that caller is known,
// when it stores its result, then it keeps the facts but not the stat trust:
// the next recovery reads again instead of reusing pre-save bytes.
func TestGoModuleSourceStoreDropsTrustAfterConcurrentSave(t *testing.T) {
	mi, _, repos := goModuleLifecycleFixture(t, "memory")
	main := filepath.Join(repos[0].Path, "main.go")
	now := time.Unix(1700000000, 0)
	old := now.Add(-time.Hour)
	require.NoError(t, os.Chtimes(main, old, old))
	pinGoModuleSourceClock(t, now)
	writeFile(t, filepath.Join(repos[1].Path, "lib.go"), "package library\nfunc Other(){}\n")
	for _, entry := range repos[:2] {
		_, err := mi.TrackRepoCtx(t.Context(), entry)
		require.NoError(t, err)
	}
	mi.runCrossRepoResolveFiles([]string{"b/lib.go"})
	idx := mi.GetIndexer("a")
	require.Contains(t, idx.goModuleSourceCache, "main.go")
	// Touch the stat so the next recovery verifies by reading.
	touched := old.Add(-time.Hour)
	require.NoError(t, os.Chtimes(main, touched, touched))
	original := readGoModuleCallerFile
	reads := 0
	readGoModuleCallerFile = func(path string) ([]byte, error) {
		reads++
		src, err := original(path)
		if reads == 1 {
			idx.invalidateGoModuleSourceStamp("main.go") // a save lands during the read
		}
		return src, err
	}
	t.Cleanup(func() { readGoModuleCallerFile = original })
	cr := mi.newCrossRepoResolver()
	mi.restoreGoModuleSourceFile(cr, "a/main.go")
	require.Equal(t, 1, reads)
	assert.True(t, idx.goModuleSourceCache["main.go"].recordedAt.IsZero(), "a result read before a known save must not be trusted by stat")
	mi.restoreGoModuleSourceFile(cr, "a/main.go")
	assert.Equal(t, 2, reads, "the next recovery verifies content again")
	mi.restoreGoModuleSourceFile(cr, "a/main.go")
	assert.Equal(t, 2, reads, "an undisturbed verification restores stat trust")
}
