package main

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/mark3labs/mcp-go/mcp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
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
	failScan   atomic.Bool
	resolveErr error
}

func (s *warmupResolveFailureStore) BeginUnresolvedEdgeScan(ctx context.Context) (graph.UnresolvedEdgeScan, error) {
	if s.failScan.Swap(false) {
		return graph.UnresolvedEdgeScan{}, s.resolveErr
	}
	return s.Store.BeginUnresolvedEdgeScan(ctx)
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
			publishWarmupCompletion(state, controller, logger, viewBuilds, warmup, time.Millisecond, time.Second)

			assert.True(t, controller.ready.Load(), "completion must leave the daemon queryable, even if compute-ready was skipped")
			assert.True(t, viewBuilds.IsOpen(), "warmup must release waiting view builds on either outcome")
			require.Len(t, logs.FilterMessage("daemon: enrichment apply gate opened").All(), 1)
			phase, _ := srv.ReadinessPhase()
			if failResolve {
				require.Len(t, logs.FilterMessage("daemon: warmup resolve failed").All(), 1, "injection must reach the real resolver")
				assert.False(t, controller.enriched.Load())
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
