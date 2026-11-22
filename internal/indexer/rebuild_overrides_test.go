package indexer

import (
	"path/filepath"
	"slices"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/graph/store_pebble"
	"github.com/zzet/gortex/internal/graph/store_sqlite"
	"github.com/zzet/gortex/internal/resolver"
)

func TestRebuildOverridesNativeOrder(t *testing.T) {
	t.Run("last_same_name_member", testRebuildOverrideMemberOrder)
	t.Run("first_existing_override", testRebuildExistingOverrideOrder)
}

const (
	overrideFixturePath  = "classes.ts"
	overrideFixtureBase  = overrideFixturePath + "::Base"
	overrideFixtureChild = overrideFixturePath + "::Child"
)

func testRebuildOverrideMemberOrder(t *testing.T) {
	facts, parent := rebuildOverrideOrderFixture()
	// The TS extractor disambiguates same-name declarations (for example an
	// instance method and a static method) by line.
	// Both IDs survive MemberMethodsByType's MethodID deduplication.
	method := &graph.Node{ID: overrideFixtureChild + ".Run_L21", Name: "Run", Kind: graph.KindMethod,
		FilePath: overrideFixturePath, Language: "typescript", StartLine: 21, EndLine: 21}
	facts.Nodes = append(facts.Nodes, method)
	facts.File.NodeCount++
	facts.Edges = append(facts.Edges, &graph.Edge{From: method.ID, To: overrideFixtureChild,
		Kind: graph.EdgeMemberOf, FilePath: overrideFixturePath, Line: method.StartLine})

	var locked map[graph.EdgeIdentity]*graph.Edge
	for _, order := range []string{"ascending", "descending"} {
		input := facts
		input.Edges = slices.Clone(facts.Edges)
		contenders := input.Edges[2:]
		if order == "descending" {
			slices.Reverse(contenders)
		}
		native, err := store_sqlite.Open(filepath.Join(t.TempDir(), "native.sqlite"))
		require.NoError(t, err)
		t.Cleanup(func() { require.NoError(t, native.Close()) })
		require.NoError(t, native.AddBatchChecked(input.Nodes, []*graph.Edge{parent, input.Edges[1]}))
		// The SQLite projection orders by e.id. Commit each distinct member
		// separately, then check what the native resolver will actually read.
		for _, member := range contenders {
			require.NoError(t, native.AddBatchChecked(nil, []*graph.Edge{member}))
		}
		members := native.MemberMethodsByType()[overrideFixtureChild]
		require.Len(t, members, 2)
		require.Equal(t, contenders[0].From, members[0].MethodID)
		require.Equal(t, contenders[1].From, members[1].MethodID)

		before := slices.Concat([]*graph.Edge{parent}, input.Edges[1:])
		require.ElementsMatch(t, before, native.AllEdges(), "native settled input")
		require.Equal(t, 1, resolver.New(native).InferOverrides())
		want := &graph.Edge{From: overrideFixtureChild + ".Run_L21", To: overrideFixtureBase + ".Run",
			Kind: graph.EdgeOverrides, FilePath: overrideFixturePath, Line: 21,
			Confidence: 1, ConfidenceLabel: "EXTRACTED", Origin: graph.OriginASTResolved}
		if order == "descending" {
			want.From, want.Line = overrideFixtureChild+".Run", 7
		}
		rows := rebuildOverrideRowsByKey(t, native.AllEdges())
		require.Equal(t, rebuildOverrideRowsByKey(t, append(before, want)), rows, "native last member wins (%s)", order)
		if order == "ascending" {
			locked = rows // Ascending complete member keys (the From IDs differ).
		} else {
			require.NotContains(t, locked, graph.EdgeIdentityFor(want), "reverse native replay emitted a different key")
		}
		require.Equal(t, locked, runRebuildOverrideOrder(t, input, parent, nil), "private order %s", order)
	}
}

func testRebuildExistingOverrideOrder(t *testing.T) {
	facts, parent := rebuildOverrideOrderFixture()
	// Same method pair, two distinct sites with different payloads. Only
	// Origin and the already-populated Tier may change on the chosen site.
	low := &graph.Edge{From: overrideFixtureChild + ".Run", To: overrideFixtureBase + ".Run",
		Kind: graph.EdgeOverrides, FilePath: overrideFixturePath, Line: 9, Confidence: .75,
		ConfidenceLabel: "medium", Origin: graph.OriginASTInferred, Tier: "heuristic",
		Meta: map[string]any{"site": "early"}}
	high := *low
	high.Line, high.Confidence, high.ConfidenceLabel = 17, .5, "low"
	high.Meta = map[string]any{"site": "late"}

	var locked map[graph.EdgeIdentity]*graph.Edge
	for _, order := range []string{"ascending", "descending"} {
		existing := []*graph.Edge{low, &high}
		if order == "descending" {
			slices.Reverse(existing)
		}
		native := graph.New()
		base := slices.Concat([]*graph.Edge{parent}, facts.Edges[1:])
		native.AddBatch(facts.Nodes, base)
		for _, edge := range existing {
			copy := *edge // Graph's provenance mutation edits the stored row in place.
			native.AddEdge(&copy)
		}
		var presented []*graph.Edge
		for _, edge := range native.GetOutEdgesByNodeIDs([]string{low.From})[low.From] {
			if edge.Kind == graph.EdgeOverrides {
				presented = append(presented, edge)
			}
		}
		require.Equal(t, existing, presented, "native adjacency order")
		require.ElementsMatch(t, slices.Concat(base, existing), native.AllEdges(), "native settled input")
		require.Zero(t, resolver.New(native).InferOverrides(), "existing pair suppresses insertion")

		upgraded := *existing[0]
		upgraded.Origin, upgraded.Tier = graph.OriginASTResolved, "ast"
		rows := rebuildOverrideRowsByKey(t, native.AllEdges())
		require.Equal(t, rebuildOverrideRowsByKey(t, append(base, &upgraded, existing[1])), rows,
			"native only upgrades the first existing full key and keeps its payload (%s)", order)
		if order == "ascending" {
			locked = rows // Ascending complete existing keys (the line is the only key difference).
		} else {
			key := graph.EdgeIdentityFor(&upgraded)
			require.NotEqual(t, locked[key], rows[key], "reverse native replay changed content at the other key")
		}
		require.Equal(t, locked, runRebuildOverrideOrder(t, facts, parent, existing), "private order %s", order)
	}
}

