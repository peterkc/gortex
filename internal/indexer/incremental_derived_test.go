package indexer

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
	"go.uber.org/zap/zaptest/observer"

	"github.com/zzet/gortex/internal/config"
	"github.com/zzet/gortex/internal/contracts"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/resolver"
	"github.com/zzet/gortex/internal/search"
)

func TestRunIncrementalDerivedPassesBodyOnlySkipsGlobalFamilies(t *testing.T) {
	mi := &MultiIndexer{
		graph:    graph.New(),
		indexers: map[string]*Indexer{},
		logger:   zap.NewNop(),
	}
	report := mi.RunIncrementalDerivedPasses(context.Background(), map[string]DerivedInvalidationPlan{
		"repo": {
			Files:         []string{"repo/a.go"},
			BodyOnlyFiles: 1,
		},
	})
	if report.Repos != 1 || report.Files != 1 || report.LegacyFallback {
		t.Fatalf("frontier report = %#v", report)
	}
	if report.Implements != 0 || report.Overrides != 0 || report.TestEdges != 0 ||
		report.Capability != 0 || report.Framework != 0 || report.ExternalCalls != 0 ||
		report.CrossRepo != 0 || report.Contracts != 0 {
		t.Fatalf("body-only edit ran a derived family: %#v", report)
	}
}

func TestContractSetsEqualIncludesAccuracyBearingLocations(t *testing.T) {
	base := contracts.Contract{ID: "http::GET::/x", Role: contracts.RoleProvider, FilePath: "a.go", SymbolID: "a.go::Serve", Line: 4}
	if !contractSetsEqual([]contracts.Contract{base}, []contracts.Contract{base}) {
		t.Fatal("identical contract sets compare different")
	}
	shifted := base
	shifted.Line++
	if contractSetsEqual([]contracts.Contract{base}, []contracts.Contract{shifted}) {
		t.Fatal("line shift was ignored by contract comparison")
	}
}

func TestContractSourceDependencyClassification(t *testing.T) {
	if contractSourceNeedsFullRefresh("repo/a.go", "go", []byte("func f() {}")) {
		t.Fatal("ordinary Go source requested a contract dependency frontier")
	}
	if !contractSourceNeedsFullRefresh("repo/router.py", "python", []byte("app.include_router(users)")) {
		t.Fatal("cross-file router mount did not request its contract dependency frontier")
	}
	if contractSourceNeedsFullRefresh("repo/go.mod", "gomod", []byte("module example.com/repo")) {
		t.Fatal("go.mod must stay on its exact dependency-contract file refresh")
	}
}

type frameworkReplacementFailureStore struct {
	graph.Store
	failure error
}

func (s *frameworkReplacementFailureStore) ReplaceDerivedContracts(graph.DerivedContractReplacement) (graph.DerivedContractReplaceResult, error) {
	return graph.DerivedContractReplaceResult{}, s.failure
}

