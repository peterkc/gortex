package resolver

import (
	"path/filepath"
	"slices"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/graph/store_pebble"
	"github.com/zzet/gortex/internal/graph/store_sqlite"
)

func TestRebuildDataflowCalleesNativeReplay(t *testing.T) {
	const file, sibling = "pkg/use.go", "pkg/decl.go"
	const arg = "unresolved::arg"
	const do, run = file + "::Do", file + "::T.Run"
	const pkgFn, odd = sibling + "::Package", file + "::unresolved::Odd"
	nodes := []*graph.Node{
		{ID: do, Name: "Do", Kind: graph.KindFunction, FilePath: file},
		{ID: sibling + "::Do", Name: "Do", Kind: graph.KindFunction, FilePath: sibling},
		{ID: run, Name: "Run", Kind: graph.KindMethod, FilePath: file},
		{ID: file + "::T.Ambiguous", Name: "Ambiguous", Kind: graph.KindMethod, FilePath: file},
		{ID: file + "::U.Ambiguous", Name: "Ambiguous", Kind: graph.KindMethod, FilePath: file},
		{ID: sibling + "::Ambiguous", Name: "Ambiguous", Kind: graph.KindFunction, FilePath: sibling},
		{ID: pkgFn, Name: "Package", Kind: graph.KindFunction, FilePath: sibling},
		{ID: sibling + "::T.Package", Name: "Package", Kind: graph.KindMethod, FilePath: sibling},
		{ID: sibling + "::T.MethodOnly", Name: "MethodOnly", Kind: graph.KindMethod, FilePath: sibling},
		{ID: sibling + "::Shared", Name: "Shared", Kind: graph.KindFunction, FilePath: sibling},
		{ID: "pkg/other.go::Shared", Name: "Shared", Kind: graph.KindFunction, FilePath: "pkg/other.go"},
		{ID: odd, Name: "Odd", Kind: graph.KindFunction, FilePath: file},
	}
	// None of these source IDs has a node or matches the dataflow source.
	// Neither the site sources nor the selected opaque target are joins to nodes.
	sites := []*graph.Edge{
		{From: "call", To: run, Kind: graph.EdgeCalls, FilePath: file, Line: 10},
		{From: "ref", To: run, Kind: graph.EdgeReferences, FilePath: file, Line: 10},
		{From: "ref", To: "other.Other", Kind: graph.EdgeReferences, FilePath: file, Line: 10},
		{From: "ref", To: "unresolved::T.Run", Kind: graph.EdgeReferences, FilePath: file, Line: 10},
		{From: "ref", To: "foreign.Run", Kind: graph.EdgeReferences, FilePath: sibling, Line: 10},
		{From: "ref", To: run, Kind: graph.EdgeReferences, FilePath: file, Line: 11},
		{From: "call", To: run, Kind: graph.EdgeCalls, FilePath: file, Line: 20},
		{From: "ref", To: "other.Run", Kind: graph.EdgeReferences, FilePath: file, Line: 20},
		{From: "ref", To: "opaque::Run", Kind: graph.EdgeReferences, FilePath: file, Line: 22},
		{From: "call", To: run, Kind: graph.EdgeCalls, FilePath: file, Line: 0},
	}
	arrival := slices.Clone(sites)
	stable := slices.Clone(sites)
	var collisions []*graph.Edge
	for i, row := range []struct {
		kind      graph.EdgeKind
		line      int
		old, want string
	}{
		// Each pair converges on run. In complete old-key order the bare
		// name wins the first pair and the wildcard wins the second.
		{graph.EdgeArgOf, 10, "unresolved::*.Run", ""},
		{graph.EdgeArgOf, 10, "repo::unresolved::Run", ""},
		{graph.EdgeValueFlow, 11, "unresolved::Run", ""},
		{graph.EdgeValueFlow, 11, "repo::unresolved::*.Run", ""},
		{graph.EdgeArgOf, 12, "unresolved::Do", do},
		{graph.EdgeValueFlow, 13, "unresolved::Run", run},
		{graph.EdgeArgOf, 14, "unresolved::Ambiguous", "unresolved::Ambiguous"},
		{graph.EdgeValueFlow, 15, "unresolved::Package", pkgFn},
		{graph.EdgeArgOf, 16, "unresolved::MethodOnly", "unresolved::MethodOnly"},
		{graph.EdgeArgOf, 17, "unresolved::Shared", "unresolved::Shared"},
		{graph.EdgeArgOf, 20, "unresolved::*.Run", "unresolved::*.Run"}, // distinct matching targets
		{graph.EdgeArgOf, 21, "unresolved::*.Run", "unresolved::*.Run"}, // no match at this line
		{graph.EdgeArgOf, 22, "unresolved::*.Run", "opaque::Run"},
		{graph.EdgeArgOf, 0, "unresolved::*.Run", "unresolved::*.Run"}, // no positive site line
		{graph.EdgeValueFlow, 30, "unresolved::Do", ""},                // consumed by a stationary row
		{graph.EdgeValueFlow, 30, do, do},
		{graph.EdgeValueFlow, 40, "unresolved::Odd", odd},
		{graph.EdgeValueFlow, 41, "unresolved::Odd", ""}, // odd but stationary destination
		{graph.EdgeValueFlow, 41, odd, odd},
	} {
		// Use only saved SQLite fields. In particular, metadata is string-valued
		// so both stores return the same types, and render-only fields stay zero.
		e := &graph.Edge{From: arg, To: row.old, Kind: row.kind, FilePath: file, Line: row.line,
			Confidence: 0.25, ConfidenceLabel: "heuristic", Origin: "extractor", Tier: "syntax",
			Meta: map[string]any{"old_target": row.old}}
		if i%2 == 1 {
			e.Confidence, e.ConfidenceLabel = 0.75, "confirmed"
			e.Origin, e.Tier, e.CrossRepo = "fixture", "semantic", true
		}
		arrival = append(arrival, e)
		if i < 4 {
			collisions = append(collisions, e)
		}
		if row.want != "" {
			moved := *e
			moved.To = row.want
			stable = append(stable, &moved)
		}
	}
	wantRows := func(winners ...*graph.Edge) []*graph.Edge {
		rows := slices.Clone(stable)
		for _, winner := range winners {
			moved := *winner
			moved.To = run
			rows = append(rows, &moved)
		}
		slices.SortFunc(rows, compareRebuildImportEdges)
		return rows
	}
	ordered := slices.Clone(arrival)
	slices.SortFunc(ordered, compareRebuildImportEdges)
	locked := rebuildDataflowCalleeSQLite(t, nodes, ordered)
	require.Equal(t, wantRows(collisions[1], collisions[3]), locked)

	reversed := slices.Clone(arrival)
	slices.Reverse(reversed)
	const consumed = 11 // four convergent rows, two stationary losers, five other moves
	for _, tc := range []struct {
		name    string
		input   []*graph.Edge
		winners []*graph.Edge
	}{
		{"arrival", arrival, []*graph.Edge{collisions[0], collisions[2]}},
		{"reversed", reversed, []*graph.Edge{collisions[1], collisions[3]}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			require.Equal(t, wantRows(tc.winners...), rebuildDataflowCalleeSQLite(t, nodes, tc.input))
			build := openRebuildBareScopeFixture(t)
			build.AddBatch(nodes, tc.input)
			require.Equal(t, ordered, rebuildFnPointerSaved(t, build))

			count, err := build.RebuildDataflowCallees(t.Context())
			require.NoError(t, err)
			require.Equal(t, consumed, count)
			require.Equal(t, locked, rebuildFnPointerSaved(t, build))

			count, err = build.RebuildDataflowCallees(t.Context())
			require.NoError(t, err)
			require.Zero(t, count)
			require.Equal(t, locked, rebuildFnPointerSaved(t, build))
		})
	}
}

