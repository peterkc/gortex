package indexer

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
	"go.uber.org/zap/zaptest/observer"

	"github.com/zzet/gortex/internal/config"
	"github.com/zzet/gortex/internal/contracts"
	"github.com/zzet/gortex/internal/graph"
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
