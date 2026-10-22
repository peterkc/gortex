package indexer

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	"github.com/zzet/gortex/internal/config"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/graph/store_sqlite"
	"github.com/zzet/gortex/internal/parser"
	"github.com/zzet/gortex/internal/parser/languages"
	"github.com/zzet/gortex/internal/progress"
)

// Embed the real store so the shadow and its optional persistence capabilities
// stay intact. Only the drain boundaries are intercepted.
type firstIndexBulkProbe struct {
	*store_sqlite.Store
	beforeBegin func()
	duringDrain func()
	ftsErr      error
	panicDrain  bool
	panicFlush  bool
	begins      int
	flushes     int
	batches     int
}

func (s *firstIndexBulkProbe) BeginBulkLoad() {
	s.begins++
	if s.beforeBegin != nil {
		s.beforeBegin()
	}
	s.Store.BeginBulkLoad()
}

func (s *firstIndexBulkProbe) AddBatch(nodes []*graph.Node, edges []*graph.Edge) {
	s.batches++
	if s.duringDrain != nil {
		s.duringDrain()
	}
	if s.panicDrain {
		panic("injected drain panic")
	}
	s.Store.AddBatch(nodes, edges)
}

func (s *firstIndexBulkProbe) BatchUpsertSymbolFTS(items []graph.SymbolFTSItem) error {
	if s.ftsErr != nil {
		return s.ftsErr
	}
	return s.Store.BatchUpsertSymbolFTS(items)
}

func (s *firstIndexBulkProbe) FlushBulk() error {
	s.flushes++
	if s.panicFlush && s.flushes == 1 {
		panic("injected flush panic")
	}
	return s.Store.FlushBulk()
}

func newFirstIndexBulkFixture(t *testing.T) (*firstIndexBulkProbe, *sql.DB) {
	t.Helper()
	t.Setenv("GORTEX_SHADOW_MAX_FILES", "1000000")
	t.Setenv("GORTEX_SHADOW_MAX_BYTES", "1073741824")
	t.Setenv("GORTEX_STREAMING_FLUSH", "0")
	path := filepath.Join(t.TempDir(), "store.sqlite")
	store, err := store_sqlite.Open(path)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, store.Close()) })
	db, err := sql.Open("sqlite", "file:"+filepath.ToSlash(path)+"?mode=ro&_pragma=synchronous(NORMAL)")
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, db.Close()) })
	return &firstIndexBulkProbe{Store: store}, db
}

func firstIndexFixtureRepo(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "app.py"), "def fetch(url):\n    return url\n")
	return dir
}

func newFirstIndexBulkIndexer(t *testing.T, store graph.Store, prefix string) *Indexer {
	t.Helper()
	reg := parser.NewRegistry()
	reg.Register(languages.NewPythonExtractor())
	cfg := config.Default().Index
	cfg.Workers = 1
	idx := New(store, reg, cfg, zap.NewNop())
	idx.SetRepoPrefix(prefix)
	t.Cleanup(idx.Close)
	return idx
}

func firstIndexBulkActive(t *testing.T, store *store_sqlite.Store) bool {
	t.Helper()
	health, err := store.PlannerStatsHealth(context.Background())
	require.NoError(t, err)
	return health.Reason == "bulk_window_active"
}

func firstIndexSchemaIndexes(t *testing.T, db *sql.DB) []string {
	t.Helper()
	rows, err := db.Query("SELECT name FROM sqlite_schema WHERE type='index' ORDER BY name")
	require.NoError(t, err)
	defer func() { require.NoError(t, rows.Close()) }()
	var names []string
	for rows.Next() {
		var name string
		require.NoError(t, rows.Scan(&name))
		names = append(names, name)
	}
	require.NoError(t, rows.Err())
	return names
}