func TestIncrementalFrameworkReplacementRefusalIsVisible(t *testing.T) {
	for _, failure := range []error{errors.ErrUnsupported, errors.New("framework replacement failed"), context.Canceled} {
		t.Run(failure.Error(), func(t *testing.T) {
			// Given changed EF mapping facts with no replacement capability, when the
			// derived coordinator runs, then the old mapping remains and refusal is named.
			base := graph.New()
			base.AddBatch([]*graph.Node{
				{ID: "repo/Widget.cs::Widget", Name: "Widget", Kind: graph.KindType, Language: "csharp", FilePath: "repo/Widget.cs", RepoPrefix: "repo", Meta: map[string]any{"ef_attribute_table": "new_widgets"}},
			}, []*graph.Edge{
				{From: "repo/Widget.cs::Widget", To: "repo/db::orm::old_widgets", Kind: graph.EdgeModelsTable, Meta: map[string]any{"orm": "efcore", "binding": "attribute", "table_name": "old_widgets"}},
			})
			var store graph.Store = &residualBatchCountingStore{Store: base}
			if failure != errors.ErrUnsupported {
				store = &frameworkReplacementFailureStore{Store: store, failure: failure}
			}
			core, logs := observer.New(zap.InfoLevel)
			mi := &MultiIndexer{graph: store, indexers: map[string]*Indexer{}, logger: zap.New(core)}
			report := mi.RunIncrementalDerivedPasses(t.Context(), map[string]DerivedInvalidationPlan{
				"repo": {Flags: DerivedInvalidatesDeclarations, Files: []string{"repo/Widget.cs"}},
			})
			if !report.Incomplete || !errors.Is(report.FrameworkError, failure) || len(report.FrameworkRefused) != 1 {
				t.Fatalf("EF refusal missing from report: %+v", report)
			}
			if logs.FilterMessage("incremental framework passes refused").Len() != 1 {
				t.Fatal("EF replacement failure looked like a complete empty pass")
			}
			out := base.GetOutEdges("repo/Widget.cs::Widget")
			if len(out) != 1 || out[0].To != "repo/db::orm::old_widgets" {
				t.Fatalf("refused framework pass changed mapping: %+v", out)
			}
		})
	}
}

func TestFrameworkReplacementRefusalReachesOwners(t *testing.T) {
	for _, operation := range []string{"index", "resolve_all", "global"} {
		t.Run(operation, func(t *testing.T) {
			// Given a mapping delta behind a stripped store, when its production
			// owner runs, then an error or named incomplete pass replaces silent success.
			base := graph.New()
			base.AddBatch([]*graph.Node{
				{ID: "repo/Widget.cs::Widget", Name: "Widget", Kind: graph.KindType, Language: "csharp", FilePath: "repo/Widget.cs", RepoPrefix: "repo", Meta: map[string]any{"ef_attribute_table": "new_widgets"}},
			}, []*graph.Edge{
				{From: "repo/Widget.cs::Widget", To: "repo/db::orm::old_widgets", Kind: graph.EdgeModelsTable, Meta: map[string]any{"orm": "efcore", "binding": "attribute", "table_name": "old_widgets"}},
			})
			store := &residualBatchCountingStore{Store: base}
			core, logs := observer.New(zap.InfoLevel)
			idx := New(store, newTestRegistry(), config.Default().Index, zap.New(core))
			t.Cleanup(idx.Close)
			idx.SetRepoPrefix("repo")
			switch operation {
			case "index":
				root := t.TempDir()
				require.NoError(t, os.WriteFile(filepath.Join(root, "main.go"), []byte("package app\nfunc Main() {}\n"), 0600))
				result, err := idx.IndexCtx(t.Context(), root)
				require.ErrorIs(t, err, errors.ErrUnsupported)
				require.Nil(t, result)
			case "resolve_all":
				idx.ResolveAll()
				require.Len(t, logs.FilterMessage("indexer: framework passes refused").All(), 1)
			case "global":
				mi := &MultiIndexer{graph: store, indexers: map[string]*Indexer{}, logger: zap.New(core)}
				mi.runGlobalGraphPassesTopologyHeld(t.Context(), nil, false)
				require.Len(t, logs.FilterMessage("global pass: framework dispatch synthesis refused").All(), 1)
				require.Len(t, logs.FilterMessage("global passes incomplete").All(), 1)
				require.Empty(t, logs.FilterMessage("global passes complete").All())
			}
			out := base.GetOutEdges("repo/Widget.cs::Widget")
			require.Len(t, out, 1)
			require.Equal(t, "repo/db::orm::old_widgets", out[0].To)
		})
	}
}