// Model the TS parser's explicit `class Child extends Base` receipt, then
// retarget that raw edge to the settled base. Only the tail pass is under test.
func rebuildOverrideOrderFixture() (store_pebble.ParseFacts, *graph.Edge) {
	nodes := []*graph.Node{
		{ID: overrideFixtureBase, Name: "Base", Kind: graph.KindType, FilePath: overrideFixturePath,
			Language: "typescript", StartLine: 1, EndLine: 4},
		{ID: overrideFixtureBase + ".Run", Name: "Run", Kind: graph.KindMethod, FilePath: overrideFixturePath,
			Language: "typescript", StartLine: 2, EndLine: 2},
		{ID: overrideFixtureChild, Name: "Child", Kind: graph.KindType, FilePath: overrideFixturePath,
			Language: "typescript", StartLine: 6, EndLine: 22},
		{ID: overrideFixtureChild + ".Run", Name: "Run", Kind: graph.KindMethod, FilePath: overrideFixturePath,
			Language: "typescript", StartLine: 7, EndLine: 20},
	}
	raw := &graph.Edge{From: overrideFixtureChild, To: "unresolved::Base", Kind: graph.EdgeExtends,
		FilePath: overrideFixturePath, Line: 6, Origin: graph.OriginASTResolved,
		Meta: map[string]any{"ref_context": graph.RefContextInherit}}
	settled := *raw
	settled.To = overrideFixtureBase
	return store_pebble.ParseFacts{
		File:              graph.FileMetaRow{FilePath: overrideFixturePath, NodeCount: len(nodes)},
		EffectiveLanguage: "typescript",
		Nodes:             nodes,
		Edges: []*graph.Edge{
			raw,
			{From: nodes[1].ID, To: overrideFixtureBase, Kind: graph.EdgeMemberOf, FilePath: overrideFixturePath, Line: 2},
			{From: nodes[3].ID, To: overrideFixtureChild, Kind: graph.EdgeMemberOf, FilePath: overrideFixturePath, Line: 7},
		},
	}, &settled
}

func runRebuildOverrideOrder(t *testing.T, facts store_pebble.ParseFacts, parent *graph.Edge, existing []*graph.Edge) map[graph.EdgeIdentity]*graph.Edge {
	t.Helper()
	ctx := t.Context()
	build, err := store_pebble.OpenBuild(filepath.Join(t.TempDir(), "working"), store_pebble.FullProfile)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, build.Close()) })
	require.NoError(t, build.AddParsedFile("", facts))
	require.NoError(t, build.ApplyLinkReindexes(ctx, []graph.EdgeReindex{{Edge: parent, OldTo: facts.Edges[0].To}}))
	// The existing rows came through parse assembly too. Keep their raw
	// receipt membership so the proof knows which full keys can suppress.
	build.AddBatch(nil, existing)
	facts.Edges = slices.Concat(facts.Edges, existing)

	read := func() []*graph.Edge {
		t.Helper()
		var rows []*graph.Edge
		require.NoError(t, build.RebuildView().VisitEdges(ctx, func(e *graph.Edge) error {
			rows = append(rows, e)
			return nil
		}))
		return rows
	}
	require.ElementsMatch(t, slices.Concat([]*graph.Edge{parent}, facts.Edges[1:]), read(), "private settled input")
	require.NoError(t, resolver.RunRebuildOverrides(ctx, build, func(yield func(store_pebble.ParseFacts, error) bool) {
		yield(facts, nil)
	}))
	return rebuildOverrideRowsByKey(t, read())
}

func rebuildOverrideRowsByKey(t *testing.T, edges []*graph.Edge) map[graph.EdgeIdentity]*graph.Edge {
	t.Helper()
	rows := make(map[graph.EdgeIdentity]*graph.Edge, len(edges))
	for _, e := range edges {
		key := graph.EdgeIdentityFor(e)
		require.NotContains(t, rows, key, "duplicate row at full key")
		rows[key] = e
	}
	return rows
}