func assertFirstIndexNoReceipts(t *testing.T, store *store_sqlite.Store, prefix string) {
	t.Helper()
	_, found, err := store.GetRepoIndexState(prefix)
	assert.NoError(t, err)
	assert.False(t, found, "index state must not precede successful drain")
	assert.Empty(t, store.LoadFileMtimes(prefix), "mtimes must not precede successful drain")
}

func assertFirstIndexRestored(t *testing.T, store *store_sqlite.Store, db *sql.DB, indexes []string) {
	t.Helper()
	require.False(t, firstIndexBulkActive(t, store), "standalone window must be closed before restoration probes")
	assert.Equal(t, indexes, firstIndexSchemaIndexes(t, db))
	// This pooled read runs only after closure. The store package's own tests
	// separately check restoration on the actual pinned writer connection.
	var synchronous int
	require.NoError(t, db.QueryRow("PRAGMA synchronous").Scan(&synchronous))
	assert.Equal(t, 1, synchronous, "synchronous must be NORMAL after window closure")
}

func TestFirstIndexDrainUsesBulkWindow(t *testing.T) {
	probe, db := newFirstIndexBulkFixture(t)
	indexes := firstIndexSchemaIndexes(t, db)
	probe.beforeBegin = func() { assertFirstIndexNoReceipts(t, probe.Store, "repo") }
	probe.duringDrain = func() {
		assert.True(t, firstIndexBulkActive(t, probe.Store), "cold shadow drain must engage standalone bulk mode")
		assertFirstIndexNoReceipts(t, probe.Store, "repo")
		assert.NotEqual(t, indexes, firstIndexSchemaIndexes(t, db), "dense indexes must be deferred during drain")
	}
	idx := newFirstIndexBulkIndexer(t, probe, "repo")
	result, err := idx.IndexCtx(context.Background(), firstIndexFixtureRepo(t))
	require.NoError(t, err)
	require.Positive(t, result.NodeCount)
	require.Positive(t, probe.batches, "test must observe the shadow drain")
	assert.Equal(t, 1, probe.begins)
	assert.Equal(t, 1, probe.flushes)
	state, found, err := probe.GetRepoIndexState("repo")
	require.NoError(t, err)
	require.True(t, found)
	assert.Equal(t, result.NodeCount, state.NodeCount)
	assert.Equal(t, result.EdgeCount, state.EdgeCount)
	assert.Contains(t, probe.LoadFileMtimes("repo"), "app.py")
	assertFirstIndexRestored(t, probe.Store, db, indexes)
}

func TestSecondRepoWarmStoreNoBulkWindow(t *testing.T) {
	probe, db := newFirstIndexBulkFixture(t)
	_, err := newFirstIndexBulkIndexer(t, probe, "a").IndexCtx(context.Background(), firstIndexFixtureRepo(t))
	require.NoError(t, err)
	require.Positive(t, probe.NodeCount())
	indexes := firstIndexSchemaIndexes(t, db)
	probe.begins, probe.flushes, probe.batches = 0, 0, 0
	probe.duringDrain = func() { assertFirstIndexRestored(t, probe.Store, db, indexes) }
	result, err := newFirstIndexBulkIndexer(t, probe, "b").IndexCtx(context.Background(), firstIndexFixtureRepo(t))
	require.NoError(t, err)
	require.Positive(t, result.NodeCount)
	require.Positive(t, probe.batches)
	assert.Equal(t, 1, probe.begins)
	assert.Equal(t, 1, probe.flushes)
	assertFirstIndexRestored(t, probe.Store, db, indexes)
	assert.Contains(t, probe.LoadFileMtimes("a"), "app.py")
	assert.Contains(t, probe.LoadFileMtimes("b"), "app.py")
}

