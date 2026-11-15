package main

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/mark3labs/mcp-go/mcp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	"go.uber.org/zap/zaptest/observer"

	"github.com/zzet/gortex/internal/config"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/graph/store_sqlite"
	"github.com/zzet/gortex/internal/graphview"
	"github.com/zzet/gortex/internal/indexer"
	gortexmcp "github.com/zzet/gortex/internal/mcp"
	"github.com/zzet/gortex/internal/parser"
	"github.com/zzet/gortex/internal/parser/languages"
	"github.com/zzet/gortex/internal/query"
)

// Fail one resolver scan without closing the store underneath cleanup,
// watchers, or other background work.
type warmupResolveFailureStore struct {
	*store_sqlite.Store
	failScan         atomic.Bool
	resolveErr       error
	refuseIncoming   atomic.Bool
	incomingRefusals atomic.Int64
	forceImpact      atomic.Bool
	failCapability   atomic.Bool
}

func (s *warmupResolveFailureStore) EvictEdgesFromSourcesByKinds(ctx context.Context, sources []string, kinds []graph.EdgeKind) (int, error) {
	if s.failCapability.Load() {
		return 0, errors.ErrUnsupported
	}
	return s.Store.EvictEdgesFromSourcesByKinds(ctx, sources, kinds)
}

func (s *warmupResolveFailureStore) BeginUnresolvedEdgeScan(ctx context.Context) (graph.UnresolvedEdgeScan, error) {
	if s.failScan.Swap(false) {
		return graph.UnresolvedEdgeScan{}, s.resolveErr
	}
	return s.Store.BeginUnresolvedEdgeScan(ctx)
}

func (s *warmupResolveFailureStore) GetInEdgesByNodeIDs(ids []string) map[string][]*graph.Edge {
	rows := s.Store.GetInEdgesByNodeIDs(ids)
	if s.refuseIncoming.Load() && len(ids) > 0 {
		s.incomingRefusals.Add(1)
		edge := &graph.Edge{From: "caller", To: ids[0], Kind: graph.EdgeCalls}
		batch := make([]*graph.Edge, graph.MaxIncomingSourceCandidateRows+1)
		for i := range batch {
			batch[i] = edge
		}
		rows[ids[0]] = batch
	}
	return rows
}

func (s *warmupResolveFailureStore) BackfillWorkspaceSlugsWithImpact(slugs []graph.WorkspaceSlug) graph.WorkspaceSlugBackfillResult {
	result := s.Store.BackfillWorkspaceSlugsWithImpact(slugs)
	if s.forceImpact.Swap(false) {
		// Select the lifecycle-owned full pass without changing storage behavior.
		result.Changed++
		result.ResolutionAffected++
	}
	return result
}

type warmupReadinessClient struct {
	notifications chan mcp.JSONRPCNotification
}

func (c *warmupReadinessClient) Initialize()       {}
func (c *warmupReadinessClient) Initialized() bool { return true }
func (c *warmupReadinessClient) SessionID() string { return "warmup-test" }
func (c *warmupReadinessClient) NotificationChannel() chan<- mcp.JSONRPCNotification {
	return c.notifications
}

