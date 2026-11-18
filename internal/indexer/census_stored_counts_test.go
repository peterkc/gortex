package indexer

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
	"go.uber.org/zap/zaptest/observer"

	"github.com/zzet/gortex/internal/config"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/graph/store_sqlite"
	"github.com/zzet/gortex/internal/search"
)

func censusCountSource(t *testing.T, logs *observer.ObservedLogs) string {
	t.Helper()
	entries := logs.FilterMessage("indexer: clean census counts").All()
	require.Len(t, entries, 1)
	source, _ := entries[0].ContextMap()["source"].(string)
	return source
}

// A restart over an unchanged tree reports the counts the last index stored
// and counts no row.
func TestCleanCensusReusesTheStoredCounts(t *testing.T) {
	root := t.TempDir()
	writeFile(t, filepath.Join(root, "main.go"), "package sample\nfunc Alpha() {}\nfunc Beta() { Alpha() }\n")
	entry := config.RepoEntry{Path: root, Name: "repo"}
	cm := newTestConfigManager(t)
	cm.Global().Repos = []config.RepoEntry{entry}
	store, err := store_sqlite.Open(filepath.Join(t.TempDir(), "store.sqlite"))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, store.Close()) })
	seed := NewMultiIndexer(graph.Store(store), newTestRegistry(), search.NewNull(), cm, zap.NewNop())
	_, err = seed.IndexAll()
	require.NoError(t, err)
	stored, found, err := store.GetRepoIndexState("repo")
	require.NoError(t, err)
	require.True(t, found)
	require.Positive(t, stored.NodeCount)

	counted := cleanCensusCounts.Load()
	core, logs := observer.New(zap.InfoLevel)
	restarted := NewMultiIndexer(graph.Store(store), newTestRegistry(), search.NewNull(), cm, zap.New(core))
	result, err := restarted.ReconcileRepoCtx(t.Context(), entry, seed.GetIndexer("repo").FileMtimes())
	require.NoError(t, err)
	require.NotNil(t, result)
	assert.Equal(t, "stored", censusCountSource(t, logs))
	assert.Equal(t, counted, cleanCensusCounts.Load(), "a clean census counted rows")
	assert.Equal(t, stored.NodeCount, result.NodeCount)
	assert.Equal(t, stored.EdgeCount, result.EdgeCount)
}

// A clean census with no stored state counts once and stores the count; the
// next census reuses it.
func TestCleanCensusWithNothingStoredCountsOnceAndStores(t *testing.T) {
	store, err := store_sqlite.Open(filepath.Join(t.TempDir(), "store.sqlite"))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, store.Close()) })
	store.AddBatch([]*graph.Node{
		{ID: "repo/a.go::A", Kind: graph.KindFunction, Name: "A", FilePath: "repo/a.go", RepoPrefix: "repo"},
		{ID: "repo/a.go::B", Kind: graph.KindFunction, Name: "B", FilePath: "repo/a.go", RepoPrefix: "repo"},
	}, []*graph.Edge{{From: "repo/a.go::B", To: "repo/a.go::A", Kind: graph.EdgeCalls, FilePath: "repo/a.go"}})
	core, logs := observer.New(zap.InfoLevel)
	idx := New(store, newTestRegistry(), config.Default().Index, zap.New(core))
	defer idx.Close()
	idx.SetRepoPrefix("repo")
	idx.SetRootPath(t.TempDir())

	counted := cleanCensusCounts.Load()
	first, err := idx.cleanCensusResult(t.Context(), 1, time.Now())
	require.NoError(t, err)
	assert.Equal(t, "counted", censusCountSource(t, logs))
	assert.Equal(t, counted+1, cleanCensusCounts.Load())
	assert.Equal(t, 2, first.NodeCount)
	stored, found, err := store.GetRepoIndexState("repo")
	require.NoError(t, err)
	require.True(t, found, "the counted census stored nothing")
	assert.Equal(t, 2, stored.NodeCount)
	assert.Equal(t, 1, stored.EdgeCount)

	core2, logs2 := observer.New(zap.InfoLevel)
	idx.logger = zap.New(core2)
	second, err := idx.cleanCensusResult(t.Context(), 1, time.Now())
	require.NoError(t, err)
	assert.Equal(t, "stored", censusCountSource(t, logs2))
	assert.Equal(t, counted+1, cleanCensusCounts.Load(), "the second census counted again")
	assert.Equal(t, first.NodeCount, second.NodeCount)
	_ = logs
}