func TestOuterCoordinatedWindowNotClosedByRepo(t *testing.T) {
	probe, db := newFirstIndexBulkFixture(t)
	indexes := firstIndexSchemaIndexes(t, db)
	require.True(t, probe.BeginCoordinatedBulkLoad())
	t.Cleanup(func() { require.NoError(t, probe.EndCoordinatedBulkLoad()) })
	during := firstIndexSchemaIndexes(t, db)
	require.NotEqual(t, indexes, during)
	probe.duringDrain = func() {
		assert.True(t, firstIndexBulkActive(t, probe.Store))
		assert.Equal(t, during, firstIndexSchemaIndexes(t, db))
	}
	_, err := newFirstIndexBulkIndexer(t, probe, "repo").IndexCtx(context.Background(), firstIndexFixtureRepo(t))
	require.NoError(t, err)
	require.Positive(t, probe.batches)
	assert.Equal(t, 1, probe.begins)
	assert.Equal(t, 1, probe.flushes)
	assert.True(t, firstIndexBulkActive(t, probe.Store), "repository must not close outer coordinated window")
	assert.Equal(t, during, firstIndexSchemaIndexes(t, db))
	require.NoError(t, probe.EndCoordinatedBulkLoad())
	assertFirstIndexRestored(t, probe.Store, db, indexes)
}

type firstIndexCancelReporter struct{ cancel context.CancelFunc }

func (r firstIndexCancelReporter) Report(stage string, _, _ int) {
	if stage == "parsing" {
		r.cancel()
	}
}

type firstIndexPanicReporter struct{}

func (firstIndexPanicReporter) Report(stage string, _, _ int) {
	if stage == "building search index" {
		panic("injected body panic")
	}
}

func TestFirstIndexFailureRestoresStore(t *testing.T) {
	for _, mode := range []string{"drain error", "cancel before drain", "panic in body", "panic in drain", "panic in flush"} {
		t.Run(mode, func(t *testing.T) {
			probe, db := newFirstIndexBulkFixture(t)
			indexes := firstIndexSchemaIndexes(t, db)
			injected := errors.New("injected drain FTS error")
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			switch mode {
			case "drain error":
				probe.ftsErr = injected
			case "panic in drain":
				probe.panicDrain = true
			case "cancel before drain":
				ctx = progress.WithReporter(ctx, firstIndexCancelReporter{cancel: cancel})
			case "panic in body":
				ctx = progress.WithReporter(ctx, firstIndexPanicReporter{})
			case "panic in flush":
				probe.panicFlush = true
			}
			probe.duringDrain = func() {
				assert.True(t, firstIndexBulkActive(t, probe.Store), "failure fixture must exercise an engaged bulk window")
			}
			idx := newFirstIndexBulkIndexer(t, probe, "repo")
			dir := firstIndexFixtureRepo(t)
			switch mode {
			case "panic in body":
				assert.PanicsWithValue(t, "injected body panic", func() { _, _ = idx.IndexCtx(ctx, dir) })
				assert.Zero(t, probe.NodeCount(), "body panic must not drain shadow nodes")
				assert.Zero(t, probe.EdgeCount(), "body panic must not drain shadow edges")
			case "panic in drain":
				require.PanicsWithValue(t, "injected drain panic", func() { _, _ = idx.IndexCtx(ctx, dir) })
			case "panic in flush":
				require.PanicsWithValue(t, "injected flush panic", func() { _, _ = idx.IndexCtx(ctx, dir) })
				assert.Equal(t, 2, probe.flushes, "the deferred backstop must flush after a FlushBulk panic")
			case "drain error":
				_, err := idx.IndexCtx(ctx, dir)
				require.ErrorIs(t, err, injected)
			default:
				_, err := idx.IndexCtx(ctx, dir)
				require.ErrorIs(t, err, context.Canceled)
			}
			if mode == "cancel before drain" || mode == "panic in body" {
				assert.Zero(t, probe.begins)
				assert.Zero(t, probe.batches)
			} else {
				require.Positive(t, probe.batches)
				assert.Equal(t, 1, probe.begins)
			}
			assertFirstIndexRestored(t, probe.Store, db, indexes)
			assertFirstIndexNoReceipts(t, probe.Store, "repo")
			assert.Same(t, probe, idx.graph, "panic and error paths must restore disk target")
		})
	}
}
