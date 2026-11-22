package indexer

import (
	"context"
	"errors"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	"github.com/zzet/gortex/internal/config"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/graph/store_sqlite"
	"github.com/zzet/gortex/internal/parser"
	"github.com/zzet/gortex/internal/parser/languages"
	"github.com/zzet/gortex/internal/search"
)

// TestBulkLoad_PersistsVectorsToBackend guards the fix where the embedding
// pass run under the bulk-load shadow swap dropped the vector index on the
// floor. During a bulk index idx.graph points at the in-memory shadow, which
// does not implement graph.VectorSearcher — so the embedded vectors only ever
// reached the in-process HNSW and never the sqlite `vectors` table. They were
// lost on the next daemon restart, forcing a full (paid) re-embed every time.
//
// The fix prepares an immutable vector plan while the shadow is live, then
// installs it only after the graph has drained and idx.graph points back to the
// durable store. This test asserts both persistence and delegated publication.
func TestBulkLoad_PersistsVectorsToBackend(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "app.py"), `
def fetch(url):
    return url

def store(value):
    return value
`)

	sqliteDir := t.TempDir()
	store, err := store_sqlite.Open(filepath.Join(sqliteDir, "store.sqlite"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = store.Close() })

	// Sanity: sqlite is a BulkLoader (so the shadow swap engages, which is the
	// code path that regressed) and a VectorSearcher (so it CAN persist).
	_, isBulk := graph.Store(store).(graph.BulkLoader)
	require.True(t, isBulk, "sqlite must be a BulkLoader to exercise the shadow swap")
	_, isVec := graph.Store(store).(graph.VectorSearcher)
	require.True(t, isVec, "sqlite must be a VectorSearcher to persist embeddings")

	reg := parser.NewRegistry()
	reg.Register(languages.NewPythonExtractor())
	cfg := config.Default().Index
	cfg.Workers = 2
	idx := New(store, reg, cfg, zap.NewNop())
	idx.SetEmbedder(&poolEmbedder{})

	_, err = idx.IndexCtx(context.Background(), dir)
	require.NoError(t, err)

	var allIDs []string
	for _, n := range store.AllNodes() {
		allIDs = append(allIDs, n.ID)
	}
	require.NotEmpty(t, allIDs, "the index must have produced nodes")

	embs := store.GetEmbeddings(allIDs)
	require.NotEmpty(t, embs,
		"embedded vectors must be persisted to the sqlite backend under the bulk loader "+
			"(regression: vectors lived only in the in-process HNSW and were lost on restart)")
	assertDelegatedVectorPublication(t, idx)
}

type vectorPublicationProbeStore struct {
	*store_sqlite.Store
	flushStarted  chan struct{}
	flushRelease  chan struct{}
	replaceCalled chan struct{}
	flushOnce     sync.Once
	replaceOnce   sync.Once
	flushErr      error
	replaceErr    error
	replaceCalls  atomic.Int32
}

func newVectorPublicationProbeStore(store *store_sqlite.Store) *vectorPublicationProbeStore {
	return &vectorPublicationProbeStore{
		Store:         store,
		flushStarted:  make(chan struct{}),
		replaceCalled: make(chan struct{}),
	}
}

func (s *vectorPublicationProbeStore) FlushBulk() error {
	s.flushOnce.Do(func() { close(s.flushStarted) })
	if s.flushRelease != nil {
		<-s.flushRelease
	}
	if s.flushErr != nil {
		return s.flushErr
	}
	return s.Store.FlushBulk()
}

func (s *vectorPublicationProbeStore) ReplaceVectorCorpus(
	ctx context.Context,
	repoPrefix string,
	dims int,
	items []graph.VectorCorpusItem,
) (graph.VectorCorpusStats, error) {
	s.replaceCalls.Add(1)
	s.replaceOnce.Do(func() { close(s.replaceCalled) })
	if s.replaceErr != nil {
		return graph.VectorCorpusStats{}, s.replaceErr
	}
	return s.Store.ReplaceVectorCorpus(ctx, repoPrefix, dims, items)
}

