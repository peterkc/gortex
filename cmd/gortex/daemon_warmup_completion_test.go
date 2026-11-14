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
	}{
		{name: "success"},
		{name: "resolve_failure_after_ready", failAfterReady: true},
		{name: "resolve_failure_before_ready", failBeforeReady: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			failResolve := tc.failBeforeReady || tc.failAfterReady
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
			logger := zap.New(core)
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

			assert.True(t, controller.ready.Load(), "completion must leave the daemon queryable, even if compute-ready was skipped")
			assert.True(t, viewBuilds.IsOpen(), "warmup must release waiting view builds on either outcome")
			require.Len(t, logs.FilterMessage("daemon: enrichment apply gate opened").All(), 1)
			phase, _ := srv.ReadinessPhase()
			if failResolve {
				require.Len(t, logs.FilterMessage("daemon: warmup resolve failed").All(), 1, "injection must reach the real resolver")
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
				assert.Equal(t, "pre-enrichment resolve failed; check the daemon log", notification.Params.AdditionalFields["error"])
				entries := logs.FilterMessage("daemon: warmup incomplete").All()
				require.Len(t, entries, 1)
				assert.Equal(t, resolveErr.Error(), entries[0].ContextMap()["error"])
			} else {
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
	for _, phase := range []string{"same_repo", "cross_repo"} {
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
			})

			// When the exact warmup resolve encounters the bounded refusal.
			mw, warmup := warmupDaemonStateContext(t.Context(), state, logger, markReady, func(context.Context, *indexer.MultiWatcher) error { return nil })
			if mw != nil {
				t.Cleanup(func() { require.NoError(t, mw.Stop()) })
			}

			// Then the full-pass fallback publishes ready and completes startup relations.
			require.NoError(t, warmup.resolveErr)
			require.Positive(t, g.incomingRefusals.Load(), "the bounded refusal must be exercised")
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