func TestWarmupCompletion(t *testing.T) {
	for _, tc := range []struct {
		name            string
		failBeforeReady bool
		failAfterReady  bool
		resolveStep     string
		failLate        bool
	}{
		{name: "success"},
		{name: "resolve_failure_after_ready", failAfterReady: true},
		{name: "resolve_failure_before_ready", failBeforeReady: true},
		{name: "global_resolve_success", resolveStep: "workspace_backfill_before_derived"},
		{name: "global_resolve_failure", resolveStep: "workspace_backfill_before_derived", failLate: true},
		{name: "post_batch_resolve_success", resolveStep: "workspace_backfill_after_derived"},
		{name: "post_batch_resolve_failure", resolveStep: "workspace_backfill_after_derived", failLate: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			// Given a warmup whose selected resolve pass either succeeds or fails.
			failResolve := tc.failBeforeReady || tc.failAfterReady || tc.failLate
			t.Setenv("GORTEX_ENRICH_OVERLAP", "1")
			dir := t.TempDir()
			repo := filepath.Join(dir, "repo")
			require.NoError(t, os.Mkdir(repo, 0o755))
			require.NoError(t, os.WriteFile(filepath.Join(repo, "main.go"), []byte("package main\nfunc main() { missing() }\n"), 0o644))
			backend, err := store_sqlite.Open(filepath.Join(dir, "store.sqlite"))
			require.NoError(t, err)
			t.Cleanup(func() { require.NoError(t, backend.Close()) })
			resolveErr := errors.New("injected resolve failure: sql: database is closed")
			g := &warmupResolveFailureStore{Store: backend, resolveErr: resolveErr}
			g.failScan.Store(tc.failBeforeReady)
			cm, err := config.NewConfigManager(filepath.Join(dir, "config.yaml"))
			require.NoError(t, err)
			require.NoError(t, cm.Global().AddRepo(config.RepoEntry{Path: repo}))
			core, logs := observer.New(zap.InfoLevel)
			logger := zap.New(core, zap.Hooks(func(entry zapcore.Entry) error {
				if tc.resolveStep != "" && entry.Message == "daemon: warmup step start" {
					steps := logs.FilterMessage(entry.Message).All()
					if steps[len(steps)-1].ContextMap()["step"] == tc.resolveStep {
						g.forceImpact.Store(true)
						g.failScan.Store(tc.failLate)
					}
				}
				return nil
			}))
			reg := parser.NewRegistry()
			reg.Register(languages.NewGoExtractor())
			idx := indexer.New(g, reg, config.Default().Index, logger)
			mi := indexer.NewMultiIndexer(g, reg, idx.Search(), cm, logger)
			t.Cleanup(func() { require.NoError(t, mi.Close(context.Background())) })
			srv := gortexmcp.NewServer(query.NewEngine(g), g, idx, nil, logger, nil)
			state := &daemonState{graph: g, indexer: idx, multiIndexer: mi, configManager: cm, mcpServer: srv}
			controller := &realController{graph: g}
			markReady := sync.OnceFunc(func() {
				controller.MarkReady(time.Millisecond)
				g.failScan.Store(tc.failAfterReady)
			})

			// When warmup joins its phases and the final outcome is published.
			mw, warmup := warmupDaemonState(state, logger, markReady)
			if mw != nil {
				t.Cleanup(func() { require.NoError(t, mw.Stop()) })
			}
			require.Equal(t, !tc.failBeforeReady, controller.ready.Load(), "the before-ready failure must skip the compute-ready callback")
			client := &warmupReadinessClient{notifications: make(chan mcp.JSONRPCNotification, 16)}
			require.NoError(t, srv.MCPServer().RegisterSession(context.Background(), client))
			t.Cleanup(func() { srv.MCPServer().UnregisterSession(context.Background(), client.SessionID()) })
			ctx := gortexmcp.WithSessionID(context.Background(), client.SessionID())
			srv.MCPServer().HandleMessage(ctx, json.RawMessage(`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"subscribe_workspace_readiness","arguments":{}}}`))
			require.Len(t, client.notifications, 1, "subscription must replay the latest warmup phase")
			<-client.notifications
			viewBuilds := indexer.NewViewBuildGate()
			publishWarmupCompletion(context.Background(), state, controller, logger, viewBuilds, warmup, time.Millisecond, time.Second)

			// Then only successful resolution can report successful enrichment.
			assert.True(t, controller.ready.Load(), "completion must leave the daemon queryable, even if compute-ready was skipped")
			assert.True(t, viewBuilds.IsOpen(), "warmup must release waiting view builds on either outcome")
			require.Len(t, logs.FilterMessage("daemon: enrichment apply gate opened").All(), 1)
			phase, _ := srv.ReadinessPhase()
			if failResolve {
				failureMessage := "daemon: warmup resolve failed"
				if tc.failLate {
					failureMessage = "daemon: warmup global resolve incomplete"
					if tc.resolveStep == "workspace_backfill_after_derived" {
						failureMessage = "daemon: warmup post-batch resolve incomplete"
					}
				}
				require.Len(t, logs.FilterMessage(failureMessage).All(), 1, "injection must reach the selected real resolver")
				assert.ErrorIs(t, warmup.resolveErr, resolveErr)
				assert.True(t, mi.RelationsPending(), "failed resolution must not certify startup relations")
				assert.False(t, controller.enriched.Load())
				// Then health retains the same queryable but unenriched outcome.
				health := buildDaemonHealthSnapshot(time.Now(), controller, state, nil)
				assert.Equal(t, true, health["ready"])
				assert.Equal(t, false, health["enriched"])
				assert.Equal(t, "warmup_incomplete", phase)
				assert.Empty(t, logs.FilterMessage("daemon: enrichment complete").All())
				assert.Len(t, logs.FilterMessage("daemon: warmup summary").All(), 1)
				require.Len(t, client.notifications, 1, "failed warmup must publish exactly one terminal failure event")
				notification := <-client.notifications
				assert.Equal(t, "notifications/workspace_readiness", notification.Method)
				assert.Equal(t, "warmup_incomplete", notification.Params.AdditionalFields["phase"])
				assert.Equal(t, true, notification.Params.AdditionalFields["ready"])
				assert.Equal(t, false, notification.Params.AdditionalFields["enriched"])
				assert.Equal(t, "warmup resolve failed; check the daemon log", notification.Params.AdditionalFields["error"])
				entries := logs.FilterMessage("daemon: warmup incomplete").All()
				require.Len(t, entries, 1)
				assert.Equal(t, resolveErr.Error(), entries[0].ContextMap()["error"])
			} else {
				assert.NoError(t, warmup.resolveErr)
				assert.False(t, mi.RelationsPending())
				if tc.resolveStep == "workspace_backfill_after_derived" {
					assert.Len(t, logs.FilterMessage("daemon: post-batch workspace eligibility change forced full cross-repo catch-up").All(), 1)
				}
				require.Len(t, client.notifications, 1)
				notification := <-client.notifications
				assert.Equal(t, "notifications/workspace_readiness", notification.Method)
				assert.Equal(t, "enrichment_complete", notification.Params.AdditionalFields["phase"])
				assert.Equal(t, true, notification.Params.AdditionalFields["enriched"])
				assert.True(t, controller.enriched.Load())
				assert.Equal(t, "enrichment_complete", phase)
				assert.Len(t, logs.FilterMessage("daemon: enrichment complete").All(), 1)
				assert.Len(t, logs.FilterMessage("daemon: warmup summary").All(), 1)
				assert.Empty(t, logs.FilterMessage("daemon: warmup incomplete").All())
			}
		})
	}
}

