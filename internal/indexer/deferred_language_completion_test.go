package indexer

import (
	"context"
	"fmt"
	"github.com/stretchr/testify/require"
	"github.com/zzet/gortex/internal/config"
	"github.com/zzet/gortex/internal/contracts"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/graph/store_sqlite"
	"github.com/zzet/gortex/internal/semantic"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	"go.uber.org/zap/zaptest/observer"
	"path"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

// seedSlashIndexedFile seeds a pending file under its slash-separated graph
// path, which these tests compare with literal "r/..." paths on every OS.
func seedSlashIndexedFile(t *testing.T, g graph.Store, repoPrefix, relPath string) string {
	t.Helper()
	graphPath := path.Join(repoPrefix, relPath)
	g.AddBatch([]*graph.Node{{
		ID:         graphPath,
		Kind:       graph.KindFile,
		Name:       path.Base(relPath),
		FilePath:   graphPath,
		Language:   "go",
		RepoPrefix: repoPrefix,
		Meta:       map[string]any{graph.MetaReparsePendingEnrichment: true},
	}}, nil)
	return graphPath
}

type deferredLanguageProvider struct {
	language  string
	fail      bool
	partial   bool
	calls     int
	hook      func()
	nilResult bool
	prepared  int
}

func (p *deferredLanguageProvider) Name() string        { return "language-" + p.language }
func (p *deferredLanguageProvider) Languages() []string { return []string{p.language} }
func (p *deferredLanguageProvider) Available() bool     { return true }
func (p *deferredLanguageProvider) Close() error        { return nil }
func (p *deferredLanguageProvider) Enrich(g graph.Store, root string) (*semantic.EnrichResult, error) {
	return p.EnrichFile(g, root, "")
}
func (p *deferredLanguageProvider) EnrichFile(_ graph.Store, _, _ string) (*semantic.EnrichResult, error) {
	p.calls++
	if p.hook != nil {
		p.hook()
	}
	if p.nilResult {
		return nil, nil
	}
	if p.fail {
		return nil, fmt.Errorf("%s failed", p.language)
	}
	return &semantic.EnrichResult{Provider: p.Name(), Language: p.language, Partial: p.partial, AbortReason: "fixture abort", BoundReason: "fixture bound"}, nil
}
func (p *deferredLanguageProvider) PrepareFileExportsContext(context.Context, string, string, []string) error {
	p.prepared++
	return nil
}

func TestDeferredFileLanguageCompletion(t *testing.T) {
	t.Run("receipt refused after body", func(t *testing.T) {
		g := &dischargeBoundaryStore{Store: openTestSqlite(t)}
		idx, mi, _, _ := deferredLanguageFixture(t, g)
		authority := NewOutputGenerationAuthority(nil)
		idx.SetOutputGenerationAuthority(authority)
		run := mi.BeginStartupDeferredPasses(t.Context(), nil)
		run.Wait()
		var newer *OutputMutationReceipt
		g.hook = func() {
			var err error
			newer, err = authority.Begin(t.Context(), OutputEntryIndexFile, idx.legacyOutputTarget())
			require.NoError(t, err)
		}
		result := run.FinishTailResult()
		require.NotNil(t, newer, "completion body must run before refusal")
		newer.Abandon()
		require.ErrorIs(t, result.EnrichErr, ErrOutputMutationReceiptSuperseded)
		require.False(t, result.CrossRepoComplete)
	})
	t.Run("canceled between languages", func(t *testing.T) {
		idx, mi, a, b := deferredLanguageFixture(t, openTestSqlite(t))
		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()
		core, _ := observer.New(zap.InfoLevel)
		idx.logger = zap.New(core, zap.Hooks(func(entry zapcore.Entry) error {
			if entry.Message == "semantic enrichment result" {
				cancel()
			}
			return nil
		}))
		result := mi.BeginStartupDeferredPasses(ctx, nil).FinishTailResult()
		require.ErrorIs(t, result.EnrichErr, context.Canceled)
		require.Equal(t, 1, a.calls)
		require.Zero(t, b.calls)
		require.Equal(t, []string{"r/a.ts"}, idx.pendingEnrichFrontier())
	})
	for _, mode := range []string{"disabled", "no providers", "forced"} {
		t.Run("admission definition "+mode, func(t *testing.T) {
			idx, mi, _, _ := deferredLanguageFixture(t, openTestSqlite(t))
			cfg := semantic.Config{Enabled: mode != "disabled"}
			mgr := semantic.NewManager(cfg, zap.NewNop())
			defer mgr.Close()
			if mode != "no providers" {
				mgr.RegisterProvider(&deferredLanguageProvider{language: "go"})
			}
			idx.SetSemanticManager(mgr)
			if mode == "forced" {
				t.Setenv("GORTEX_WARMUP_FORCE_ENRICH", "1")
				_, _, generation := idx.deferredEnrichScope()
				require.True(t, idx.clearPendingEnrich(generation))
			}
			idx.repositoryMutations().closeAdmission()
			result := mi.BeginStartupDeferredPasses(t.Context(), nil).FinishTailResult()
			if mode == "forced" {
				require.Error(t, result.EnrichErr)
			} else {
				require.NoError(t, result.EnrichErr)
			}
		})
	}
	t.Run("full completion then new request", func(t *testing.T) {
		t.Setenv("GORTEX_ENRICH_MIN_NODES", "0")
		g := &fullCompletionStore{Store: openTestSqlite(t)}
		idx, mi, _, _ := deferredLanguageFixture(t, g)
		idx.SetRootPath(commitGitRepo(t))
		idx.markPendingEnrichFull()
		g.hook = idx.markPendingEnrichFull
		result := mi.BeginStartupDeferredPasses(t.Context(), nil).FinishTailResult()
		require.True(t, g.recorded)
		require.NoError(t, result.EnrichErr, "a later request does not invalidate accepted completion")
		require.True(t, idx.pendingEnrich.Load())
	})
	t.Run("full discharge holds ownership", func(t *testing.T) {
		t.Setenv("GORTEX_ENRICH_MIN_NODES", "0")
		g := &dischargeBoundaryStore{Store: openTestSqlite(t)}
		idx, mi, _, _ := deferredLanguageFixture(t, g)
		idx.markPendingEnrichFull()
		g.hook = func() {
			require.Empty(t, idx.repositoryMutations().lane, "full discharge must hold mutation ownership")
		}
		result := mi.BeginStartupDeferredPasses(t.Context(), nil).FinishTailResult()
		require.NoError(t, result.EnrichErr)
	})
	t.Run("lost mutation admission", func(t *testing.T) {
		idx, mi, _, _ := deferredLanguageFixture(t, openTestSqlite(t))
		idx.repositoryMutations().closeAdmission()
		result := mi.BeginStartupDeferredPasses(t.Context(), nil).FinishTailResult()
		require.Error(t, result.EnrichErr)
		require.True(t, idx.pendingEnrich.Load())
		require.Len(t, idx.pendingEnrichFrontier(), 2)
	})
	t.Run("startup full and current marker", func(t *testing.T) {
		t.Setenv("GORTEX_ENRICH_MIN_NODES", "0")
		idx, mi, a, b := deferredLanguageFixture(t, openTestSqlite(t))
		idx.markPendingEnrichFull()
		result := mi.BeginStartupDeferredPasses(t.Context(), nil).FinishTailResult()
		require.NoError(t, result.EnrichErr)
		require.Zero(t, a.prepared)
		require.Zero(t, b.prepared)
		require.Empty(t, idx.pendingEnrichFrontier())
		a.calls = 0
		b.calls = 0
		mi.BeginStartupDeferredPasses(t.Context(), nil).FinishTailResult()
		require.Zero(t, a.calls)
		require.Zero(t, b.calls)
	})
	t.Run("all callers and no-work", deferredLanguageCallers)
	t.Run("durable restart", deferredLanguageRestart)
	t.Run("new generation and owned tail", deferredLanguageGeneration)
	t.Run("mutation boundary discharge", deferredLanguageMutationBoundary)

	for _, mode := range []string{"error", "partial", "restart"} {
		t.Run(mode, func(t *testing.T) {
			store := openTestSqlite(t)
			goProvider := &deferredLanguageProvider{language: "go", fail: mode != "partial", partial: mode == "partial"}
			tsProvider := &deferredLanguageProvider{language: "typescript"}
			mgr := semantic.NewManager(semantic.Config{Enabled: true}, zap.NewNop())
			mgr.RegisterProvider(goProvider)
			mgr.RegisterProvider(tsProvider)
			t.Cleanup(func() { require.NoError(t, mgr.Close()) })
			idx := New(store, newTestRegistry(), config.Default().Index, zap.NewNop())
			idx.SetRepoPrefix("r")
			idx.SetRootPath(t.TempDir())
			idx.SetSemanticManager(mgr)
			goPath := seedSlashIndexedFile(t, store, "r", "a.go")
			tsPath := seedSlashIndexedFile(t, store, "r", "a.ts")
			node := store.GetFileNodes(tsPath)[0]
			node.Language = "typescript"
			store.AddBatch([]*graph.Node{node}, nil)
			idx.markPendingEnrichFiles([]string{goPath, tsPath})
			mi := newEmptyMultiIndexer(t, store)
			mi.indexers["r"] = idx
			result := mi.RunDeferredPassesAllResult(context.Background())
			if mode == "partial" {
				require.ErrorContains(t, result.EnrichErr, "fixture abort")
				require.ErrorContains(t, result.EnrichErr, "fixture bound")
			}
			require.Equal(t, 1, goProvider.calls)
			if mode == "restart" {
				resumed := New(store, newTestRegistry(), config.Default().Index, zap.NewNop())
				resumed.SetRepoPrefix("r")
				resumed.SetRootPath(idx.rootPath)
				resumed.SetSemanticManager(mgr)
				require.True(t, resumed.MaybeSeedPendingEnrich())
				files, full, _ := resumed.deferredEnrichScope()
				require.False(t, full)
				require.Equal(t, []string{goPath}, files, "restart must re-arm only failed language")
			} else {
				require.Equal(t, 1, tsProvider.calls, "later independent language must run")
				require.Equal(t, []string{goPath}, pendingLedger(t, idx), "successful sibling must discharge")
			}
		})
	}
}

func deferredLanguageFixture(t *testing.T, g graph.Store) (*Indexer, *MultiIndexer, *deferredLanguageProvider, *deferredLanguageProvider) {
	t.Helper()
	a := &deferredLanguageProvider{language: "go"}
	b := &deferredLanguageProvider{language: "typescript"}
	mgr := semantic.NewManager(semantic.Config{Enabled: true, EnrichOnWatch: true}, zap.NewNop())
	mgr.RegisterProvider(a)
	mgr.RegisterProvider(b)
	t.Cleanup(func() { require.NoError(t, mgr.Close()) })
	idx := New(g, newTestRegistry(), config.Default().Index, zap.NewNop())
	idx.SetRepoPrefix("r")
	idx.SetRootPath(t.TempDir())
	idx.SetSemanticManager(mgr)
	ga := seedSlashIndexedFile(t, g, "r", "a.go")
	gb := seedSlashIndexedFile(t, g, "r", "a.ts")
	node := g.GetFileNodes(gb)[0]
	node.Language = "typescript"
	g.AddBatch([]*graph.Node{node}, nil)
	idx.markPendingEnrichFiles([]string{ga, gb})
	mi := newEmptyMultiIndexer(t, g)
	mi.indexers["r"] = idx
	t.Cleanup(func() { require.NoError(t, mi.Close(context.Background())) })
	return idx, mi, a, b
}
func deferredLanguageCallers(t *testing.T) {
	for _, route := range []string{"pool", "direct", "watcher"} {
		for _, failed := range []string{"go", "typescript", "nil"} {
			t.Run(route+" "+failed, func(t *testing.T) {
				g := openTestSqlite(t)
				idx, mi, a, b := deferredLanguageFixture(t, g)
				a.fail = failed == "go"
				b.fail = failed == "typescript"
				a.nilResult = failed == "nil"
				core, logs := observer.New(zap.WarnLevel)
				idx.logger = zap.New(core)
				if failed == "go" {
					a.fail = false
					a.partial = true
				}
				noWork := []string{"r/gone.go", "r/go.mod", "r/go.work", seedSlashIndexedFile(t, g, "r", "none.rb")}
				node := g.GetFileNodes("r/none.rb")[0]
				node.Language = "ruby"
				g.AddBatch([]*graph.Node{node}, nil)
				idx.markPendingEnrichFiles(append(noWork, "r/a.ts", "r/a.ts"))
				switch route {
				case "pool":
					mi.RunDeferredPassesAllResult(t.Context())
				case "direct":
					idx.pendingContractReg = contracts.NewRegistry()
					idx.RunDeferredPasses(t.Context())
				case "watcher":
					require.NoError(t, idx.coordinateRepositoryMutation(t.Context(), OutputEntryDeferredPasses, func() error { idx.runIncrementalWatcherSemantic([]string{"r/a.go", "r/a.ts"}); return nil }))
				}
				require.Equal(t, 1, a.calls)
				require.Zero(t, a.prepared)
				require.Zero(t, b.prepared)
				require.Equal(t, 1, b.calls)
				want := []string{}
				if failed == "go" {
					want = []string{"r/a.go"}
					entries := logs.FilterMessage("file-scoped deferred semantic enrichment partial").All()
					require.Len(t, entries, 1)
					require.Equal(t, "go", entries[0].ContextMap()["language"])
					require.EqualValues(t, 1, entries[0].ContextMap()["files"])
					require.Equal(t, "fixture abort", entries[0].ContextMap()["abort_reason"])
					require.Equal(t, "fixture bound", entries[0].ContextMap()["bound_reason"])
				}
				if failed == "typescript" {
					want = []string{"r/a.ts"}
				}
				require.Equal(t, want, idx.pendingEnrichFrontier())
				require.Equal(t, len(want) > 0, idx.pendingEnrich.Load())
			})
		}
	}
	// Existing recovery precedence is part of this exact narrow check too.
	t.Run("dirty seed", TestMaybeSeedPendingEnrich_DirtyTreeResumesFromLedger)
	t.Run("stale marker dominates", TestMaybeSeedPendingEnrich_StaleMarkerStillWinsOverLedger)
	t.Run("non-git seed", TestMaybeSeedPendingEnrich_NonGitRepoResumesFromLedger)
}
func deferredLanguageRestart(t *testing.T) {
	path := filepath.Join(t.TempDir(), "ledger.sqlite")
	g, err := store_sqlite.Open(path)
	require.NoError(t, err)
	t.Cleanup(func() { _ = g.Close() })
	idx, mi, a, _ := deferredLanguageFixture(t, g)
	repo := commitGitRepo(t)
	idx.SetRootPath(repo)
	idx.semanticMgr.RecordRepoEnrichmentComplete(g, "r", repoHead(repo), false)
	a.fail = true
	result := mi.RunDeferredPassesAllResult(t.Context())
	require.Error(t, result.EnrichErr)
	require.NoError(t, mi.Close(t.Context()))
	require.NoError(t, g.Close())
	g, err = store_sqlite.Open(path)
	require.NoError(t, err)
	resumed := New(g, newTestRegistry(), config.Default().Index, zap.NewNop())
	resumed.SetRepoPrefix("r")
	resumed.SetRootPath(repo)
	resumed.SetSemanticManager(idx.semanticMgr)
	require.True(t, resumed.MaybeSeedPendingEnrich())
	files, full, _ := resumed.deferredEnrichScope()
	require.False(t, full)
	require.Equal(t, []string{"r/a.go"}, files)
	a.fail = false
	resumed.runDeferredEnrich()
	require.Empty(t, resumed.pendingEnrichFrontier())
	require.NoError(t, g.Close())
	g, err = store_sqlite.Open(path)
	require.NoError(t, err)
	resumed.graph = g
	require.False(t, resumed.MaybeSeedPendingEnrich())
}
func deferredLanguageGeneration(t *testing.T) {
	for _, event := range []string{"same", "new", "full", "attempt"} {
		t.Run(event, func(t *testing.T) {
			idx, mi, _, b := deferredLanguageFixture(t, openTestSqlite(t))
			entered, release := make(chan struct{}), make(chan struct{})
			b.hook = func() {
				close(entered)
				select {
				case <-release:
				case <-time.After(10 * time.Second):
					panic("language release timed out")
				}
			}
			run := mi.BeginDeferredPasses(t.Context(), nil)
			select {
			case <-entered:
			case <-time.After(10 * time.Second):
				t.Fatal("later language did not dispatch")
			}
			require.NoError(t, idx.coordinateRepositoryMutation(t.Context(), OutputEntryDeferredPasses, func() error {
				switch event {
				case "full":
					idx.markPendingEnrichFull()
				case "new":
					seedSlashIndexedFile(t, idx.graph, "r", "new.go")
					idx.markPendingEnrichFiles([]string{"r/new.go"})
				case "attempt":
					idx.deferredAttempt = &deferredPassAttempt{indexer: idx}
				default:
					idx.markPendingEnrichFiles([]string{"r/a.go"})
				}
				return nil
			}))
			close(release)
			result := run.FinishTailResult()
			require.Error(t, result.EnrichErr)
			require.True(t, idx.pendingEnrich.Load())
			want := []string{"r/a.go", "r/a.ts"}
			if event == "new" {
				want = append(want, "r/new.go")
			}
			require.Equal(t, want, idx.pendingEnrichFrontier())
			if event == "full" {
				_, full, _ := idx.deferredEnrichScope()
				require.True(t, full)
			}
		})
	}
}

type dischargeBoundaryStore struct {
	graph.Store
	hook func()
	once sync.Once
}

func (s *dischargeBoundaryStore) GetFileNodesByPaths(paths []string) map[string][]*graph.Node {
	if s.hook != nil {
		s.once.Do(s.hook)
	}
	return s.Store.GetFileNodesByPaths(paths)
}
func deferredLanguageMutationBoundary(t *testing.T) {
	for _, full := range []bool{false, true} {
		t.Run(fmt.Sprint(full), func(t *testing.T) { deferredLanguageOwnedDischarge(t, full) })
	}
}
func deferredLanguageOwnedDischarge(t *testing.T, full bool) {
	g := &dischargeBoundaryStore{Store: openTestSqlite(t)}
	idx, mi, _, _ := deferredLanguageFixture(t, g)
	if full {
		t.Setenv("GORTEX_ENRICH_MIN_NODES", "0")
		idx.markPendingEnrichFull()
	}
	run := mi.BeginDeferredPasses(t.Context(), nil)
	run.Wait()
	queued := make(chan struct{})
	done := make(chan error, 1)
	coordinator := idx.repositoryMutations()
	// The hook runs after the generation check and just before durable discharge.
	// Queue a real mutation through the same coordinator while the tail owns it.
	g.hook = func() {
		require.Empty(t, coordinator.lane, "tail must hold actual mutation ownership across durable discharge")
		coordinator.batchAdmissionHook = func() { close(queued) }
		go func() {
			done <- idx.coordinateRepositoryMutation(t.Context(), OutputEntryDeferredPasses, func() error {
				seedSlashIndexedFile(t, g.Store, "r", "a.go")
				idx.markPendingEnrichFiles([]string{"r/a.go"})
				return nil
			})
		}()
		select {
		case <-queued:
		case <-time.After(10 * time.Second):
			t.Fatal("new mutation did not reach admission")
		}
	}
	result := run.FinishTailResult()
	require.NoError(t, result.EnrichErr)
	select {
	case err := <-done:
		require.NoError(t, err)
	case <-time.After(10 * time.Second):
		t.Fatal("queued mutation did not finish")
	}
	coordinator.batchAdmissionHook = nil
	g.hook = nil
	require.Equal(t, []string{"r/a.go"}, idx.pendingEnrichFrontier())
	files, full, _ := idx.deferredEnrichScope()
	require.False(t, full)
	require.Equal(t, []string{"r/a.go"}, files)
}

// A completion-marker callback models a new reindex request after this pass was accepted.
type fullCompletionStore struct {
	graph.Store
	hook     func()
	recorded bool
}

func (s *fullCompletionStore) GetEnrichmentState(repo, provider string) (graph.EnrichmentState, bool, error) {
	return s.Store.(graph.EnrichmentStateStore).GetEnrichmentState(repo, provider)
}
func (s *fullCompletionStore) SetEnrichmentState(state graph.EnrichmentState) error {
	err := s.Store.(graph.EnrichmentStateStore).SetEnrichmentState(state)
	if state.Provider == "__repo__" && s.hook != nil {
		s.recorded = true
		s.hook()
	}
	return err
}

func (s *fullCompletionStore) RepoLanguageFileCounts(prefixes []string) []graph.RepoLanguageFileCount {
	return s.Store.(graph.RepoLanguageFileCountReader).RepoLanguageFileCounts(prefixes)
}

func (s *dischargeBoundaryStore) RepoLanguageFileCounts(prefixes []string) []graph.RepoLanguageFileCount {
	return s.Store.(graph.RepoLanguageFileCountReader).RepoLanguageFileCounts(prefixes)
}
