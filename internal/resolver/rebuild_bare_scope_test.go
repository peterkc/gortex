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

func TestRebuildBareNameScopeNativeReplay(t *testing.T) {
	const file = "pkg/lex.go"
	const owner = file + "::F"
	const early, latest, future = owner + "#local:x:early", owner + "#local:x:latest", owner + "#local:x:future"
	const oddParam = owner + "#param:x:a" // SQLite binary order: A, Z, a
	const parked, later = owner + "#local:plugin::unresolved::parked", owner + "#local:parked:later"
	nodes := []*graph.Node{
		{ID: owner, Name: "F", Kind: graph.KindFunction, FilePath: file, Language: "go"},
		{ID: early, Name: "x", Kind: graph.KindLocal, FilePath: file, StartLine: 2},
		{ID: latest, Name: "x", Kind: graph.KindLocal, FilePath: file, StartLine: 10},
		{ID: future, Name: "x", Kind: graph.KindLocal, FilePath: file, StartLine: 50},
		{ID: owner + "#local:x:tieA", Name: "x", Kind: graph.KindLocal, FilePath: file, StartLine: 15},
		{ID: owner + "#local:x:tieB", Name: "x", Kind: graph.KindLocal, FilePath: file, StartLine: 15},
		{ID: oddParam, Name: "x", Kind: graph.KindParam, FilePath: file},
		{ID: owner + "#param:x:Z", Name: "x", Kind: graph.KindParam, FilePath: file},
		{ID: owner + "#param:x:A", Name: "x", Kind: graph.KindParam, FilePath: file},
		{ID: owner + "#param:even:1", Name: "even", Kind: graph.KindParam, FilePath: file},
		{ID: owner + "#param:even:2", Name: "even", Kind: graph.KindParam, FilePath: file},
		{ID: parked, Name: "parked", Kind: graph.KindLocal, FilePath: file, StartLine: 5},
		{ID: later, Name: "parked", Kind: graph.KindLocal, FilePath: file, StartLine: 80},
	}

	// Distinct old keys converge on the same read. The bare row arrives first
	// but sorts after the repository-prefixed row by the complete old key.
	bare := &graph.Edge{From: early, To: "unresolved::x", Kind: graph.EdgeReads, FilePath: file, Line: 10,
		Confidence: 0.25, ConfidenceLabel: "heuristic", Origin: "bare", Tier: "syntax",
		Meta: map[string]any{"winner": "bare"}}
	prefixed := *bare
	prefixed.To, prefixed.Confidence, prefixed.ConfidenceLabel = "repo::unresolved::x", 0.75, "confirmed"
	prefixed.Origin, prefixed.Tier, prefixed.CrossRepo = "prefixed", "semantic", true
	prefixed.Meta = map[string]any{"winner": "prefixed"}
	arrival := []*graph.Edge{bare, &prefixed}
	var stable []*graph.Edge
	for _, row := range []struct {
		kind      graph.EdgeKind
		line      int
		old, want string
	}{
		{graph.EdgeArgOf, 11, "unresolved::x", ""}, // loses to a stationary row, but counts
		{graph.EdgeArgOf, 11, latest, latest},
		{graph.EdgeReferences, 15, "unresolved::x", oddParam}, // latest local tie falls back, not to an older local
		{graph.EdgeReferences, 16, "unresolved::even", "unresolved::even"},
		{graph.EdgeValueFlow, 0, "unresolved::x", future},   // no site line: no upper bound
		{graph.EdgeArgOf, 20, "unresolved::parked", parked}, // odd spelling, no destination row at this site
		{graph.EdgeArgOf, 21, "unresolved::parked", ""},
		{graph.EdgeArgOf, 21, parked, parked}, // already at the chosen ID: do not treat as moving
		{graph.EdgeArgOf, 81, parked, later},  // same odd target at a different site really moves
		{graph.EdgeReads, 22, "unresolved::obj.x", "unresolved::obj.x"},
		{graph.EdgeCalls, 22, "unresolved::x", "unresolved::x"}, // not a lexical kind
	} {
		e := &graph.Edge{From: early, To: row.old, Kind: row.kind, FilePath: file, Line: row.line,
			Confidence: 0.5, ConfidenceLabel: "candidate", Origin: "fixture", Tier: "syntax",
			Meta: map[string]any{"old_target": row.old}}
		arrival = append(arrival, e)
		if row.want != "" {
			moved := *e
			moved.To = row.want
			stable = append(stable, &moved)
		}
	}
	wantRows := func(winner *graph.Edge) []*graph.Edge {
		moved := *winner
		moved.To = latest
		rows := append(slices.Clone(stable), &moved)
		slices.SortFunc(rows, compareRebuildImportEdges)
		return rows
	}
	ordered := slices.Clone(arrival)
	slices.SortFunc(ordered, compareRebuildImportEdges)
	locked := rebuildBareScopeSQLite(t, nodes, ordered)
	require.Equal(t, wantRows(&prefixed), locked)

	reversed := slices.Clone(arrival)
	slices.Reverse(reversed)
	const consumed = 8 // both convergent reads, two stationary losers and four other moves
	for _, tc := range []struct {
		name   string
		input  []*graph.Edge
		winner *graph.Edge
	}{
		{"bare_first", arrival, bare},
		{"prefixed_first", reversed, &prefixed},
	} {
		t.Run(tc.name, func(t *testing.T) {
			require.Equal(t, wantRows(tc.winner), rebuildBareScopeSQLite(t, nodes, tc.input))
			build := openRebuildBareScopeFixture(t)
			build.AddBatch(nodes, tc.input)
			require.Equal(t, ordered, rebuildFnPointerSaved(t, build))

			count, err := build.RebuildBareNameScope(t.Context())
			require.NoError(t, err)
			require.Equal(t, consumed, count)
			require.Equal(t, locked, rebuildFnPointerSaved(t, build))

			count, err = build.RebuildBareNameScope(t.Context())
			require.NoError(t, err)
			require.Zero(t, count)
			require.Equal(t, locked, rebuildFnPointerSaved(t, build))
		})
	}
}