func TestWarmupCompletionAfterExactFrontierRefusal(t *testing.T) {
	for _, phase := range []string{"same_repo", "cross_repo", "capability"} {
		t.Run(phase, func(t *testing.T) {
			// Given a persisted repository with one offline file change.
			t.Setenv("GORTEX_ENRICH_OVERLAP", "1")
			dir := t.TempDir()
			repo := filepath.Join(dir, "repo")
			require.NoError(t, os.Mkdir(repo, 0o755))
			for _, name := range []string{"target", "a", "b", "c", "d", "e"} {
				require.NoError(t, os.WriteFile(filepath.Join(repo, name+".go"), []byte("package repo\nfunc "+name+"() {}\n"), 0o644))
			}
			backend, err := store_sqlite.Open(filepath.Join(dir, "store.sqlite"))
			require.NoError(t, err)
			t.Cleanup(func() { require.NoError(t, backend.Close()) })
			g := &warmupResolveFailureStore{Store: backend}
			cm, err := config.NewConfigManager(filepath.Join(dir, "config.yaml"))
			require.NoError(t, err)
			entry := config.RepoEntry{Path: repo, Name: "repo"}
			require.NoError(t, cm.Global().AddRepo(entry))
			reg := parser.NewRegistry()
			reg.Register(languages.NewGoExtractor())
			idx := indexer.New(g, reg, config.Default().Index, zap.NewNop())
			cold := indexer.NewMultiIndexer(g, reg, idx.Search(), cm, zap.NewNop())
			t.Cleanup(func() { require.NoError(t, cold.Close(context.Background())) })
			_, err = cold.TrackRepoCtx(context.Background(), entry)
			require.NoError(t, err)
			require.NoError(t, cold.Close(context.Background()))
			path := filepath.Join(repo, "target.go")
			require.NoError(t, os.WriteFile(path, []byte("package repo\nfunc target() { a() }\n"), 0o644))
			future := time.Now().Add(5 * time.Second)
			require.NoError(t, os.Chtimes(path, future, future))
			core, logs := observer.New(zap.InfoLevel)
			logger := zap.New(core, zap.Hooks(func(entry zapcore.Entry) error {
				if entry.Message == "daemon: resolve readiness phase published" {
					g.refuseIncoming.Store(phase == "same_repo")
				}
				return nil
			}))
			mi := indexer.NewMultiIndexer(g, reg, idx.Search(), cm, logger)
			t.Cleanup(func() { require.NoError(t, mi.Close(context.Background())) })
			srv := gortexmcp.NewServer(query.NewEngine(g), g, idx, nil, logger, nil)
			state := &daemonState{graph: g, indexer: idx, multiIndexer: mi, configManager: cm, mcpServer: srv}
			controller := &realController{graph: g}
			client := &warmupReadinessClient{notifications: make(chan mcp.JSONRPCNotification, 64)}
			require.NoError(t, srv.MCPServer().RegisterSession(t.Context(), client))
			t.Cleanup(func() { srv.MCPServer().UnregisterSession(context.Background(), client.SessionID()) })
			ctx := gortexmcp.WithSessionID(t.Context(), client.SessionID())
			srv.MCPServer().HandleMessage(ctx, json.RawMessage(`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"subscribe_workspace_readiness","arguments":{}}}`))
			var readyCalls atomic.Int64
			markReady := sync.OnceFunc(func() {
				readyCalls.Add(1)
				controller.MarkReady(time.Millisecond)
				publishReadinessPhase(state, "ready", true, nil)
				g.refuseIncoming.Store(phase == "cross_repo")
				g.failCapability.Store(phase == "capability")
			})

			// When exact warmup encounters a bounded resolve refusal or a capability write failure.
			mw, warmup := warmupDaemonStateContext(t.Context(), state, logger, markReady, func(context.Context, *indexer.MultiWatcher) error { return nil })
			if mw != nil {
				t.Cleanup(func() { require.NoError(t, mw.Stop()) })
			}

			// Then resolve fallback or best-effort derived work leaves warmup queryable.
			require.NoError(t, warmup.resolveErr)
			if phase == "capability" {
				// Given a failed capability write, when exact warmup joins its
				// best-effort passes, then its report is incomplete, not silently complete.
				require.Len(t, logs.FilterMessage("incremental derived passes refused").All(), 1)
				require.Len(t, logs.FilterMessage("daemon: exact warm derived passes incomplete").All(), 1)
				require.Empty(t, logs.FilterMessage("daemon: exact warm derived passes complete").All())
			} else {
				require.Positive(t, g.incomingRefusals.Load(), "the bounded refusal must be exercised")
			}
			frontiers := logs.FilterMessage("daemon: warmup delta frontier").All()
			require.Len(t, frontiers, 1)
			require.Equal(t, true, frontiers[0].ContextMap()["exact"])
			require.EqualValues(t, 1, readyCalls.Load())
			require.True(t, controller.ready.Load())
			require.False(t, mi.RelationsPending())
			var phases []string
			for len(client.notifications) > 0 {
				notification := <-client.notifications
				phases = append(phases, notification.Params.AdditionalFields["phase"].(string))
			}
			readyPhase := slices.Index(phases, "ready")
			require.NotEqual(t, -1, readyPhase, "ready must be published after escalation")
			require.Less(t, readyPhase, slices.Index(phases, "resolve_done"), "compute-ready must precede the resolve tail")
			publishWarmupCompletion(t.Context(), state, controller, logger, indexer.NewViewBuildGate(), warmup, time.Millisecond, time.Second)
			phaseName, _ := srv.ReadinessPhase()
			require.Equal(t, "enrichment_complete", phaseName)
			require.True(t, controller.enriched.Load())
		})
	}
}

