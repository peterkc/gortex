package indexer

import (
	"iter"
	"maps"
	"slices"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/graph/store_pebble"
	"github.com/zzet/gortex/internal/parser"
	"github.com/zzet/gortex/internal/parser/languages"
)

func TestRebuildTestSymbolsDeferredAnnotationMembership(t *testing.T) {
	const testPath, prodPath, rustPath = "a_test.go", "work.go", "src/check.rs"
	testFile := &graph.Node{ID: testPath, Kind: graph.KindFile, Name: testPath, FilePath: testPath, Language: "go"}
	test := &graph.Node{ID: testPath + "::TestCover", Kind: graph.KindFunction, Name: "TestCover", FilePath: testPath, Language: "go"}
	prodFile := &graph.Node{ID: prodPath, Kind: graph.KindFile, Name: prodPath, FilePath: prodPath, Language: "go"}
	prod := &graph.Node{ID: prodPath + "::Work", Kind: graph.KindFunction, Name: "Work", FilePath: prodPath, Language: "go",
		Meta: map[string]any{"is_test": true}}
	rustFile := &graph.Node{ID: rustPath, Kind: graph.KindFile, Name: "check.rs", FilePath: rustPath, Language: "rust"}
	deferred := &graph.Node{ID: rustPath + "::check", Kind: graph.KindFunction, Name: "check", FilePath: rustPath, Language: "rust"}
	var extracted parser.ExtractionResult
	languages.EmitAnnotationEdge(deferred.ID, "rust", "tokio::test", "", rustPath, 1, &extracted, make(map[string]bool))
	require.Len(t, extracted.Edges, 1)
	annotation := extracted.Edges[0]
	toDeferred := &graph.Edge{From: test.ID, To: deferred.ID, Kind: graph.EdgeCalls, FilePath: testPath, Line: 4, Origin: graph.OriginASTResolved}
	toProd := &graph.Edge{From: test.ID, To: prod.ID, Kind: graph.EdgeCalls, FilePath: testPath, Line: 5, Origin: graph.OriginASTResolved}
	fromDeferred := &graph.Edge{From: deferred.ID, To: prod.ID, Kind: graph.EdgeCalls, FilePath: rustPath, Line: 3, Origin: graph.OriginASTResolved}
	want := &graph.Edge{From: test.ID, To: prod.ID, Kind: graph.EdgeTests, FilePath: testPath, Line: 5, Origin: graph.OriginASTInferred}
	rustNodes := append([]*graph.Node{rustFile, deferred}, extracted.Nodes...)
	nodes := append([]*graph.Node{testFile, test, prodFile, prod}, rustNodes...)
	calls := []*graph.Edge{toDeferred, toProd, fromDeferred}
	for _, annotated := range []bool{false, true} {
		native := graph.New()
		for _, n := range nodes {
			copy := *n
			copy.Meta = maps.Clone(n.Meta)
			native.AddNode(&copy)
		}
		native.AddBatch(nil, calls)
		other := &graph.Edge{From: test.ID, To: deferred.ID, Kind: graph.EdgeTests, FilePath: testPath, Line: 4, Origin: graph.OriginASTInferred}
		if annotated {
			native.AddBatch(nil, []*graph.Edge{annotation})
			other = &graph.Edge{From: deferred.ID, To: prod.ID, Kind: graph.EdgeTests, FilePath: rustPath, Line: 3, Origin: graph.OriginASTInferred}
		}
		markTestSymbolsAndEmitEdges(native)
		require.ElementsMatch(t, []*graph.Edge{want, other}, slices.Collect(native.EdgesByKind(graph.EdgeTests)), "native annotated=%t", annotated)
		if annotated {
			require.Equal(t, map[string]any{"is_test": true, "test_role": "test", "test_runner": "cargo-test"}, native.GetNode(deferred.ID).Meta)
		}
	}

	build, _ := openRebuildTestFileStore(t)
	receipts := seedRebuildTestSymbolReceipts(t, build, []store_pebble.ParseFacts{
		{EffectiveLanguage: "go", File: graph.FileMetaRow{FilePath: testPath, NodeCount: 2}, Nodes: []*graph.Node{testFile, test}, Edges: calls[:2]},
		{EffectiveLanguage: "rust", File: graph.FileMetaRow{FilePath: rustPath, NodeCount: len(rustNodes)}, Nodes: rustNodes, Edges: []*graph.Edge{annotation, fromDeferred}},
		{EffectiveLanguage: "go", File: graph.FileMetaRow{FilePath: prodPath, NodeCount: 2}, Nodes: []*graph.Node{prodFile, prod}},
	})
	require.NoError(t, runRebuildTestFiles(t.Context(), build, receipts, nil))
	require.NoError(t, runRebuildTestSymbols(t.Context(), build, receipts))
	stampedFile, stampedTest := *testFile, *test
	stampedFile.Meta = map[string]any{"is_test_file": true, "test_runner": "gotest"}
	stampedTest.Meta = map[string]any{"is_test": true, "test_role": "test", "test_runner": "gotest"}
	wantNodes := append([]*graph.Node{&stampedFile, &stampedTest}, nodes[2:]...)
	wantEdges := append(slices.Clone(calls), annotation, want)
	slices.SortFunc(wantEdges, compareRebuildTestFileEdgeKeys)
	requireRebuildTestFileRows(t, build, wantNodes, wantEdges)
}