func TestCrossRepoMaterializationRefusalReachesCoordinators(t *testing.T) {
	for _, operation := range []string{"incremental", "global_full", "global_scoped", "deferred", "deferred_exact", "pre_enrich"} {
		t.Run(operation, func(t *testing.T) {
			// Given an unmarked cross-repo base edge behind an unknown wrapper,
			// when a best-effort owner runs, then its result is explicitly incomplete.
			base := graph.New()
			base.AddBatch([]*graph.Node{
				{ID: "a/a.go::A", Kind: graph.KindFunction, FilePath: "a/a.go", RepoPrefix: "a"},
				{ID: "b/b.go::B", Kind: graph.KindFunction, FilePath: "b/b.go", RepoPrefix: "b"},
			}, []*graph.Edge{{From: "a/a.go::A", To: "b/b.go::B", Kind: graph.EdgeCalls, FilePath: "a/a.go", Line: 3}})
			store := &residualBatchCountingStore{Store: base}
			core, logs := observer.New(zap.InfoLevel)
			mi := &MultiIndexer{graph: store, indexers: map[string]*Indexer{}, logger: zap.New(core)}
			switch operation {
			case "incremental":
				report := mi.RunIncrementalDerivedPasses(t.Context(), map[string]DerivedInvalidationPlan{
					"b": {Flags: DerivedInvalidatesDeclarations, Files: []string{"b/b.go"}},
				})
				require.ErrorIs(t, report.CrossRepoError, errors.ErrUnsupported)
				require.True(t, report.Incomplete)
				require.Zero(t, report.CrossRepo)
				require.Len(t, logs.FilterMessage("incremental cross-repo materialization refused").All(), 1)
				require.Empty(t, logs.FilterMessage("incremental derived passes complete").All())
			case "global_full", "global_scoped":
				var scope map[string]struct{}
				if operation == "global_scoped" {
					scope = map[string]struct{}{"b": {}}
				}
				mi.runGlobalGraphPassesTopologyHeld(t.Context(), scope, false)
				require.Len(t, logs.FilterMessage("global pass: cross-repo edges refused").All(), 1)
				require.Len(t, logs.FilterMessage("global passes incomplete").All(), 1)
				require.Empty(t, logs.FilterMessage("global passes complete").All())
			case "pre_enrich":
				require.ErrorIs(t, mi.RunPreEnrichResolveFiles(t.Context(), []string{"b/b.go"}, nil), errors.ErrUnsupported)
			case "deferred_exact":
				mode, complete := mi.resolveDeferredMutations(&graph.MutationReceipt{Complete: true, ChangedFiles: []string{"a/a.go"}}, true, nil, false)
				require.Equal(t, deferredResolveFailed, mode)
				require.False(t, complete)
			case "deferred":
				mode, complete := mi.resolveDeferredMutations(nil, true, nil, false)
				require.Equal(t, deferredResolveFailed, mode)
				require.False(t, complete)
				require.Len(t, logs.FilterMessage("DEFERRED-TIMING fallback cross-repo materialization refused").All(), 1)
			}
			for _, edge := range base.GetOutEdges("a/a.go::A") {
				require.False(t, edge.CrossRepo)
				require.NotEqual(t, graph.EdgeCrossRepoCalls, edge.Kind)
			}
		})
	}
}