// A changed tree does not take the clean path: the scoped reindex counts, and
// no clean census reports counts.
func TestChangedTreeDoesNotTakeTheStoredCensusCounts(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "main.go")
	writeFile(t, path, "package sample\nfunc Alpha() {}\n")
	entry := config.RepoEntry{Path: root, Name: "repo"}
	cm := newTestConfigManager(t)
	cm.Global().Repos = []config.RepoEntry{entry}
	store, err := store_sqlite.Open(filepath.Join(t.TempDir(), "store.sqlite"))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, store.Close()) })
	seed := NewMultiIndexer(graph.Store(store), newTestRegistry(), search.NewNull(), cm, zap.NewNop())
	_, err = seed.IndexAll()
	require.NoError(t, err)
	prior := seed.GetIndexer("repo").FileMtimes()

	writeFile(t, path, "package sample\nfunc Alpha() {}\nfunc Gamma() {}\n")
	changedAt := time.Unix(0, prior["main.go"]).Add(time.Second)
	require.NoError(t, os.Chtimes(path, changedAt, changedAt))
	core, logs := observer.New(zap.DebugLevel)
	restarted := NewMultiIndexer(graph.Store(store), newTestRegistry(), search.NewNull(), cm, zap.New(core))
	result, err := restarted.ReconcileRepoCtx(t.Context(), entry, prior)
	require.NoError(t, err)
	require.NotNil(t, result)
	assert.Empty(t, logs.FilterMessage("indexer: clean census counts").All())
	entries := logs.FilterMessage("daemon: reconciled repo from snapshot").All()
	require.Len(t, entries, 1)
	assert.NotEqual(t, "census_noop", entries[0].ContextMap()["route"])
}

type storedCensusReadFailureStore struct {
	graph.Store
	readErr     error
	countCalls  int
	stateWrites int
}

func (s *storedCensusReadFailureStore) GetRepoIndexState(string) (graph.RepoIndexState, bool, error) {
	return graph.RepoIndexState{}, false, s.readErr
}

func (s *storedCensusReadFailureStore) SetRepoIndexState(graph.RepoIndexState) error {
	s.stateWrites++
	return nil
}

func (s *storedCensusReadFailureStore) RepoMemoryEstimate(repo string) graph.RepoMemoryEstimate {
	s.countCalls++
	return s.Store.RepoMemoryEstimate(repo)
}

func TestCleanCensusRefusesStateReadFailure(t *testing.T) {
	for _, readErr := range []error{errors.New("census state read failed"), context.Canceled} {
		t.Run(readErr.Error(), func(t *testing.T) {
			// Given a failed state read, when a clean census runs, then it returns
			// that error without counting rows, restamping state, or publishing counts.
			store := &storedCensusReadFailureStore{Store: graph.New(), readErr: readErr}
			idx := New(store, newTestRegistry(), config.Default().Index, zap.NewNop())
			t.Cleanup(idx.Close)
			idx.SetRepoPrefix("repo")
			idx.SetRootPath(t.TempDir())

			result, err := idx.cleanCensusResult(t.Context(), 1, time.Now())
			assert.ErrorIs(t, err, readErr)
			assert.Nil(t, result)
			assert.Zero(t, store.countCalls)
			assert.Zero(t, store.stateWrites)
			assert.Nil(t, idx.lastRepoCounts.Load())
		})
	}
}

func TestCleanCensusWithoutStateReaderCountsActualRows(t *testing.T) {
	for _, empty := range []bool{false, true} {
		t.Run(fmt.Sprintf("empty=%v", empty), func(t *testing.T) {
			// Given no optional state cache, when a clean census runs, then actual
			// counts remain available, including a valid empty repository.
			base := graph.New()
			if !empty {
				base.AddBatch([]*graph.Node{
					{ID: "repo/a.go::A", Kind: graph.KindFunction, RepoPrefix: "repo"},
					{ID: "repo/a.go::B", Kind: graph.KindFunction, RepoPrefix: "repo"},
				}, []*graph.Edge{{From: "repo/a.go::B", To: "repo/a.go::A", Kind: graph.EdgeCalls}})
			}
			store := &residualBatchCountingStore{Store: base}
			idx := New(store, newTestRegistry(), config.Default().Index, zap.NewNop())
			t.Cleanup(idx.Close)
			idx.SetRepoPrefix("repo")
			idx.SetRootPath(t.TempDir())

			result, err := idx.cleanCensusResult(t.Context(), 0, time.Now())
			require.NoError(t, err)
			require.NotNil(t, result)
			if empty {
				assert.Zero(t, result.NodeCount)
				assert.Zero(t, result.EdgeCount)
			} else {
				assert.Equal(t, 2, result.NodeCount)
				assert.Equal(t, 1, result.EdgeCount)
			}
		})
	}
}

func TestCleanCensusReusesParentStateCounts(t *testing.T) {
	// Given an output generation with no own state, when its parent has stored
	// counts, then a clean census reuses those counts without stamping the child.
	builder, request, claim, _ := claimedCopyFixture(t)
	parentState := graph.RepoIndexState{RepoPrefix: request.RepoPrefix, NodeCount: 7, EdgeCount: 11}
	require.NoError(t, generationAt(builder.Store, 0).SetRepoIndexState(parentState))
	handle := generationAt(builder.Store, claim.GenerationID)
	core, logs := observer.New(zap.InfoLevel)
	idx := New(handle, newTestRegistry(), config.Default().Index, zap.New(core))
	idx.SetCensusCatalog(builder.Catalog)
	t.Cleanup(idx.Close)
	idx.SetRepoPrefix(request.RepoPrefix)
	idx.SetRootPath(request.RootPath)

	result, err := idx.cleanCensusResult(t.Context(), 1, time.Now())
	require.NoError(t, err)
	require.NotNil(t, result)
	assert.Equal(t, 7, result.NodeCount)
	assert.Equal(t, 11, result.EdgeCount)
	assert.Equal(t, "stored", censusCountSource(t, logs))
	_, found, err := handle.GetRepoIndexState(request.RepoPrefix)
	require.NoError(t, err)
	assert.False(t, found)
}