func TestRebuildTestSymbolsRejectsUnprovedProducer(t *testing.T) {
	const testPath, prodPath, foreignPath = "a_test.go", "work.go", "src/foreign.rs"
	for _, kind := range []graph.EdgeKind{graph.EdgeAnnotated, graph.EdgeCalls} {
		t.Run(string(kind), func(t *testing.T) {
			testFile := &graph.Node{ID: testPath, Kind: graph.KindFile, Name: testPath, FilePath: testPath, Language: "go"}
			test := &graph.Node{ID: testPath + "::TestCover", Kind: graph.KindFunction, Name: "TestCover", FilePath: testPath, Language: "go"}
			safe := &graph.Node{ID: prodPath + "::Safe", Kind: graph.KindFunction, Name: "Safe", FilePath: prodPath, Language: "go"}
			prod := &graph.Node{ID: prodPath + "::Work", Kind: graph.KindFunction, Name: "Work", FilePath: prodPath, Language: "go"}
			safeCall := &graph.Edge{From: test.ID, To: safe.ID, Kind: graph.EdgeCalls, FilePath: testPath, Line: 3, Origin: graph.OriginASTResolved}
			settled := &graph.Edge{From: test.ID, To: prod.ID, Kind: graph.EdgeCalls, FilePath: testPath, Line: 4, Origin: graph.OriginASTResolved}
			raw := *settled
			raw.To, raw.Origin = "unresolved::*.Work", graph.OriginASTInferred
			foreign := store_pebble.ParseFacts{EffectiveLanguage: "rust", File: graph.FileMetaRow{FilePath: foreignPath}}
			if kind == graph.EdgeAnnotated {
				// The saved target is recognized, but the actual receipt is not
				// the annotation's recording file. It could veto the settled call.
				var extracted parser.ExtractionResult
				languages.EmitAnnotationEdge(prod.ID, "rust", "test", "", foreignPath, 1, &extracted, make(map[string]bool))
				require.Len(t, extracted.Edges, 1)
				extracted.Edges[0].FilePath = prodPath
				foreign.Nodes, foreign.Edges = extracted.Nodes, extracted.Edges
				foreign.File.NodeCount = len(foreign.Nodes)
			} else {
				// A different raw To cannot disambiguate a producer once linking
				// has changed To. Both receipts record the exact same call site.
				competing := raw
				competing.To = "unresolved::other.Work"
				foreign.Edges = []*graph.Edge{&competing}
			}
			build, _ := openRebuildTestFileStore(t)
			receipts := seedRebuildTestSymbolReceipts(t, build, []store_pebble.ParseFacts{
				{EffectiveLanguage: "go", File: graph.FileMetaRow{FilePath: testPath, NodeCount: 2}, Nodes: []*graph.Node{testFile, test}, Edges: []*graph.Edge{safeCall, &raw}},
				{EffectiveLanguage: "go", File: graph.FileMetaRow{FilePath: prodPath, NodeCount: 2}, Nodes: []*graph.Node{safe, prod}},
				foreign,
			})
			require.NoError(t, build.ApplyLinkReindexes(t.Context(), []graph.EdgeReindex{{Edge: settled, OldTo: raw.To}}))
			require.NoError(t, runRebuildTestFiles(t.Context(), build, receipts, nil))
			stampedFile := *testFile
			stampedFile.Meta = map[string]any{"is_test_file": true, "test_runner": "gotest"}
			wantNodes := append([]*graph.Node{&stampedFile, test, safe, prod}, foreign.Nodes...)
			wantEdges := append([]*graph.Edge{safeCall, settled}, foreign.Edges...)
			slices.SortFunc(wantEdges, compareRebuildTestFileEdgeKeys)
			requireRebuildTestFileRows(t, build, wantNodes, wantEdges)
			var unknown *store_pebble.RebuildUnknownProvenanceError
			require.ErrorAs(t, runRebuildTestSymbols(t.Context(), build, receipts), &unknown)
			require.EqualValues(t, 1, unknown.Count)
			requireRebuildTestFileRows(t, build, wantNodes, wantEdges)
		})
	}
}

func seedRebuildTestSymbolReceipts(t *testing.T, build *store_pebble.BuildStore, facts []store_pebble.ParseFacts) iter.Seq2[store_pebble.ParseFacts, error] {
	t.Helper()
	for _, receipt := range facts {
		require.NoError(t, build.AddParsedFile("", receipt))
	}
	return func(yield func(store_pebble.ParseFacts, error) bool) {
		for _, receipt := range facts {
			if !yield(receipt, nil) {
				return
			}
		}
	}
}