func TestWarmupCompletionStopsOnCanceledContext(t *testing.T) {
	for _, tc := range []struct {
		name       string
		resolveErr error
	}{
		{name: "success"},
		{name: "resolve_failure", resolveErr: errors.New("injected resolve failure")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			// Given a startup context canceled after the warmup tail joined.
			ctx, cancel := context.WithCancel(t.Context())
			cancel()
			core, logs := observer.New(zap.InfoLevel)
			controller := &realController{}
			viewBuilds := indexer.NewViewBuildGate()

			// When the final publisher runs.
			publishWarmupCompletion(ctx, &daemonState{}, controller, zap.New(core), viewBuilds, &warmupTimings{resolveErr: tc.resolveErr}, time.Millisecond, time.Second)

			// Then view admission stays closed and no outcome is published.
			assert.False(t, viewBuilds.IsOpen(), "a canceled startup must not admit view builds")
			assert.Zero(t, logs.Len(), "a canceled startup must not log a warmup outcome")
		})
	}
}

func TestWarmupCompletionRefusesUnsupportedWorkspaceBackfill(t *testing.T) {
	// Given a configured repository behind a store with no slug backfiller,
	// when isolated warmup joins, then readiness remains possible but enrichment
	// and cross-workspace completion are not claimed.
	dir := t.TempDir()
	repo := filepath.Join(dir, "repo")
	require.NoError(t, os.Mkdir(repo, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(repo, "main.go"), []byte("package main\nfunc main() {}\n"), 0o644))
	backend, err := store_sqlite.Open(filepath.Join(dir, "store.sqlite"))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, backend.Close()) })
	store := &struct{ graph.Store }{Store: backend}
	cm, err := config.NewConfigManager(filepath.Join(dir, "config.yaml"))
	require.NoError(t, err)
	require.NoError(t, cm.Global().AddRepo(config.RepoEntry{Path: repo, Workspace: "shared"}))
	core, logs := observer.New(zap.InfoLevel)
	logger := zap.New(core)
	reg := parser.NewRegistry()
	reg.Register(languages.NewGoExtractor())
	idx := indexer.New(store, reg, config.Default().Index, logger)
	mi := indexer.NewMultiIndexer(store, reg, idx.Search(), cm, logger)
	t.Cleanup(func() { require.NoError(t, mi.Close(context.Background())) })
	srv := gortexmcp.NewServer(query.NewEngine(store), store, idx, nil, logger, nil)
	state := &daemonState{graph: store, indexer: idx, multiIndexer: mi, configManager: cm, mcpServer: srv}
	controller := &realController{graph: store}
	mw, warmup := warmupDaemonStateContext(t.Context(), state, logger, sync.OnceFunc(func() { controller.MarkReady(time.Millisecond) }), func(context.Context, *indexer.MultiWatcher) error { return nil })
	if mw != nil {
		t.Cleanup(func() { require.NoError(t, mw.Stop()) })
	}
	require.ErrorIs(t, warmup.resolveErr, errors.ErrUnsupported)
	require.Len(t, logs.FilterMessage("daemon: workspace slug backfill incomplete").All(), 2)
	refusedSteps := 0
	for _, entry := range logs.FilterMessage("daemon: warmup step complete").All() {
		step := entry.ContextMap()["step"]
		if step == "workspace_backfill_before_derived" || step == "workspace_backfill_after_derived" {
			refusedSteps++
			require.Equal(t, "incomplete", entry.ContextMap()["outcome"])
			require.NotEmpty(t, entry.ContextMap()["error"])
		}
	}
	require.Equal(t, 2, refusedSteps)
	require.Empty(t, logs.FilterMessage("daemon: warmup global resolve incomplete").All(), "failed stamping must not continue cross-workspace resolution")
	for _, entry := range logs.FilterMessage("daemon: warmup phase start").All() {
		require.NotEqual(t, "global_resolve", entry.ContextMap()["phase"])
	}
	publishWarmupCompletion(t.Context(), state, controller, logger, indexer.NewViewBuildGate(), warmup, time.Millisecond, time.Second)
	require.True(t, controller.ready.Load())
	require.False(t, controller.enriched.Load())
	require.True(t, mi.RelationsPending())
	phase, _ := srv.ReadinessPhase()
	require.Equal(t, "warmup_incomplete", phase)
	require.Empty(t, logs.FilterMessage("daemon: enrichment complete").All())
}

// daemonCleanupFixture keeps a real interrupted untrack and its stale config
// across a fresh MI/lifecycle, as daemon startup does. It never opens readiness
// during setup, so no physical cleanup can escape the test's observations.
type daemonCleanupFixture struct {
	t       *testing.T
	store   *store_sqlite.Store
	state   *daemonState
	root    string
	tracked indexer.RegisterResult
	nodeID  string
}