func TestRebuildDataflowCalleesRejectsMovingDestination(t *testing.T) {
	const file = "pkg/chain.go"
	const middle, last = file + "::plugin::unresolved::Second", file + "::Second"
	// A producer can supply a callable named First whose ID itself looks like
	// an unresolved Second. The complete destination of first is second's old key.
	nodes := []*graph.Node{
		{ID: middle, Name: "First", Kind: graph.KindFunction, FilePath: file},
		{ID: last, Name: "Second", Kind: graph.KindFunction, FilePath: file},
	}
	first := &graph.Edge{From: "unresolved::arg", To: "unresolved::First", Kind: graph.EdgeValueFlow, FilePath: file, Line: 8,
		Meta: map[string]any{"producer": "plugin", "row": "first"}}
	second := &graph.Edge{From: first.From, To: middle, Kind: first.Kind, FilePath: file, Line: 8,
		Meta: map[string]any{"producer": "plugin", "row": "second"}}
	independent := &graph.Edge{From: "earlier", To: "unresolved::Second", Kind: graph.EdgeArgOf, FilePath: file, Line: 7,
		Meta: map[string]any{"row": "independent"}}
	input := []*graph.Edge{first, second, independent}
	build := openRebuildBareScopeFixture(t)
	build.AddBatch(nodes, input)
	slices.SortFunc(input, compareRebuildImportEdges)
	require.Equal(t, input, rebuildFnPointerSaved(t, build))

	count, err := build.RebuildDataflowCallees(t.Context())
	require.Zero(t, count)
	require.ErrorIs(t, err, store_pebble.ErrRebuildAttributionChain)
	var chain *store_pebble.RebuildAttributionChainError
	require.ErrorAs(t, err, &chain)
	require.Equal(t, uint64(1), chain.Count)
	require.Equal(t, graph.EdgeIdentityFor(first), chain.Old)
	require.Equal(t, graph.EdgeIdentityFor(second), chain.Destination)
	require.Equal(t, input, rebuildFnPointerSaved(t, build))
}

