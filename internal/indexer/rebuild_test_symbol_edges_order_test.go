package indexer

import (
	"maps"
	"slices"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/graph/store_pebble"
)

func TestRebuildTestSymbolsNativeFirstSiteOrder(t *testing.T) {
	ctx := t.Context()
	const (
		aPath, zPath, testPath = "a.go", "z.go", "suite_test.go"
		callerID               = testPath + "::TestSubject"
		helperID               = testPath + "::fixture.prepare"
		subjectID              = zPath + "::Subject"
	)
	caller := &graph.Node{ID: callerID, Kind: graph.KindFunction, Name: "TestSubject", FilePath: testPath,
		Language: "go", StartLine: 10, EndLine: 20, Meta: map[string]any{"fixture_tag": "caller"}}
	helper := &graph.Node{ID: helperID, Kind: graph.KindMethod, Name: "prepare", FilePath: testPath, Language: "go",
		StartLine: 25, EndLine: 27}
	// A stale stamp on a production callee is not membership in the native test set.
	subject := &graph.Node{ID: subjectID, Kind: graph.KindFunction, Name: "Subject", FilePath: zPath,
		Language: "go", StartLine: 10, EndLine: 14, Meta: map[string]any{"is_test": true}}
	callA := &graph.Edge{From: callerID, To: subjectID, Kind: graph.EdgeCalls, FilePath: aPath, Line: 90,
		Origin: graph.OriginASTResolved, Tier: "ast", Confidence: 0.75, ConfidenceLabel: "medium",
		Meta: map[string]any{"fixture_tag": "call payload"}}
	callZ := *callA
	callZ.FilePath, callZ.Line = zPath, 2
	help := &graph.Edge{From: callerID, To: helperID, Kind: graph.EdgeCalls, FilePath: testPath, Line: 12,
		Origin: graph.OriginASTResolved}
	missing := &graph.Edge{From: callerID, To: "unresolved::Missing", Kind: graph.EdgeCalls,
		FilePath: testPath, Line: 13, Origin: graph.OriginASTInferred}

	wantA := &graph.Edge{From: callerID, To: subjectID, Kind: graph.EdgeTests, FilePath: aPath, Line: 90,
		Origin: graph.OriginASTInferred}
	wantZ := &graph.Edge{From: callerID, To: subjectID, Kind: graph.EdgeTests, FilePath: zPath, Line: 2,
		Origin: graph.OriginASTInferred}
	old := *wantA
	old.Origin, old.Tier, old.Confidence, old.ConfidenceLabel = graph.OriginASTResolved, "ast", 0.9, "high"
	old.Meta = map[string]any{"fixture_tag": "old same-key payload"}
	other := old
	other.Line = 91
	other.Meta = map[string]any{"fixture_tag": "old different-site payload"}
	prior := []*graph.Edge{&old, &other}

	build, _ := openRebuildTestFileStore(t)
	// This is a post-link fixture, not a claim that extraction produces
	// cross-recorded calls. Each site's receipt belongs to its recording file;
	// the From symbol is declared in a separate, supported test file.
	rawA := graph.Edge{From: callerID, To: "unresolved::pkg.Subject", Kind: graph.EdgeCalls,
		FilePath: aPath, Line: 90, Origin: graph.OriginASTInferred}
	rawZ := rawA
	rawZ.To, rawZ.FilePath, rawZ.Line = "unresolved::Subject", zPath, 2
	var checked []store_pebble.ParseFacts
	var nodes []*graph.Node
	for _, f := range []struct {
		path  string
		nodes []*graph.Node
		edges []*graph.Edge
	}{
		{aPath, nil, []*graph.Edge{&rawA}},
		{testPath, []*graph.Node{caller, helper}, []*graph.Edge{help, missing}},
		{zPath, []*graph.Node{subject}, []*graph.Edge{&rawZ}},
	} {
		file := &graph.Node{ID: f.path, Kind: graph.KindFile, Name: f.path, FilePath: f.path, Language: "go"}
		decls := append([]*graph.Node{file}, f.nodes...)
		payload := encodeRebuildTestParseFacts(t, store_pebble.ParseFacts{
			File:  graph.FileMetaRow{FilePath: f.path, NodeCount: len(decls)},
			Nodes: decls, Edges: f.edges, EffectiveLanguage: "go",
		})
		facts := decodeRebuildTestParseFacts(t, payload)
		require.Equal(t, f.path, facts.File.FilePath)
		require.Equal(t, "go", facts.EffectiveLanguage)
		require.Equal(t, f.edges, facts.Edges, "exact From and recording site on %s", f.path)
		require.NoError(t, build.AddParsedFile("", facts))
		checked = append(checked, facts)
		nodes = append(nodes, decls...)
	}
	// Old targets put z before a in the resolver frontier. Once both calls
	// are settled to the same target, their complete keys put a before z.
	require.NoError(t, build.ApplyLinkReindexes(ctx, []graph.EdgeReindex{
		{Edge: &callZ, OldTo: rawZ.To}, {Edge: callA, OldTo: rawA.To},
	}))
	build.AddBatch(nil, prior)
	forward := []*graph.Edge{callA, &callZ, help, missing}
	reverse := []*graph.Edge{&callZ, callA, help, missing}
	input := slices.Concat(forward, prior)
	slices.SortFunc(input, compareRebuildTestFileEdgeKeys)
	requireRebuildTestFileRows(t, build, nodes, input)

	locked := rebuildTestSymbolsNativeOrder(t, nodes, forward, prior, []*graph.Edge{wantA, &other})
	rebuildTestSymbolsNativeOrder(t, nodes, reverse, prior, []*graph.Edge{&old, &other, wantZ})
	require.Equal(t, subject, locked.GetNode(subjectID), "stale stamp leaves the production node unchanged")
	want := locked.AllEdges()
	slices.SortFunc(want, compareRebuildTestFileEdgeKeys)

	receipts := func(yield func(store_pebble.ParseFacts, error) bool) {
		for _, facts := range checked {
			if !yield(facts, nil) {
				return
			}
		}
	}
	require.NoError(t, runRebuildTestFiles(ctx, build, receipts, nil))
	require.NoError(t, runRebuildTestSymbols(ctx, build, receipts))
	requireRebuildTestFileRows(t, build, locked.AllNodes(), want)
}

func rebuildTestSymbolsNativeOrder(t *testing.T, nodes []*graph.Node, calls, prior, wantTests []*graph.Edge) *graph.Graph {
	t.Helper()
	nativeNodes := make([]*graph.Node, 0, len(nodes))
	for _, n := range nodes {
		copy := *n
		copy.Meta = maps.Clone(n.Meta)
		nativeNodes = append(nativeNodes, &copy)
	}
	outgoing := slices.Concat(calls, prior)
	nativeEdges := make([]*graph.Edge, 0, len(outgoing))
	for _, e := range outgoing {
		copy := *e
		copy.Meta = maps.Clone(e.Meta)
		nativeEdges = append(nativeEdges, &copy)
	}
	g := graph.New()
	g.AddBatch(nativeNodes, nativeEdges)
	id := calls[0].From
	require.Equal(t, outgoing, g.GetOutEdgesByNodeIDs([]string{id})[id], "native outgoing order")
	marked, emitted := markTestSymbolsAndEmitEdges(g)
	require.Equal(t, 2, marked, "file-owned caller and helper method")
	require.Equal(t, 1, emitted, "subject only; helper and unresolved call are vetoed")
	require.ElementsMatch(t, slices.Concat(calls, wantTests), g.AllEdges(), "native Tests winner and full rows")
	return g
}