func newDaemonCleanupFixture(t *testing.T) *daemonCleanupFixture {
	t.Helper()
	base := t.TempDir()
	store, err := store_sqlite.Open(filepath.Join(base, "cleanup.sqlite"))
	require.NoError(t, err)
	cm, err := config.NewConfigManager(filepath.Join(base, "config.yaml"))
	require.NoError(t, err)
	f := &daemonCleanupFixture{t: t, store: store}
	f.open(store, cm)
	t.Cleanup(func() {
		require.NoError(t, f.state.lifecycle.Close())
		require.NoError(t, f.state.multiIndexer.Close(context.Background()))
		require.NoError(t, f.store.Close())
	})
	f.root = startupPublicationRepo(t, base, "closing")
	f.tracked, err = f.state.lifecycle.Register(t.Context(), config.RepoEntry{Path: f.root, Name: "closing"}, indexer.TrackSourceCLI)
	require.NoError(t, err)
	require.NoError(t, f.tracked.CatalogErr)
	f.nodeID = "closing/pending.go::PendingOldSentinel"
	store.AddNode(&graph.Node{ID: f.nodeID, Name: "PendingOldSentinel", Kind: graph.KindFunction, FilePath: "closing/pending.go", RepoPrefix: "closing"})
	out, err := f.state.lifecycle.Untrack(t.Context(), f.root)
	require.NoError(t, err)
	require.True(t, out.Pending)
	return f
}

func (f *daemonCleanupFixture) open(g graph.Store, cm *config.ConfigManager) {
	f.t.Helper()
	registry := parser.NewRegistry()
	registry.Register(languages.NewGoExtractor())
	logger := zap.NewNop()
	idx := indexer.New(g, registry, config.Default().Index, logger)
	mi := indexer.NewMultiIndexer(g, registry, idx.Search(), cm, logger)
	lifecycle, err := indexer.NewCheckoutLifecycle(indexer.CheckoutLifecycleConfig{
		MultiIndexer: mi, ConfigManager: cm, Graph: f.store, Logger: logger,
	})
	require.NoError(f.t, err)
	lifecycle.EnableDeferredRepositoryCleanup()
	lifecycle.EnableDeferredSeedRetirements()
	srv := gortexmcp.NewServer(query.NewEngine(g), g, idx, nil, logger, nil, gortexmcp.MultiRepoOptions{
		MultiIndexer: mi, ConfigManager: cm, CheckoutLifecycle: lifecycle,
	})
	f.state = &daemonState{graph: g, indexer: idx, multiIndexer: mi, configManager: cm, lifecycle: lifecycle, mcpServer: srv}
}

func (f *daemonCleanupFixture) restart(g graph.Store) {
	f.t.Helper()
	require.NoError(f.t, f.state.lifecycle.Close())
	require.NoError(f.t, f.state.multiIndexer.Close(context.Background()))
	cm, err := config.NewConfigManager(f.state.configManager.Global().ConfigPath())
	require.NoError(f.t, err)
	f.open(g, cm)
}

type cleanupWarmupMtimes struct {
	*store_sqlite.Store
	loads atomic.Int64
}

func (s *cleanupWarmupMtimes) LoadFileMtimes(prefix string) map[string]int64 {
	s.loads.Add(1)
	return s.Store.LoadFileMtimes(prefix)
}

func TestWarmupPendingCleanupRefusesWarmAndColdConfiguredTargets(t *testing.T) {
	for _, mode := range []string{"warm", "cold"} {
		t.Run(mode, func(t *testing.T) {
			// Given stale persisted config whose closing graph still owns its
			// prefix, with either warm sidecar data or no prior mtimes.
			f := newDaemonCleanupFixture(t)
			if mode == "cold" {
				require.NoError(t, f.store.ReplaceFileMtimes(f.tracked.Prefix, nil))
			} else {
				require.NotEmpty(t, f.store.LoadFileMtimes(f.tracked.Prefix))
			}
			g := &cleanupWarmupMtimes{Store: f.store}
			f.restart(g)
			require.NoError(t, f.state.lifecycle.Seed(t.Context()))
			require.True(t, f.state.lifecycle.RepositoryAdmissionClosed(f.tracked.Prefix))
			var loadCalls atomic.Int64
			original := warmupLoadRepo
			t.Cleanup(func() { warmupLoadRepo = original })
			warmupLoadRepo = func(ctx context.Context, a warmupRepoAccounting, state *daemonState, entry config.RepoEntry, prior map[string]int64, deferrable bool, logger *zap.Logger) (string, error) {
				loadCalls.Add(1)
				return original(ctx, a, state, entry, prior, deferrable, logger)
			}
			g.loads.Store(0)

			// When warmup visits that job, before either raw MI loading path.
			watcher, timings := warmupDaemonStateContext(t.Context(), f.state, zap.NewNop(), nil, nil)
			if watcher != nil {
				t.Cleanup(func() { require.NoError(t, watcher.Stop()) })
			}

			// Then it never reads prior mtimes, indexes or records changed work,
			// and FinishRepoLoad gives the client a terminal cleanup reason.
			require.Zero(t, g.loads.Load())
			require.Zero(t, loadCalls.Load())
			require.Zero(t, timings.filesReindexed)
			require.Nil(t, f.state.multiIndexer.GetMetadata(f.tracked.Prefix))
			load, found := f.state.multiIndexer.RepoLoadFor(f.root)
			require.True(t, found)
			require.True(t, load.State.Unavailable())
			require.False(t, load.State.Loading())
			require.Equal(t, indexer.ErrRepositoryCleanupPending.Error(), load.Reason)
			require.NotNil(t, f.store.GetNode(f.nodeID))
			intents, err := f.store.Catalog().ListTrackingIntents(t.Context(), f.tracked.CheckoutID)
			require.NoError(t, err)
			for _, intent := range intents {
				require.False(t, intent.Active)
			}
		})
	}
}