func TestPreEnrichMaterializationRefusalFinishesIndependentTails(t *testing.T) {
	t.Setenv("GORTEX_SYNTH_EXTERNAL_CALLS", "1")
	for _, supported := range []bool{false, true} {
		t.Run(map[bool]string{false: "without_marker", true: "native_graph"}[supported], func(t *testing.T) {
			// Given base resolution and an independent missing flag capability,
			// when the exact pass refuses, then Go terminals and test edges remain complete.
			base := graph.New()
			base.AddBatch([]*graph.Node{
				{ID: "a/a_test.go", Kind: graph.KindFile, Name: "a_test.go", FilePath: "a/a_test.go", RepoPrefix: "a", WorkspaceID: "shared", Language: "go"},
				{ID: "a/a_test.go::TestA", Kind: graph.KindFunction, Name: "TestA", FilePath: "a/a_test.go", RepoPrefix: "a", WorkspaceID: "shared", Language: "go"},
				{ID: "b/b.go", Kind: graph.KindFile, Name: "b.go", FilePath: "b/b.go", RepoPrefix: "b", WorkspaceID: "shared", Language: "go"},
				{ID: "b/b.go::B", Kind: graph.KindFunction, Name: "B", FilePath: "b/b.go", RepoPrefix: "b", WorkspaceID: "shared", Language: "go"},
			}, []*graph.Edge{{From: "a/a_test.go", To: "b/b.go", Kind: graph.EdgeImports, FilePath: "a/a_test.go", Line: 1}})
			var store graph.Store = base
			if !supported {
				store = &struct {
					graph.Store
					graph.ScopedEdgeKindEvicter
				}{Store: base, ScopedEdgeKindEvicter: base}
			}
			idx := New(store, newTestRegistry(), config.Default().Index, zap.NewNop())
			t.Cleanup(idx.Close)
			mi := &MultiIndexer{graph: store, indexers: map[string]*Indexer{"a": idx}, logger: zap.NewNop()}
			hookCalls := 0
			err := mi.RunPreEnrichResolveFiles(t.Context(), []string{"a/a_test.go"}, func() {
				hookCalls++
				base.AddBatch(nil, []*graph.Edge{
					{From: "a/a_test.go::TestA", To: "b/b.go::B", Kind: graph.EdgeCalls, FilePath: "a/a_test.go", Line: 3},
					{From: "a/a_test.go::TestA", To: graph.UnresolvedMarker + "B", Kind: graph.EdgeCalls, FilePath: "a/a_test.go", Line: 4},
					{From: "a/a_test.go::TestA", To: graph.UnresolvedMarker + "extern::example.org/missing::Missing", Kind: graph.EdgeCalls, FilePath: "a/a_test.go", Line: 5, Meta: map[string]any{"go_import": "example.org/missing::Missing"}},
				})
			})
			if supported {
				require.NoError(t, err)
			} else {
				require.ErrorIs(t, err, errors.ErrUnsupported)
			}
			require.Equal(t, 1, hookCalls)
			require.NotNil(t, findEdge(base, graph.EdgeCalls, "a/a_test.go::TestA", "b/b.go::B"), "cross-repo resolution must have run")
			assert.NotNil(t, findEdge(base, graph.EdgeTests, "a/a_test.go::TestA", "b/b.go::B"), "retargeted test projection must finish before returning refusal")
			assert.NotNil(t, base.GetNode("external-call::dep::example.org/missing"), "demoted Go-call synthesis must finish before returning refusal")
			require.NotNil(t, findEdge(base, graph.EdgeCalls, "a/a_test.go::TestA", "external-call::dep::example.org/missing"))
		})
	}
}

// deferredUnmarkedStore applies target changes but injects a missing promoted flag.
// The missing marker must refuse only materialization, not independent queue tails.
type deferredUnmarkedStore struct {
	graph.Store
	graph.ScopedEdgeKindEvicter
}

func (s *deferredUnmarkedStore) ReindexEdges(batch []graph.EdgeReindex) {
	for _, row := range batch {
		row.Edge.CrossRepo = false
	}
	s.Store.ReindexEdges(batch)
}