func vectorPersistFixture(t *testing.T, files int) string {
	t.Helper()
	dir := t.TempDir()
	for i := 0; i < files; i++ {
		writeFile(t, filepath.Join(dir, "app"+string(rune('a'+i))+".py"), `
def fetch(url):
    return url

def store(value):
    return value
`)
	}
	return dir
}

func newVectorPersistIndexer(t *testing.T, store graph.Store, emb *poolEmbedder) *Indexer {
	t.Helper()
	reg := parser.NewRegistry()
	reg.Register(languages.NewPythonExtractor())
	cfg := config.Default().Index
	cfg.Workers = 2
	idx := New(store, reg, cfg, zap.NewNop())
	idx.SetEmbedder(emb)
	return idx
}

func assertDelegatedVectorPublication(t *testing.T, idx *Indexer) {
	t.Helper()
	backend, release := idx.swappable().AcquireBackend()
	defer release()
	hybrid, ok := backend.(*search.HybridBackend)
	require.True(t, ok, "vector publication must install a hybrid backend")
	require.NotNil(t, hybrid.VectorIndex())
	require.Greater(t, hybrid.VectorIndex().Count(), 0)
	require.Zero(t, hybrid.VectorSizeBytes(), "durable delegation must retain no heap HNSW")
}

func waitForVectorPublicationSignal(t *testing.T, signal <-chan struct{}, label string) {
	t.Helper()
	select {
	case <-signal:
	case <-time.After(10 * time.Second):
		t.Fatalf("timed out waiting for %s", label)
	}
}

func TestBulkLoad_PublishesVectorsOnlyAfterDurableFlush(t *testing.T) {
	t.Setenv("GORTEX_SHADOW_MAX_FILES", "1000000")
	t.Setenv("GORTEX_SHADOW_MAX_BYTES", "1073741824")
	t.Setenv("GORTEX_STREAMING_FLUSH", "0")
	dir := vectorPersistFixture(t, 1)
	base, err := store_sqlite.Open(filepath.Join(t.TempDir(), "store.sqlite"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = base.Close() })
	probe := newVectorPublicationProbeStore(base)
	probe.flushRelease = make(chan struct{})
	idx := newVectorPersistIndexer(t, probe, &poolEmbedder{})

	before, release := idx.swappable().AcquireBackend()
	release()
	done := make(chan error, 1)
	go func() {
		_, indexErr := idx.IndexCtx(context.Background(), dir)
		done <- indexErr
	}()

	waitForVectorPublicationSignal(t, probe.flushStarted, "bulk flush")
	select {
	case <-probe.replaceCalled:
		t.Fatal("vector corpus was installed before the shadow graph completed its durable flush")
	default:
	}
	during, release := idx.swappable().AcquireBackend()
	release()
	if during != before {
		t.Fatal("active search backend changed before durable graph flush")
	}

	close(probe.flushRelease)
	select {
	case indexErr := <-done:
		require.NoError(t, indexErr)
	case <-time.After(10 * time.Second):
		t.Fatal("index did not finish after releasing the durable flush")
	}
	waitForVectorPublicationSignal(t, probe.replaceCalled, "vector corpus replacement")
	require.Equal(t, int32(1), probe.replaceCalls.Load())
	assertDelegatedVectorPublication(t, idx)
}

