package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"github.com/stretchr/testify/require"
	"github.com/zzet/gortex/internal/config"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/graph/store_sqlite"
	"github.com/zzet/gortex/internal/indexer"
	gortexmcp "github.com/zzet/gortex/internal/mcp"
	"github.com/zzet/gortex/internal/parser"
	"github.com/zzet/gortex/internal/parser/languages"
	"github.com/zzet/gortex/internal/query"
	"github.com/zzet/gortex/internal/semantic"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	"go.uber.org/zap/zaptest/observer"
	"os"
	"path/filepath"
	"testing"
	"testing/synctest"
	"time"
)

type startupExportProvider struct {
	prepared      int
	bound         int
	fail          bool
	partial       bool
	cancel        context.CancelFunc
	contextErr    error
	bindingBudget time.Duration
	language      string
	supersede     bool
	sibling       *startupExportProvider
	prepHook      func(context.Context) error
}

func (p *startupExportProvider) Name() string { return "startup-export" + p.language }
func (p *startupExportProvider) Languages() []string {
	if p.language != "" {
		return []string{p.language}
	}
	return []string{"go"}
}
func (p *startupExportProvider) Available() bool { return true }
func (p *startupExportProvider) Close() error    { return nil }
func (p *startupExportProvider) Enrich(graph.Store, string) (*semantic.EnrichResult, error) {
	p.bound++
	if p.fail {
		return nil, errors.New("injected full enrichment failure")
	}
	return &semantic.EnrichResult{Provider: p.Name(), Language: p.Languages()[0], Partial: p.partial}, nil
}
func (p *startupExportProvider) EnrichFile(graph.Store, string, string) (*semantic.EnrichResult, error) {
	return nil, nil
}
func (p *startupExportProvider) PrepareFileExportsContext(ctx context.Context, _, _ string, _ []string) error {
	p.prepared++
	if p.prepHook != nil {
		return p.prepHook(ctx)
	}
	return ctx.Err()
}
func (p *startupExportProvider) EnrichFilesContext(ctx context.Context, _ graph.Store, _, _ string, _ []string) (*semantic.EnrichResult, error) {
	p.bound++
	if deadline, ok := ctx.Deadline(); ok {
		p.bindingBudget = time.Until(deadline)
	}
	if p.cancel != nil {
		p.cancel()
		p.contextErr = ctx.Err()
		return nil, p.contextErr
	}
	if p.fail {
		return nil, errors.New("injected Go file failure")
	}
	return &semantic.EnrichResult{Provider: p.Name(), Language: p.Languages()[0], Partial: p.partial}, nil
}
func startupExportWarmup(t *testing.T, p *startupExportProvider, ctx context.Context, overlap bool) (*daemonState, *realController, *warmupTimings, *observer.ObservedLogs) {
	t.Helper()
	if overlap {
		t.Setenv("GORTEX_ENRICH_OVERLAP", "1")
	} else {
		t.Setenv("GORTEX_ENRICH_OVERLAP", "0")
	}
	dir := t.TempDir()
	repo := filepath.Join(dir, "repo")
	require.NoError(t, os.Mkdir(repo, 0755))
	require.NoError(t, os.WriteFile(filepath.Join(repo, "main.go"), []byte("package main\nfunc main() {}\n"), 0644))
	require.NoError(t, os.WriteFile(filepath.Join(repo, "sibling.ts"), []byte("export const sibling = 1;\n"), 0644))
	g, err := store_sqlite.Open(filepath.Join(dir, "store.sqlite"))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, g.Close()) })
	cm, err := config.NewConfigManager(filepath.Join(dir, "config.yaml"))
	require.NoError(t, err)
	entry := config.RepoEntry{Path: repo, Name: "repo"}
	require.NoError(t, cm.Global().AddRepo(entry))
	reg := parser.NewRegistry()
	reg.Register(languages.NewGoExtractor())
	reg.Register(languages.NewTypeScriptExtractor())
	idx := indexer.New(g, reg, config.Default().Index, zap.NewNop())
	cold := indexer.NewMultiIndexer(g, reg, idx.Search(), cm, zap.NewNop())
	_, err = cold.TrackRepoCtx(t.Context(), entry)
	require.NoError(t, err)
	require.NoError(t, cold.Close(t.Context()))
	for _, path := range []string{"repo/main.go", "repo/sibling.ts"} {
		nodes := g.GetFileNodes(path)
		require.NotEmpty(t, nodes)
		for _, node := range nodes {
			if node.Kind == graph.KindFile {
				if node.Meta == nil {
					node.Meta = map[string]any{}
				}
				node.Meta[graph.MetaReparsePendingEnrichment] = true
				g.AddBatch([]*graph.Node{node}, nil)
			}
		}
	}
	core, logs := observer.New(zap.InfoLevel)
	var mi *indexer.MultiIndexer
	logger := zap.New(core, zap.Hooks(func(entry zapcore.Entry) error {
		if p.supersede && entry.Message == "semantic enrichment result" {
			entries := logs.FilterMessage(entry.Message).All()
			if entries[len(entries)-1].ContextMap()["language"] == "typescript" {
				p.supersede = false
				require.NoError(t, os.WriteFile(filepath.Join(repo, "main.go"), []byte("package main\nfunc main() { var changed=1; _=changed }\n"), 0644))
				_, err := mi.IncrementalReindexRepo("repo", []string{"main.go"})
				require.NoError(t, err)
			}
		}
		return nil
	}))
	mi = indexer.NewMultiIndexer(g, reg, idx.Search(), cm, logger)
	t.Cleanup(func() { require.NoError(t, mi.Close(context.Background())) })
	mgr := semantic.NewManager(semantic.Config{Enabled: true, TimeoutSeconds: 120}, logger)
	mgr.RegisterProvider(p)
	p.sibling = &startupExportProvider{language: "typescript"}
	mgr.RegisterProvider(p.sibling)
	mi.SetSemanticManager(mgr)
	t.Cleanup(func() { require.NoError(t, mgr.Close()) })
	srv := gortexmcp.NewServer(query.NewEngine(g), g, idx, nil, logger, nil)
	state := &daemonState{graph: g, indexer: idx, multiIndexer: mi, configManager: cm, mcpServer: srv}
	controller := &realController{graph: g}
	_, warmup := warmupDaemonStateContext(ctx, state, logger, func() { controller.MarkReady(time.Millisecond) }, func(context.Context, *indexer.MultiWatcher) error { return nil })
	return state, controller, warmup, logs
}
func TestStartupFileExportPreparationLifecycle(t *testing.T) {
	t.Run("fresh partial budget after long preparation", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			p := &startupExportProvider{prepHook: func(ctx context.Context) error {
				deadline, ok := ctx.Deadline()
				require.True(t, ok)
				require.Equal(t, 480*time.Second, time.Until(deadline))
				<-time.After(121 * time.Second)
				return ctx.Err()
			}}
			startupExportWarmup(t, p, t.Context(), false)
			require.Equal(t, 1, p.prepared)
			require.Equal(t, 1, p.bound)
			require.Equal(t, 120*time.Second, p.bindingBudget)
		})
	})

	for _, overlap := range []bool{false, true} {
		t.Run(fmt.Sprint(overlap), func(t *testing.T) {
			p := &startupExportProvider{}
			_, _, _, _ = startupExportWarmup(t, p, t.Context(), overlap)
			require.Equal(t, 1, p.prepared, "startup must prepare exports before binding")
			require.Equal(t, 1, p.bound)
		})
	}
	t.Run("parent cancellation", func(t *testing.T) {
		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()
		p := &startupExportProvider{cancel: cancel}
		startupExportWarmup(t, p, ctx, false)
		require.ErrorIs(t, p.contextErr, context.Canceled, "startup parent cancellation must reach admitted binding")
	})
}
func TestWarmupRequiredEnrichmentIncomplete(t *testing.T) {
	for _, mode := range []string{"failure", "partial", "superseded", "full failure", "full partial"} {
		t.Run(mode, func(t *testing.T) {
			full := mode == "full failure" || mode == "full partial"
			if full {
				t.Setenv("GORTEX_WARMUP_FORCE_ENRICH", "1")
				t.Setenv("GORTEX_ENRICH_MIN_NODES", "0")
			}
			p := &startupExportProvider{fail: mode == "failure" || mode == "full failure", partial: mode == "partial" || mode == "full partial", supersede: mode == "superseded"}
			state, controller, warmup, logs := startupExportWarmup(t, p, t.Context(), false)
			require.Equal(t, 1, p.bound, "fixture must exercise required file enrichment")
			require.NoError(t, warmup.resolveErr)
			pubCore, pubLogs := observer.New(zap.InfoLevel)
			publishWarmupCompletion(t.Context(), state, controller, zap.New(pubCore), indexer.NewViewBuildGate(), warmup, time.Millisecond, time.Second)
			require.True(t, controller.ready.Load())
			require.False(t, controller.enriched.Load(), "failed required work cannot publish enriched=true")
			phase, _ := state.mcpServer.ReadinessPhase()
			require.Equal(t, "warmup_incomplete", phase)
			require.Empty(t, pubLogs.FilterMessage("daemon: enrichment complete").All())
			require.Equal(t, 1, p.sibling.bound)
			goFile := state.graph.GetFileNodes("repo/main.go")[0]
			require.Equal(t, true, goFile.Meta[graph.MetaReparsePendingEnrichment])
			if mode != "superseded" && !full {
				tsFile := state.graph.GetFileNodes("repo/sibling.ts")[0]
				require.NotContains(t, tsFile.Meta, graph.MetaReparsePendingEnrichment)
			}
			require.NotEmpty(t, logs.FilterMessage("semantic enrichment result").All())
			if full {
				require.Zero(t, p.prepared)
			}
		})
	}
}

func TestWarmupPendingStatusWording(t *testing.T) {
	st := sampleStatus()
	st.Ready, st.EnrichmentComplete, st.Enrichment = true, false, nil
	var buf bytes.Buffer
	renderDaemonHeader(&buf, st)
	require.Contains(t, buf.String(), "warmup incomplete")
	require.Contains(t, buf.String(), "enrichment pending")
	require.NotContains(t, buf.String(), "in progress")
}