type warmupCleanupPurgeEntry struct {
	prefix string
	source string
	ready  bool
}

// Physical entry records what the real readiness broadcaster had published.
// Legacy cleanup can then be held without holding a SQLite transaction.
type heldWarmupCleanupPurge struct {
	*store_sqlite.Store
	server      *gortexmcp.Server
	entered     chan warmupCleanupPurgeEntry
	release     chan struct{}
	mu          sync.Mutex
	entries     []warmupCleanupPurgeEntry
	failResolve atomic.Bool
}

func (s *heldWarmupCleanupPurge) record(prefix, source string) warmupCleanupPurgeEntry {
	_, ready := s.server.ReadinessPhase()
	entry := warmupCleanupPurgeEntry{prefix: prefix, source: source, ready: ready}
	s.mu.Lock()
	s.entries = append(s.entries, entry)
	s.mu.Unlock()
	return entry
}

func (s *heldWarmupCleanupPurge) PurgeRepo(prefix string) error {
	entry := s.record(prefix, "repository_cleanup")
	s.entered <- entry
	<-s.release
	return s.Store.PurgeRepo(prefix)
}

func (s *heldWarmupCleanupPurge) PurgeRepoContext(ctx context.Context, prefix string) error {
	s.record(prefix, "orphan_maintenance")
	return s.Store.PurgeRepoContext(ctx, prefix)
}

var errCleanupWarmupResolve = errors.New("injected cleanup warmup resolve failure")

func (s *heldWarmupCleanupPurge) BeginUnresolvedEdgeScan(ctx context.Context) (graph.UnresolvedEdgeScan, error) {
	if s.failResolve.Swap(false) {
		return graph.UnresolvedEdgeScan{}, errCleanupWarmupResolve
	}
	return s.Store.BeginUnresolvedEdgeScan(ctx)
}

type warmupCleanupFinalizer struct {
	*indexer.DedicatedBaseRuntime
	finalized chan string
}

func (o *warmupCleanupFinalizer) FinalizeDedicatedBaseOwner(graphID string, owner store_sqlite.DedicatedBaseOwner, drained <-chan struct{}) error {
	if err := o.DedicatedBaseRuntime.FinalizeDedicatedBaseOwner(graphID, owner, drained); err != nil {
		return err
	}
	o.finalized <- graphID
	return nil
}