func TestRebuildBareNameScopeRejectsMovingDestination(t *testing.T) {
	const file = "pkg/chain.go"
	const owner = file + "::F"
	const middle, last = owner + "#local:plugin::unresolved::second", owner + "#local:second"
	// Supplied plugin-shaped facts: the local named first has an ID that
	// itself spells an unresolved second. Both value-flow edges move: the
	// first onto the second edge's full old key, the second to last.
	nodes := []*graph.Node{
		{ID: owner, Name: "F", Kind: graph.KindFunction, FilePath: file},
		{ID: middle, Name: "first", Kind: graph.KindLocal, FilePath: file, StartLine: 2},
		{ID: last, Name: "second", Kind: graph.KindLocal, FilePath: file, StartLine: 3},
	}
	first := &graph.Edge{From: owner, To: "unresolved::first", Kind: graph.EdgeValueFlow, FilePath: file, Line: 8,
		Meta: map[string]any{"producer": "plugin", "row": "first"}}
	second := &graph.Edge{From: owner, To: middle, Kind: graph.EdgeValueFlow, FilePath: file, Line: 8,
		Meta: map[string]any{"producer": "plugin", "row": "second"}}
	// An independent move sorts before the chain. Even this row must not change.
	independent := &graph.Edge{From: owner, To: "unresolved::second", Kind: graph.EdgeReads, FilePath: file, Line: 7,
		Meta: map[string]any{"row": "independent"}}
	input := []*graph.Edge{first, second, independent}
	build := openRebuildBareScopeFixture(t)
	build.AddBatch(nodes, input)
	slices.SortFunc(input, compareRebuildImportEdges)
	require.Equal(t, input, rebuildFnPointerSaved(t, build))

	// Unselected raw pending rows still enable the native cold tail. With no
	// worker binary, an attempted candidate scan would fail instead.
	seen := 0
	options := store_pebble.RebuildLinkOptions{ScratchParent: t.TempDir(), SpillBytes: 1}
	err := RunRebuildMain(t.Context(), build, options, RebuildMainInputs{
		SelectCallers: func(page []*graph.Edge) ([]*graph.Edge, error) {
			seen += len(page)
			return nil, nil
		},
	})
	require.Equal(t, len(input), seen)
	require.ErrorIs(t, err, store_pebble.ErrRebuildAttributionChain)
	var chain *store_pebble.RebuildAttributionChainError
	require.ErrorAs(t, err, &chain)
	require.Equal(t, uint64(1), chain.Count)
	require.Equal(t, graph.EdgeIdentityFor(first), chain.Old)
	require.Equal(t, graph.EdgeIdentityFor(second), chain.Destination)
	require.Equal(t, input, rebuildFnPointerSaved(t, build))
}

func rebuildBareScopeSQLite(t *testing.T, nodes []*graph.Node, input []*graph.Edge) []*graph.Edge {
	t.Helper()
	sql, err := store_sqlite.Open(filepath.Join(t.TempDir(), "native.sqlite"))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, sql.Close()) })
	require.NoError(t, sql.AddBatchChecked(nodes, nil))
	for _, e := range input {
		// The native pending identity scan walks SQLite rowids, not old keys.
		require.NoError(t, sql.AddBatchChecked(nil, []*graph.Edge{e}))
	}
	New(sql).bindBareNameScopeRefs()
	rows := sql.AllEdges()
	slices.SortFunc(rows, compareRebuildImportEdges)
	return rows
}

func openRebuildBareScopeFixture(t *testing.T) *store_pebble.BuildStore {
	t.Helper()
	build, err := store_pebble.OpenBuild(filepath.Join(t.TempDir(), "build"), store_pebble.FullProfile)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, build.Close()) })
	return build
}