func TestRunRebuildMainDataflowCalleesAfterLexical(t *testing.T) {
	const file = "pkg/use.go"
	const owner = file + "::F"
	const local = owner + "#local:invoke::Run"
	nodes := []*graph.Node{
		{ID: owner, Name: "F", Kind: graph.KindFunction, FilePath: file, Language: "go"},
		{ID: local, Name: "invoke", Kind: graph.KindLocal, FilePath: file, StartLine: 2},
	}
	ref := &graph.Edge{From: owner, To: "unresolved::invoke", Kind: graph.EdgeReferences, FilePath: file, Line: 5}
	arg := &graph.Edge{From: "unresolved::arg", To: "unresolved::*.Run", Kind: graph.EdgeArgOf, FilePath: file, Line: 5}
	build := openRebuildBareScopeFixture(t)
	build.AddBatch(nodes, []*graph.Edge{ref, arg})
	require.Equal(t, []*graph.Edge{ref, arg}, rebuildFnPointerSaved(t, build))

	// Selecting no callers keeps the candidate worker closed. The raw frontier
	// still enables the tails; only the newly bound reference provides Run's site.
	seen := 0
	options := store_pebble.RebuildLinkOptions{ScratchParent: t.TempDir(), SpillBytes: 1}
	require.NoError(t, RunRebuildMain(t.Context(), build, options, RebuildMainInputs{
		SelectCallers: func(page []*graph.Edge) ([]*graph.Edge, error) {
			seen += len(page)
			return nil, nil
		},
	}))
	require.Equal(t, 2, seen)
	boundRef, boundArg := *ref, *arg
	boundRef.To, boundArg.To = local, local
	require.Equal(t, []*graph.Edge{&boundRef, &boundArg}, rebuildFnPointerSaved(t, build))
}

func rebuildDataflowCalleeSQLite(t *testing.T, nodes []*graph.Node, input []*graph.Edge) []*graph.Edge {
	t.Helper()
	sql, err := store_sqlite.Open(filepath.Join(t.TempDir(), "native.sqlite"))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, sql.Close()) })
	require.NoError(t, sql.AddBatchChecked(nodes, nil))
	for _, e := range input {
		// One insert per row fixes rowids. The base light census has no
		// ORDER BY; its kind index presents same-kind ties in rowid order
		// here. The different saved winners above verify that observation.
		require.NoError(t, sql.AddBatchChecked(nil, []*graph.Edge{e}))
	}
	r := New(sql)
	census, stats := r.buildPostBareAttributionCensus(defaultAttributionCensusLimits)
	require.NotNil(t, census)
	defer census.close()
	require.Empty(t, stats.fallbackReason)
	require.Equal(t, len(input), stats.rowsScanned)
	r.bindDataflowCalleeRefsFromCensus(census)
	rows := sql.AllEdges()
	slices.SortFunc(rows, compareRebuildImportEdges)
	return rows
}
