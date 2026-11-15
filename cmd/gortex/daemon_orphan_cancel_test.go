package main

import (
	"context"
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
	"github.com/zzet/gortex/internal/indexer"
	"github.com/zzet/gortex/internal/parser"
)

// Admission is held before real SQL work. This distinguishes lost cancellation
// from an expensive purge already in progress; no large fixture is needed.
type admittedOrphanPurge struct {
	*store_sqlite.Store
	entered  chan struct{}
	release  chan struct{}
	once     sync.Once
	legacy   atomic.Int32
	mu       sync.Mutex
	prefixes []string
}

func (p *admittedOrphanPurge) admit() { p.once.Do(func() { close(p.entered) }); <-p.release }
func (p *admittedOrphanPurge) PurgeRepo(prefix string) error {
	p.record(prefix)
	p.legacy.Add(1)
	p.admit()
	return p.Store.PurgeRepo(prefix)
}
func (p *admittedOrphanPurge) PurgeRepoContext(ctx context.Context, prefix string) error {
	p.record(prefix)
	p.admit()
	if err := ctx.Err(); err != nil {
		return err
	}
	return p.Store.PurgeRepo(prefix)
}

func TestWarmupOrphanPurgeRetainsCancellationAtAdmission(t *testing.T) {
	root := t.TempDir()
	store, err := store_sqlite.Open(filepath.Join(root, "tiny.sqlite"))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, store.Close()) })
	const id = "gone/main.go::RetainedOnCancel"
	store.AddNode(&graph.Node{ID: id, Kind: graph.KindFunction, Name: "RetainedOnCancel", FilePath: "gone/main.go", RepoPrefix: "gone"})
	require.NotNil(t, store.GetNode(id))
	cm, err := config.NewConfigManager(filepath.Join(root, "config.yaml"))
	require.NoError(t, err)
	cm.Global().Repos = nil
	registry := parser.NewRegistry()
	mi := indexer.NewMultiIndexer(store, registry, nil, cm, zap.NewNop())
	t.Cleanup(func() { require.NoError(t, mi.Close(context.Background())) })
	purger := &admittedOrphanPurge{Store: store, entered: make(chan struct{}), release: make(chan struct{})}
	state := &daemonState{graph: purger, multiIndexer: mi, configManager: cm}
	task := newDaemonWarmup(context.Background())
	var releaseOnce sync.Once
	release := func() { releaseOnce.Do(func() { close(purger.release) }) }
	t.Cleanup(func() { task.cancel(); release(); task.wait() })
	require.True(t, task.start(func(ctx context.Context) { warmupDaemonStateContext(ctx, state, zap.NewNop(), nil, nil) }))
	select {
	case <-purger.entered:
	case <-time.After(2 * time.Second):
		t.Fatal("tiny orphan purge did not enter admission")
	}
	task.cancel()
	release()
	joined := make(chan struct{})
	go func() { task.wait(); close(joined) }()
	select {
	case <-joined:
	case <-time.After(2 * time.Second):
		t.Fatal("tiny canceled warmup did not join")
	}
	require.NotNil(t, store.GetNode(id), "canceled admission must not delete a real orphan row")
	require.Zero(t, purger.legacy.Load(), "context-aware maintenance must receive the producer lifetime")
}

func (p *admittedOrphanPurge) record(prefix string) {
	p.mu.Lock()
	p.prefixes = append(p.prefixes, prefix)
	p.mu.Unlock()
}

func TestWarmupOrphanPurgeLeavesClosingPrefixesToTheirSaga(t *testing.T) {
	// Given one closing prefix already absent from config and one genuine
	// unjournalled orphan, both still contain real generation-zero nodes.
	f := newDaemonCleanupFixture(t)
	removed, err := f.state.configManager.Global().RemoveRepoAndSaveIfPresent(f.root)
	require.NoError(t, err)
	require.True(t, removed)
	const orphanID = "abandoned/file.go::AbandonedSentinel"
	f.store.AddNode(&graph.Node{ID: orphanID, Name: "AbandonedSentinel", Kind: graph.KindFunction, FilePath: "abandoned/file.go", RepoPrefix: "abandoned"})
	purger := &admittedOrphanPurge{Store: f.store, entered: make(chan struct{}), release: make(chan struct{})}
	close(purger.release)
	f.restart(purger)
	require.NoError(t, f.state.lifecycle.Seed(t.Context()))

	// When orphan maintenance scans before readiness, no cleanup saga owns
	// the genuine orphan, but the durable closing owner reserves its prefix.
	watcher, timings := warmupDaemonStateContext(t.Context(), f.state, zap.NewNop(), nil, nil)
	if watcher != nil {
		t.Cleanup(func() { require.NoError(t, watcher.Stop()) })
	}

	// Then only the genuine orphan reaches physical purge. The pending saga
	// keeps both its payload and its journal for post-ready replay.
	require.NoError(t, timings.err)
	purger.mu.Lock()
	prefixes := append([]string(nil), purger.prefixes...)
	purger.mu.Unlock()
	require.Equal(t, []string{"abandoned"}, prefixes)
	require.NotNil(t, f.store.GetNode(f.nodeID))
	require.Nil(t, f.store.GetNode(orphanID))
	entries, err := f.store.Catalog().ListCleanupEntries(t.Context())
	require.NoError(t, err)
	require.NotEmpty(t, entries)
}
