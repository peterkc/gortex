package indexer

import (
	"context"
	"iter"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/zzet/gortex/internal/config"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/graph/store_sqlite"
	"github.com/zzet/gortex/internal/parser"
	"github.com/zzet/gortex/internal/search"
	"go.uber.org/zap"
)

// Given persisted module edges and a clean restart, when warmup seeds retries,
// then the first save performs no definition-stream or external-terminal scan.
func TestGoModuleWarmSeedKeepsScansOffFirstSave(t *testing.T) {
	db, err := store_sqlite.Open(filepath.Join(t.TempDir(), "graph.sqlite"))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, db.Close()) })
	g := &goModuleRetryScanStore{goModulePublishScanCountingStore: goModulePublishScanCountingStore{Store: db}}
	root := t.TempDir()
	writeFile(t, filepath.Join(root, "go.mod"), "module example.org/library\n")
	cm := newTestConfigManager(t)
	cm.Global().Repos = []config.RepoEntry{{Name: "b", Path: root}}
	g.AddNode(&graph.Node{ID: "b/lib.go::Hello", Name: "Hello", Kind: graph.KindFunction, RepoPrefix: "b", FilePath: "b/lib.go", Language: "go"})
	mi := NewMultiIndexer(g, newTestRegistry(), search.NewNull(), cm, zap.NewNop())
	t.Cleanup(func() { require.NoError(t, mi.Close(context.Background())) })
	require.NoError(t, mi.SeedGoModuleDeps(t.Context()))
	require.Positive(t, g.definitions, "warmup owns the initial definition scan")
	require.Equal(t, 1, g.scans, "warmup owns the initial terminal scan")
	g.definitions, g.scans = 0, 0
	mi.runCrossRepoResolveFiles([]string{"b/lib.go"})
	assert.Zero(t, g.definitions, "first save must not seed persisted bindings")
	assert.Zero(t, g.scans, "first save must not seed external terminals")
}

type npmEvictionScanStore struct {
	graph.Store
	scans int
}

func (s *npmEvictionScanStore) RepoNodeIdentitiesSeq(repos []string, kinds ...graph.NodeKind) iter.Seq[graph.RepoNodeIdentity] {
	s.scans++
	return graph.RepoNodeIdentitiesSeq(s.Store, repos, kinds...)
}

// Given Go-only and JS/TS repositories, when npm eviction runs,
// then it scans definitions only for the JS/TS repository.
func TestNonJSTSRepoEvictionSkipsNpmRestub(t *testing.T) {
	for _, lang := range []string{"go", "typescript"} {
		t.Run(lang, func(t *testing.T) {
			g := &npmEvictionScanStore{Store: graph.New()}
			ext := "go"
			if lang == "typescript" {
				ext = "ts"
			}
			file := "repo/main." + ext
			g.AddNode(&graph.Node{ID: file, FilePath: file, RepoPrefix: "repo", Language: lang, Kind: graph.KindFile})
			g.AddNode(&graph.Node{ID: file + "::Hello", FilePath: file, RepoPrefix: "repo", Language: lang, Kind: graph.KindFunction, Name: "Hello"})
			restubTrackedNpmRepoIncoming(g, "repo")
			if lang == "go" {
				assert.Zero(t, g.scans)
			} else {
				assert.Equal(t, 1, g.scans)
			}
		})
	}
}

// Given an unbindable terminal, when unchanged provider definitions are saved
// twice, then the second pass must not re-extract the unchanged caller.
func TestGoModuleUnbindableSourceExtractionWork(t *testing.T) {
	mi, _, repos := goModuleLifecycleFixture(t, "memory")
	writeFile(t, filepath.Join(repos[0].Path, "main.go"), "package caller\nimport library \"example.org/library\"\nfunc Caller(){ library.Missing() }\n")
	writeFile(t, filepath.Join(repos[1].Path, "lib.go"), "package library\nfunc Other() {}\n")
	ext, ok := mi.registry.GetByLanguage("go")
	require.True(t, ok)
	spy := &goModuleSourceExtractionSpy{Extractor: ext}
	mi.registry.Register(spy)
	for _, entry := range repos[:2] {
		_, err := mi.TrackRepoCtx(t.Context(), entry)
		require.NoError(t, err)
	}
	mi.runCrossRepoResolveFiles([]string{"b/lib.go"})
	spy.calls = 0
	mi.runCrossRepoResolveFiles([]string{"b/lib.go"})
	assert.Zero(t, spy.calls, "an unchanged unbindable terminal must not re-parse the caller")
}

type goModuleSourceExtractionSpy struct {
	parser.Extractor
	calls int
}

func (s *goModuleSourceExtractionSpy) Extract(path string, src []byte) (*parser.ExtractionResult, error) {
	if path == "main.go" {
		s.calls++
	}
	return s.Extractor.Extract(path, src)
}

// Given a Go workspace plus a JS-only repository, when the JS repo is removed,
// then removal must not invalidate and reseed the Go terminal sweep.
func TestNonGoUntrackSkipsModuleSweep(t *testing.T) {
	db, err := store_sqlite.Open(filepath.Join(t.TempDir(), "graph.sqlite"))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, db.Close()) })
	g := &goModuleUntrackScanStore{goModuleRetryScanStore: &goModuleRetryScanStore{goModulePublishScanCountingStore: goModulePublishScanCountingStore{Store: db}}}
	cm := newTestConfigManager(t)
	js, lib := t.TempDir(), t.TempDir()
	writeFile(t, filepath.Join(js, "main.js"), "export function Hello() {}\n")
	writeFile(t, filepath.Join(lib, "go.mod"), "module example.org/library\n")
	writeFile(t, filepath.Join(lib, "lib.go"), "package library\nfunc Hello() {}\n")
	mi := NewMultiIndexer(g, newTestRegistry(), search.NewNull(), cm, zap.NewNop())
	t.Cleanup(func() { require.NoError(t, mi.Close(context.Background())) })
	for _, entry := range []config.RepoEntry{{Name: "a", Path: js}, {Name: "b", Path: lib}} {
		_, err := mi.TrackRepoCtx(t.Context(), entry)
		require.NoError(t, err)
	}
	require.NoError(t, mi.SeedGoModuleDeps(t.Context()))
	g.scans = 0
	_, _, err = mi.UntrackRepoChecked(t.Context(), "a")
	require.NoError(t, err)
	assert.Zero(t, g.scans, "JS-only untrack must not rescan Go terminals")
}

type goModuleUntrackScanStore struct{ *goModuleRetryScanStore }

func (s *goModuleUntrackScanStore) PurgeRepo(repo string) error {
	return s.Store.(interface{ PurgeRepo(string) error }).PurgeRepo(repo)
}