func TestDaemonSeedAndWarmupReachReadyBeforeRepositoryPurgeCompletes(t *testing.T) {
	for _, mode := range []string{"tracked_repository", "last_repository_removed"} {
		t.Run(mode, func(t *testing.T) {
			// Given an interrupted untrack absent from config and, optionally,
			// another configured repo with a symbol that only warmup can index.
			f := newDaemonCleanupFixture(t)
			cm := f.state.configManager
			removed, err := cm.Global().RemoveRepoAndSaveIfPresent(f.root)
			require.NoError(t, err)
			require.True(t, removed)
			if mode == "tracked_repository" {
				root := startupPublicationRepo(t, t.TempDir(), "healthy")
				require.NoError(t, os.WriteFile(filepath.Join(root, "main.go"), []byte("package healthy\nfunc TrackedAfterReadySentinel() {}\n"), 0o600))
				require.NoError(t, cm.Global().AddRepo(config.RepoEntry{Path: root, Name: "healthy"}))
				require.NoError(t, cm.Global().Save())
			}
			g := &heldWarmupCleanupPurge{Store: f.store, entered: make(chan warmupCleanupPurgeEntry, 2), release: make(chan struct{})}
			f.restart(g)
			g.server = f.state.mcpServer
			publisher, err := indexer.NewDedicatedBaseRuntime(f.store, f.state.lifecycle.ViewLeases())
			require.NoError(t, err)
			finalizer := &warmupCleanupFinalizer{DedicatedBaseRuntime: publisher, finalized: make(chan string, 2)}
			require.NoError(t, f.state.lifecycle.SetDedicatedBaseCleanupRuntime(finalizer))
			controller := &realController{graph: g}
			task := newDaemonWarmup(t.Context())
			readyReturned, continueWarmup := make(chan struct{}), make(chan struct{})
			var releaseOnce, continueOnce sync.Once
			release := func() { releaseOnce.Do(func() { close(g.release) }) }
			continueReady := func() { continueOnce.Do(func() { close(continueWarmup) }) }
			t.Cleanup(func() { task.cancel(); release(); continueReady(); task.wait() })
			type result struct {
				seedErr error
				watcher *indexer.MultiWatcher
				timings *warmupTimings
			}
			completed := make(chan result, 1)
			markReady := sync.OnceFunc(func() {
				controller.MarkReady(time.Millisecond)
				publishReadinessPhase(f.state, "ready", true, nil)
				// This is the daemon's real post-publication release signal,
				// not a test substitute for the repository cleanup runtime.
				f.state.lifecycle.MarkRepositoryCleanupReady()
				close(readyReturned)
				select {
				case <-continueWarmup:
				case <-task.ctx.Done():
				}
			})

			// When actual Seed and warmup run with the physical purge held.
			require.True(t, task.start(func(ctx context.Context) {
				seedErr := f.state.lifecycle.Seed(ctx)
				if seedErr != nil {
					completed <- result{seedErr: seedErr}
					return
				}
				watcher, timings := warmupDaemonStateContext(ctx, f.state, zap.NewNop(), markReady, nil)
				completed <- result{watcher: watcher, timings: timings}
			}))
			select {
			case <-readyReturned:
			case <-time.After(20 * time.Second):
				t.Fatal("Seed and warmup did not reach ready while cleanup was held")
			}
			var physical warmupCleanupPurgeEntry
			select {
			case physical = <-g.entered:
			case <-time.After(10 * time.Second):
				t.Fatal("ready did not start the held repository purge")
			}

			// Then ready is published before physical entry, and the old
			// graph stays unreadable while a real tracked-repo query works.
			require.True(t, controller.ready.Load())
			require.Equal(t, warmupCleanupPurgeEntry{prefix: f.tracked.Prefix, source: "repository_cleanup", ready: true}, physical)
			g.mu.Lock()
			entries := append([]warmupCleanupPurgeEntry(nil), g.entries...)
			g.mu.Unlock()
			require.Equal(t, []warmupCleanupPurgeEntry{physical}, entries, "orphan maintenance bypassed the pending saga")
			require.NotNil(t, f.store.GetNode(f.nodeID), "held physical purge completed before the tracked query")
			_, err = f.state.lifecycle.AcquireRepositoryRead(f.tracked.GraphID)
			require.ErrorIs(t, err, graphview.ErrRepositoryAdmissionClosed)
			if mode == "tracked_repository" {
				read, err := f.state.lifecycle.AcquireRepositoryRead(indexer.GraphIDFor("healthy"))
				require.NoError(t, err)
				symbols := query.NewEngine(g).FindSymbols("TrackedAfterReadySentinel", graph.KindFunction)
				read.Release()
				require.Len(t, symbols, 1)
				require.Equal(t, "TrackedAfterReadySentinel", symbols[0].Name)
				require.Equal(t, "healthy", symbols[0].RepoPrefix)
			}

			// When the held purge and warmup tail are released and joined,
			// the same journal finalizes even when no configured repo remains.
			release()
			continueReady()
			task.wait()
			out := <-completed
			require.NoError(t, out.seedErr)
			require.True(t, out.timings.canFinalize(task.ctx))
			if out.watcher != nil {
				t.Cleanup(func() { require.NoError(t, out.watcher.Stop()) })
				require.NoError(t, out.watcher.Stop())
			}
			select {
			case graphID := <-finalizer.finalized:
				require.Equal(t, f.tracked.GraphID, graphID)
			case <-time.After(10 * time.Second):
				t.Fatal("the held cleanup never finalized after ready")
			}
			require.NoError(t, f.state.lifecycle.Close())
			require.Nil(t, f.store.GetNode(f.nodeID))
			journal, err := f.store.Catalog().ListCleanupEntries(t.Context())
			require.NoError(t, err)
			require.Empty(t, journal)
			_, found, err := f.store.Catalog().GetRepositoryFamily(t.Context(), f.tracked.FamilyID)
			require.NoError(t, err)
			require.False(t, found)
		})
	}
}

func TestDaemonCleanupWaitsWhenWarmupFailsBeforeReady(t *testing.T) {
	// Given a pending untrack and a healthy repo whose resolve step fails.
	f := newDaemonCleanupFixture(t)
	_, err := f.state.configManager.Global().RemoveRepoAndSaveIfPresent(f.root)
	require.NoError(t, err)
	healthy := startupPublicationRepo(t, t.TempDir(), "resolve-failure-healthy")
	require.NoError(t, f.state.configManager.Global().AddRepo(config.RepoEntry{Path: healthy, Name: "healthy"}))
	require.NoError(t, f.state.configManager.Global().Save())
	g := &heldWarmupCleanupPurge{Store: f.store, entered: make(chan warmupCleanupPurgeEntry, 2), release: make(chan struct{})}
	close(g.release)
	f.restart(g)
	g.server = f.state.mcpServer
	require.NoError(t, f.state.lifecycle.Seed(t.Context()))
	g.failResolve.Store(true)
	controller := &realController{graph: g}
	markReady := sync.OnceFunc(func() {
		controller.MarkReady(time.Millisecond)
		publishReadinessPhase(f.state, "ready", true, nil)
		f.state.lifecycle.MarkRepositoryCleanupReady()
	})

	// When warmup returns its failed outcome and lifecycle shutdown joins.
	watcher, timings := warmupDaemonStateContext(t.Context(), f.state, zap.NewNop(), markReady, nil)
	if watcher != nil {
		t.Cleanup(func() { require.NoError(t, watcher.Stop()) })
		require.NoError(t, watcher.Stop())
	}
	require.NoError(t, f.state.lifecycle.Close())

	// Then no false ready or forced purge occurs, and recovery remains durable.
	require.ErrorIs(t, timings.resolveErr, errCleanupWarmupResolve)
	require.False(t, controller.ready.Load())
	g.mu.Lock()
	entries := append([]warmupCleanupPurgeEntry(nil), g.entries...)
	g.mu.Unlock()
	require.Empty(t, entries)
	require.NotNil(t, f.store.GetNode(f.nodeID))
	journal, err := f.store.Catalog().ListCleanupEntries(t.Context())
	require.NoError(t, err)
	require.NotEmpty(t, journal)
}