func TestDeferredGoModuleMaterializationRefusalKeepsSynthesis(t *testing.T) {
	t.Setenv("GORTEX_SYNTH_EXTERNAL_CALLS", "1")
	for _, operation := range []string{"deferred_owner", "global_owner", "untrack_owner"} {
		for _, supported := range []bool{false, true} {
			t.Run(operation+map[bool]string{false: "/without_marker", true: "/native_graph"}[supported], func(t *testing.T) {
				// Given a published shadow queue with a bound and a missing symbol,
				// when materialization refuses, then taken work still gets its tails.
				base := graph.New()
				var store graph.Store = base
				if !supported {
					store = &deferredUnmarkedStore{Store: base, ScopedEdgeKindEvicter: base}
				}
				library := t.TempDir()
				writeFile(t, filepath.Join(library, "go.mod"), "module example.org/lib\n")
				cm := newTestConfigManager(t)
				cm.Global().Repos = []config.RepoEntry{{Name: "b", Path: library, Workspace: "shared"}}
				mi := NewMultiIndexer(store, newTestRegistry(), search.NewNull(), cm, zap.NewNop())
				t.Cleanup(func() { require.NoError(t, mi.Close(context.Background())) })
				shadow := graph.New()
				shadow.AddNode(&graph.Node{ID: "a/a_test.go", Name: "a_test.go", Kind: graph.KindFile, RepoPrefix: "a", WorkspaceID: "shared", FilePath: "a/a_test.go", Language: "go"})
				shadow.AddNode(&graph.Node{ID: "a/a_test.go::TestA", Name: "TestA", Kind: graph.KindFunction, RepoPrefix: "a", WorkspaceID: "shared", FilePath: "a/a_test.go", Language: "go"})
				names := []string{"Fn", "Missing"}
				if operation == "untrack_owner" {
					names = make([]string, 513)
					for i := range names {
						names[i] = "Fn"
					}
					names[len(names)-1] = "Missing"
				}
				for line, name := range names {
					shadow.AddEdge(&graph.Edge{From: "a/a_test.go::TestA", To: graph.UnresolvedMarker + "extern::example.org/lib::" + name, Kind: graph.EdgeCalls, FilePath: "a/a_test.go", Line: line + 1, Meta: map[string]any{"go_import": "example.org/lib::" + name}})
				}
				r := resolver.New(shadow)
				r.SetGoModules(map[string]string{"b": "example.org/lib"})
				r.SetDeferGoModuleMisses(true)
				r.ResolveAll()
				base.AddBatch(shadow.AllNodes(), shadow.AllEdges())
				base.AddNode(&graph.Node{ID: "b/lib.go::Fn", Name: "Fn", Kind: graph.KindFunction, RepoPrefix: "b", WorkspaceID: "shared", FilePath: "b/lib.go", Language: "go"})
				r.SetGraph(store)
				r.SetDeferGoModuleMisses(false)
				idx := New(store, newTestRegistry(), config.Default().Index, zap.NewNop())
				idx.resolver = r
				mi.indexers["a"] = idx
				var err error
				switch operation {
				case "deferred_owner":
					err = mi.resolveDeferredGoModules(idx)
				case "untrack_owner":
					queued := r.TakeDeferredGoModuleEdges()
					parked := make([]graph.EdgeIdentity, len(queued))
					for i, edge := range queued {
						parked[i] = graph.EdgeIdentityFor(edge)
					}
					require.Len(t, parked, 513, "exercise more than one 512-ID page")
					err = mi.finalizeUntrackedGoModuleCalls(parked)
				default:
					err = mi.runCrossRepoResolveContext(t.Context(), false)
				}
				if supported {
					require.NoError(t, err)
				} else {
					require.ErrorIs(t, err, errors.ErrUnsupported)
				}
				assert.Empty(t, r.TakeDeferredGoModuleEdges(), "the published queue was taken")
				bound := 0
				for _, edge := range base.GetOutEdges("a/a_test.go::TestA") {
					if edge.Kind == graph.EdgeCalls && edge.To == "b/lib.go::Fn" {
						bound++
					}
				}
				assert.Equal(t, len(names)-1, bound, "every page of the taken queue must bind")
				assert.NotNil(t, findEdge(base, graph.EdgeCalls, "a/a_test.go::TestA", "b/lib.go::Fn"), "base binding remains applied")
				assert.NotNil(t, findEdge(base, graph.EdgeTests, "a/a_test.go::TestA", "b/lib.go::Fn"), "test projection must finish before returning the refusal")
				assert.NotNil(t, base.GetNode("external-call::dep::example.org/lib"), "synthesize the missing symbol from the taken queue")
				assert.NotNil(t, findEdge(base, graph.EdgeCalls, "a/a_test.go::TestA", "external-call::dep::example.org/lib"))
			})
		}
	}
}
