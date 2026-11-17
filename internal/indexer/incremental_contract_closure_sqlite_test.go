package indexer

import (
	"encoding/json"
	"fmt"
	"path/filepath"
	"slices"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/zzet/gortex/internal/config"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/graph/store_sqlite"
	"github.com/zzet/gortex/internal/parser"
	"github.com/zzet/gortex/internal/parser/languages"
	"github.com/zzet/gortex/internal/search"
	"go.uber.org/zap"
)

func closureIndexerRows(t *testing.T, store *store_sqlite.Store) []string {
	t.Helper()
	var result []string
	for _, n := range store.AllNodes() {
		row, err := json.Marshal(n)
		require.NoError(t, err)
		result = append(result, "node:"+string(row))
	}
	for _, e := range store.AllEdges() {
		row, err := json.Marshal(e)
		require.NoError(t, err)
		result = append(result, fmt.Sprintf("edge:%s/confidence=%g", row, e.Confidence))
	}
	slices.Sort(result)
	return result
}

func TestSQLiteContractClosureParserUpdateResume(t *testing.T) {
	// Given a durable extraction from an old parser, with an off-file ownership
	// chain and another repository's live owner retaining a shared canonical.
	root := t.TempDir()
	writeFile(t, filepath.Join(root, "failed.go"), "package fixture\nfunc Keep() {}\n")
	path := filepath.Join(t.TempDir(), "resume.sqlite")
	store, err := store_sqlite.Open(path)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, store.Close()) })
	extractor := &restageFixtureExtractor{Extractor: languages.NewGoExtractor(), name: "OldExtraction"}
	registry := parser.NewRegistry()
	registry.Register(extractor)
	idx := New(store, registry, config.IndexConfig{Workers: 1}, zap.NewNop())
	idx.search = search.NewNull()
	idx.SetRepoPrefix("fixture")
	_, err = idx.Index(root)
	require.NoError(t, err)
	const oldID = "fixture/failed.go::OldExtraction"
	require.NotNil(t, store.GetNode(oldID))
	canonical := func(id string) *graph.Node {
		return &graph.Node{ID: id, Kind: graph.KindContract, Name: id, FilePath: "records/off-file.env", RepoPrefix: "fixture",
			Meta: map[string]any{"type": "env", "contract_owner_record": true, "nested": map[string]any{"payload": "preserved"}}}
	}
	owner := func(from, to, file, repo string) *graph.Edge {
		return &graph.Edge{From: from, To: to, Kind: graph.EdgeProvides, FilePath: file,
			Meta: map[string]any{"contract_owner_repo_prefix": repo}}
	}
	require.NoError(t, store.AddBatchCheckedContext(t.Context(), []*graph.Node{
		canonical("env::UPDATE_ROOT"), canonical("env::UPDATE_TAIL"), canonical("env::UPDATE_SHARED"),
		{ID: "other/live", Kind: graph.KindFunction, Name: "Live", FilePath: "other/live.go", RepoPrefix: "other"},
	}, []*graph.Edge{
		owner(oldID, "env::UPDATE_ROOT", "fixture/failed.go", "fixture"),
		owner("env::UPDATE_ROOT", "env::UPDATE_TAIL", "records/off-file.env", "fixture"),
		owner(oldID, "env::UPDATE_SHARED", "fixture/failed.go", "fixture"),
		owner("other/live", "env::UPDATE_SHARED", "other/live.go", "other"),
	}))
	require.NoError(t, store.BatchUpsertSymbolFTS([]graph.SymbolFTSItem{
		{NodeID: "env::UPDATE_ROOT", Tokens: "updateroot"}, {NodeID: "env::UPDATE_TAIL", Tokens: "updatetail"},
		{NodeID: "env::UPDATE_SHARED", Tokens: "updateshared"},
	}))
	state, found, err := store.GetRepoIndexState("fixture")
	require.NoError(t, err)
	require.True(t, found)
	state.ExtractorVersions = snapshotJSON(t, map[string]int{"go": extractorVersionForLang("go") - 1})
	require.NoError(t, store.SetRepoIndexState(state))
	before, mtimes := closureIndexerRows(t, store), store.LoadFileMtimes("fixture")
	require.NotEmpty(t, before)
	require.NotEmpty(t, mtimes)
	extractor.name = "NewExtraction"
	calls := 0
	idx.warmupWriteBoundary = func() error { calls++; return store_sqlite.ErrWarmupWALPressure }
	// When update admission is refused before eviction, freshness cannot advance.
	_, err = idx.IncrementalReindexPaths(root, nil)
	require.ErrorIs(t, err, store_sqlite.ErrWarmupWALPressure)
	require.Equal(t, 1, calls)
	require.Equal(t, before, closureIndexerRows(t, store))
	require.Equal(t, mtimes, store.LoadFileMtimes("fixture"))
	refused, found, err := store.GetRepoIndexState("fixture")
	require.NoError(t, err)
	require.True(t, found)
	require.Equal(t, state, refused)
	require.NoError(t, store.Close())
	// Then reopening preserves the old rows/version, and the next admitted pass
	// evicts through the closure, writes the new extraction and only then stamps it.
	reopened, err := store_sqlite.Open(path)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, reopened.Close()) })
	require.Equal(t, before, closureIndexerRows(t, reopened))
	resumed := New(reopened, registry, config.IndexConfig{Workers: 1}, zap.NewNop())
	resumed.search = search.NewNull()
	resumed.SetRepoPrefix("fixture")
	resumed.SetRootPath(root)
	resumed.SetFileMtimes(reopened.LoadFileMtimes("fixture"))
	result, err := resumed.IncrementalReindexPaths(root, nil)
	require.NoError(t, err)
	require.Equal(t, 1, result.StaleFileCount)
	require.Empty(t, result.FailedFiles)
	require.Nil(t, reopened.GetNode(oldID))
	require.NotNil(t, reopened.GetNode("fixture/failed.go::NewExtraction"))
	require.Nil(t, reopened.GetNode("env::UPDATE_ROOT"))
	require.Nil(t, reopened.GetNode("env::UPDATE_TAIL"))
	require.NotNil(t, reopened.GetNode("env::UPDATE_SHARED"))
	require.Len(t, reopened.GetInEdges("env::UPDATE_SHARED"), 1)
	require.Equal(t, "other/live", reopened.GetInEdges("env::UPDATE_SHARED")[0].From)
	for _, token := range []string{"updateroot", "updatetail"} {
		documents, err := reopened.SearchSymbols(token, 10)
		require.NoError(t, err)
		require.Empty(t, documents)
	}
	documents, err := reopened.SearchSymbols("updateshared", 10)
	require.NoError(t, err)
	require.Len(t, documents, 1)
	current, found, err := reopened.GetRepoIndexState("fixture")
	require.NoError(t, err)
	require.True(t, found)
	require.Empty(t, ExtractorVersionStaleLangs(current.ExtractorVersions))
	require.Equal(t, state.IndexedSHA, current.IndexedSHA)
	stable := closureIndexerRows(t, reopened)
	repeated, err := resumed.IncrementalReindexPaths(root, nil)
	require.NoError(t, err)
	require.Zero(t, repeated.StaleFileCount)
	require.Equal(t, stable, closureIndexerRows(t, reopened))
}