func TestDaemonCleanupReleaseFollowsResolveFailureCompletion(t *testing.T) {
	for _, completion := range []string{"resolve_failure", "canceled_completion"} {
		t.Run(completion, func(t *testing.T) {
			// Given an interrupted cleanup and a warmup resolve failure that
			// skipped compute-ready but still permits the completion fallback.
			f := newDaemonCleanupFixture(t)
			_, err := f.state.configManager.Global().RemoveRepoAndSaveIfPresent(f.root)
			require.NoError(t, err)
			healthy := startupPublicationRepo(t, t.TempDir(), "resolve-failure-healthy")
			require.NoError(t, f.state.configManager.Global().AddRepo(config.RepoEntry{Path: healthy, Name: "healthy"}))
			require.NoError(t, f.state.configManager.Global().Save())
			g := &heldWarmupCleanupPurge{Store: f.store, entered: make(chan warmupCleanupPurgeEntry, 2), release: make(chan struct{})}
			var releaseOnce sync.Once
			release := func() { releaseOnce.Do(func() { close(g.release) }) }
			t.Cleanup(release)
			f.restart(g)
			g.server = f.state.mcpServer
			publisher, err := indexer.NewDedicatedBaseRuntime(f.store, f.state.lifecycle.ViewLeases())
			require.NoError(t, err)
			finalizer := &warmupCleanupFinalizer{DedicatedBaseRuntime: publisher, finalized: make(chan string, 2)}
			require.NoError(t, f.state.lifecycle.SetDedicatedBaseCleanupRuntime(finalizer))
			require.NoError(t, f.state.lifecycle.Seed(t.Context()))
			g.failResolve.Store(true)
			controller := &realController{graph: g}
			markReady := sync.OnceFunc(func() {
				controller.MarkReady(time.Millisecond)
				publishReadinessPhase(f.state, "ready", true, nil)
				f.state.lifecycle.MarkRepositoryCleanupReady()
			})
			watcher, warmup := warmupDaemonStateContext(t.Context(), f.state, zap.NewNop(), markReady, nil)
			if watcher != nil {
				t.Cleanup(func() { require.NoError(t, watcher.Stop()) })
				require.NoError(t, watcher.Stop())
			}
			require.ErrorIs(t, warmup.resolveErr, errCleanupWarmupResolve)
			require.True(t, warmup.canFinalize(t.Context()))
			require.False(t, controller.ready.Load())
			require.NotNil(t, f.store.GetNode(f.nodeID))
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			if completion == "canceled_completion" {
				cancel()
			}
			viewBuilds := indexer.NewViewBuildGate()

			// When the real final publisher chooses usable-but-unenriched status.
			publishWarmupCompletion(ctx, f.state, controller, zap.NewNop(), viewBuilds, warmup, 0, time.Second)

			if completion == "canceled_completion" {
				// Then cancellation retains the closed barrier and durable work.
				require.False(t, viewBuilds.IsOpen())
				release() // A regression must not leave teardown waiting on the spy.
				require.NoError(t, f.state.lifecycle.Close())
				g.mu.Lock()
				entries := append([]warmupCleanupPurgeEntry(nil), g.entries...)
				g.mu.Unlock()
				require.Empty(t, entries)
				require.NotNil(t, f.store.GetNode(f.nodeID))
				journal, err := f.store.Catalog().ListCleanupEntries(t.Context())
				require.NoError(t, err)
				require.NotEmpty(t, journal)
				return
			}

			// Then the fallback publishes readiness before waking physical cleanup,
			// without claiming successful enrichment or needing another sweep.
			require.True(t, controller.ready.Load())
			require.False(t, controller.enriched.Load())
			require.True(t, viewBuilds.IsOpen())
			phase, ready := f.state.mcpServer.ReadinessPhase()
			require.Equal(t, "warmup_incomplete", phase)
			require.True(t, ready)
			select {
			case entry := <-g.entered:
				require.Equal(t, warmupCleanupPurgeEntry{prefix: f.tracked.Prefix, source: "repository_cleanup", ready: true}, entry)
			case <-time.After(10 * time.Second):
				t.Fatal("resolve-failure completion did not release pending cleanup")
			}
			require.NotNil(t, f.store.GetNode(f.nodeID))
			release()
			select {
			case graphID := <-finalizer.finalized:
				require.Equal(t, f.tracked.GraphID, graphID)
			case <-time.After(10 * time.Second):
				t.Fatal("resolve-failure completion did not finalize cleanup")
			}
			require.NoError(t, f.state.lifecycle.Close())
			require.Nil(t, f.store.GetNode(f.nodeID))
			journal, err := f.store.Catalog().ListCleanupEntries(t.Context())
			require.NoError(t, err)
			require.Empty(t, journal)
		})
	}
}