func TestBulkLoad_FlushFailureDoesNotPublishVectors(t *testing.T) {
	t.Setenv("GORTEX_SHADOW_MAX_FILES", "1000000")
	t.Setenv("GORTEX_SHADOW_MAX_BYTES", "1073741824")
	t.Setenv("GORTEX_STREAMING_FLUSH", "0")
	dir := vectorPersistFixture(t, 1)
	base, err := store_sqlite.Open(filepath.Join(t.TempDir(), "store.sqlite"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = base.Close() })
	probe := newVectorPublicationProbeStore(base)
	probe.flushErr = errors.New("injected flush failure")
	idx := newVectorPersistIndexer(t, probe, &poolEmbedder{})
	before, release := idx.swappable().AcquireBackend()
	release()

	_, err = idx.IndexCtx(context.Background(), dir)
	require.ErrorIs(t, err, probe.flushErr)
	require.Zero(t, probe.replaceCalls.Load(), "failed graph persistence must not install vectors")
	after, release := idx.swappable().AcquireBackend()
	release()
	if after != before {
		t.Fatal("failed shadow drain changed the active search backend")
	}
}

func TestBulkLoad_NativeVectorFailureDoesNotPublish(t *testing.T) {
	t.Setenv("GORTEX_SHADOW_MAX_FILES", "1000000")
	t.Setenv("GORTEX_SHADOW_MAX_BYTES", "1073741824")
	t.Setenv("GORTEX_STREAMING_FLUSH", "0")
	dir := vectorPersistFixture(t, 1)
	base, err := store_sqlite.Open(filepath.Join(t.TempDir(), "store.sqlite"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = base.Close() })
	probe := newVectorPublicationProbeStore(base)
	probe.replaceErr = errors.New("injected vector replacement failure")
	emb := &poolEmbedder{}
	idx := newVectorPersistIndexer(t, probe, emb)

	before, release := idx.swappable().AcquireBackend()
	release()
	_, err = idx.IndexCtx(t.Context(), dir)
	require.NoError(t, err, "the graph build remains usable after an optional vector failure")
	require.Positive(t, base.NodeCount())
	require.Equal(t, int32(1), emb.calls, "failed installation must not re-embed")
	require.Equal(t, int32(1), probe.replaceCalls.Load())
	require.ErrorIs(t, idx.LastVectorBuildError(), ErrVectorInstall)
	require.ErrorIs(t, idx.LastVectorBuildError(), probe.replaceErr)
	stats, err := base.VectorCorpusStats(t.Context(), 3)
	require.NoError(t, err)
	require.Zero(t, stats.VectorCount)

	after, release := idx.swappable().AcquireBackend()
	defer release()
	require.Same(t, before, after, "a failed vector replacement must not publish a search backend")
}

func TestDirectSQLiteIndexPublishesDelegatedVectorCorpus(t *testing.T) {
	t.Setenv("GORTEX_SHADOW_MAX_FILES", "0")
	t.Setenv("GORTEX_STREAMING_FLUSH", "0")
	dir := vectorPersistFixture(t, 1)
	base, err := store_sqlite.Open(filepath.Join(t.TempDir(), "store.sqlite"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = base.Close() })
	probe := newVectorPublicationProbeStore(base)
	idx := newVectorPersistIndexer(t, probe, &poolEmbedder{})

	_, err = idx.IndexCtx(context.Background(), dir)
	require.NoError(t, err)
	require.Equal(t, int32(1), probe.replaceCalls.Load())
	assertDelegatedVectorPublication(t, idx)
}

func TestStreamingFlush_PublishesOneDelegatedVectorCorpus(t *testing.T) {
	t.Setenv("GORTEX_SHADOW_MAX_FILES", "0")
	t.Setenv("GORTEX_STREAMING_FLUSH", "1")
	t.Setenv("GORTEX_STREAMING_CHUNK_SIZE", "1")
	dir := vectorPersistFixture(t, 2)
	base, err := store_sqlite.Open(filepath.Join(t.TempDir(), "store.sqlite"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = base.Close() })
	probe := newVectorPublicationProbeStore(base)
	idx := newVectorPersistIndexer(t, probe, &poolEmbedder{})

	_, err = idx.IndexCtx(context.Background(), dir)
	require.NoError(t, err)
	require.Equal(t, int32(1), probe.replaceCalls.Load(),
		"streaming chunks must prepare once and publish only after the final chunk")
	assertDelegatedVectorPublication(t, idx)
}