func TestCleanCensusRefusesCatalogCancellation(t *testing.T) {
	// Given no own state and a canceled catalog read, when a clean census runs,
	// then cancellation is returned without counting or writing a new state row.
	builder, request, claim, _ := claimedCopyFixture(t)
	handle := generationAt(builder.Store, claim.GenerationID)
	idx := New(handle, newTestRegistry(), config.Default().Index, zap.NewNop())
	idx.SetCensusCatalog(builder.Catalog)
	t.Cleanup(idx.Close)
	idx.SetRepoPrefix(request.RepoPrefix)
	idx.SetRootPath(request.RootPath)
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	counted := cleanCensusCounts.Load()

	result, err := idx.cleanCensusResult(ctx, 1, time.Now())
	assert.ErrorIs(t, err, context.Canceled)
	assert.Nil(t, result)
	assert.Equal(t, counted, cleanCensusCounts.Load())
	_, found, err := handle.GetRepoIndexState(request.RepoPrefix)
	require.NoError(t, err)
	assert.False(t, found)
}

func TestCleanCensusRefusesParentStateReadFailure(t *testing.T) {
	// Given a parent whose stored counts cannot be decoded, when the child has
	// no own state, then the read error refuses counting and child restamping.
	builder, request, claim, _ := claimedCopyFixture(t)
	require.NoError(t, generationAt(builder.Store, 0).SetRepoIndexState(graph.RepoIndexState{
		RepoPrefix: request.RepoPrefix, NodeCount: 7, EdgeCount: 11,
	}))
	db, err := sql.Open("sqlite", request.StorePath)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, db.Close()) })
	_, err = db.Exec("UPDATE repo_index_state SET node_count = 'unreadable' WHERE view_gen = 0 AND repo_prefix = ?", request.RepoPrefix)
	require.NoError(t, err)
	handle := generationAt(builder.Store, claim.GenerationID)
	idx := New(handle, newTestRegistry(), config.Default().Index, zap.NewNop())
	idx.SetCensusCatalog(builder.Catalog)
	t.Cleanup(idx.Close)
	idx.SetRepoPrefix(request.RepoPrefix)
	idx.SetRootPath(request.RootPath)
	counted := cleanCensusCounts.Load()

	result, err := idx.cleanCensusResult(t.Context(), 1, time.Now())
	require.Error(t, err)
	assert.Contains(t, err.Error(), "indexer: read clean census parent index state:")
	assert.Contains(t, err.Error(), "unreadable")
	assert.Nil(t, result)
	assert.Equal(t, counted, cleanCensusCounts.Load())
	_, found, err := handle.GetRepoIndexState(request.RepoPrefix)
	require.NoError(t, err)
	assert.False(t, found)
}

// The generation source must not turn an unavailable parent cache into a
// successful row count, which would hide the missing capability.
type censusParentWithoutStateReaderStore struct {
	generationPayloadStore
}

func (s *censusParentWithoutStateReaderStore) SharesSymbolSearchCore(any) bool { return true }
func (s *censusParentWithoutStateReaderStore) AtGenerationStore(int64) graph.Store {
	return &residualBatchCountingStore{Store: graph.New()}
}

func TestCleanCensusRefusesMissingGenerationCapabilities(t *testing.T) {
	for _, missing := range []string{"catalog", "parent reader"} {
		t.Run(missing, func(t *testing.T) {
			// Given a child without own state and a missing generation capability,
			// when a clean census runs, then it refuses without counting or restamping.
			builder, request, claim, _ := claimedCopyFixture(t)
			handle := generationAt(builder.Store, claim.GenerationID)
			store := &censusParentWithoutStateReaderStore{generationPayloadStore: handle}
			idx := New(store, newTestRegistry(), config.Default().Index, zap.NewNop())
			t.Cleanup(idx.Close)
			idx.SetRepoPrefix(request.RepoPrefix)
			idx.SetRootPath(request.RootPath)
			if missing == "parent reader" {
				idx.SetCensusCatalog(builder.Catalog)
			}
			counted := cleanCensusCounts.Load()
			result, err := idx.cleanCensusResult(t.Context(), 1, time.Now())
			require.ErrorIs(t, err, errors.ErrUnsupported)
			require.Nil(t, result)
			require.Equal(t, counted, cleanCensusCounts.Load())
			_, found, err := handle.GetRepoIndexState(request.RepoPrefix)
			require.NoError(t, err)
			require.False(t, found)
		})
	}
}
