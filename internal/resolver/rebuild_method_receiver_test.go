package resolver

import (
	"os"
	"path/filepath"
	"slices"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/graph/store_pebble"
	"github.com/zzet/gortex/internal/graph/store_sqlite"
	"github.com/zzet/gortex/internal/parser/languages"
)

func TestRebuildGoMethodReceiversNativeReplay(t *testing.T) {
	const file, types = "pkg/methods.go", "pkg/types.go"
	const targetT, targetU = types + "::T", types + "::U"
	out, err := languages.NewGoExtractor().Extract(file, []byte(`package p
func (*T) Multi() {}
func (*U) Sole() {}
func (*T) Stationary() {}
`))
	require.NoError(t, err)
	members := make(map[string]*graph.Edge)
	for _, e := range out.Edges {
		if e.Kind == graph.EdgeMemberOf {
			members[e.From] = e
		}
	}
	require.Len(t, members, 3)
	multi, sole, stationaryOld := members[file+"::T.Multi"], members[file+"::U.Sole"], members[file+"::T.Stationary"]
	require.NotNil(t, multi)
	require.NotNil(t, sole)
	require.NotNil(t, stationaryOld)
	require.Equal(t, file+"::T", multi.To)
	require.Equal(t, file+"::U", sole.To)

	// Post-parse replay witness: a configured plugin parsing another file can
	// supply this From, To, kind, Go source site and metadata. Its old key is
	// distinct but rebinding either row yields the same complete saved key.
	plugin := &graph.Edge{From: multi.From, To: "pkg/a-replay.go::T", Kind: multi.Kind, FilePath: multi.FilePath, Line: multi.Line,
		Meta: map[string]any{"producer": "plugin"}}
	stationary := *stationaryOld
	stationary.To, stationary.Confidence, stationary.Origin = targetT, 0.75, "stationary"
	stationary.Meta = map[string]any{"payload": "preserve canonical row"}
	ambiguous := &graph.Edge{From: "amb/m.go::T.M", To: "amb/m.go::T", Kind: graph.EdgeMemberOf, FilePath: "amb/m.go", Line: 2}
	foreign := &graph.Edge{From: "pkg/view.ts::T.M", To: "pkg/view.ts::T", Kind: graph.EdgeMemberOf, FilePath: "pkg/view.ts", Line: 2}
	nodes := append(out.Nodes,
		&graph.Node{ID: targetT, Name: "T", Kind: graph.KindType, FilePath: types, Language: "go"},
		&graph.Node{ID: targetU, Name: "U", Kind: graph.KindType, FilePath: types, Language: "go"},
		// An existing non-type target must not protect the sole mover.
		&graph.Node{ID: sole.To, Name: "U", Kind: graph.KindVariable, FilePath: file, Language: "go"},
		&graph.Node{ID: ambiguous.From, Name: "M", Kind: graph.KindMethod, FilePath: ambiguous.FilePath, Language: "go"},
		&graph.Node{ID: "amb/a.go::T", Name: "T", Kind: graph.KindType, FilePath: "amb/a.go", Language: "go"},
		&graph.Node{ID: "amb/b.go::T", Name: "T", Kind: graph.KindType, FilePath: "amb/b.go", Language: "go"},
		&graph.Node{ID: foreign.From, Name: "M", Kind: graph.KindMethod, FilePath: foreign.FilePath, Language: "typescript"},
	)
	arrival := []*graph.Edge{multi, plugin, sole, stationaryOld, &stationary, ambiguous, foreign}
	reversed := slices.Clone(arrival)
	slices.Reverse(reversed)
	ordered := slices.Clone(arrival)
	slices.SortFunc(ordered, compareRebuildImportEdges)

	const moved = 4 // both collision claims, the sole move, and the claim lost to a stationary row
	orderedRows, orderedCount := rebuildNativeReceiverReplay(t, nodes, ordered)
	require.Equal(t, moved, orderedCount)
	movedPlugin, movedSole := *plugin, *sole
	movedPlugin.To, movedSole.To = targetT, targetU
	want := []*graph.Edge{&movedPlugin, &movedSole, &stationary, ambiguous, foreign}
	slices.SortFunc(want, compareRebuildImportEdges)
	require.Equal(t, want, orderedRows)

	for _, tc := range []struct {
		name  string
		input []*graph.Edge
		first *graph.Edge
	}{
		{"parser_first", arrival, multi},
		{"plugin_first", reversed, plugin},
	} {
		t.Run(tc.name, func(t *testing.T) {
			// Raw SQLite follows rowid, so reversing arrivals must change the
			// winning payload without changing the saved keys or counts.
			raw, rawCount := rebuildNativeReceiverReplay(t, nodes, tc.input)
			require.Equal(t, moved, rawCount)
			winner := *tc.first
			winner.To = targetT
			rawWant := []*graph.Edge{&winner, &movedSole, &stationary, ambiguous, foreign}
			slices.SortFunc(rawWant, compareRebuildImportEdges)
			require.Equal(t, rawWant, raw)

			ctx, build, _ := frontierFixture(t, 1, 0)
			build.AddBatch(nodes, tc.input)
			before := slices.Clone(tc.input)
			slices.SortFunc(before, compareRebuildImportEdges)
			require.Equal(t, before, rebuildFnPointerSaved(t, build))
			gotCount, err := build.RebuildGoMethodReceivers(ctx)
			require.NoError(t, err)
			require.Equal(t, orderedCount, gotCount)
			require.Equal(t, orderedRows, rebuildFnPointerSaved(t, build))
			gotCount, err = build.RebuildGoMethodReceivers(ctx)
			require.NoError(t, err)
			require.Zero(t, gotCount)
			require.Equal(t, orderedRows, rebuildFnPointerSaved(t, build))
		})
	}
}

func rebuildNativeReceiverReplay(t *testing.T, nodes []*graph.Node, input []*graph.Edge) ([]*graph.Edge, int) {
	t.Helper()
	sql, err := store_sqlite.Open(filepath.Join(t.TempDir(), "native.sqlite"))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, sql.Close()) })
	require.NoError(t, sql.AddBatchChecked(nodes, nil))
	for _, e := range input {
		// Insert separately to make SQLite's rowid order explicit.
		require.NoError(t, sql.AddBatchChecked(nil, []*graph.Edge{e}))
	}
	count, err := sql.RebindGoMethodReceivers("")
	require.NoError(t, err)
	rows := sql.AllEdges()
	slices.SortFunc(rows, compareRebuildImportEdges)
	return rows, count
}

func TestRunRebuildMainGoMethodReceiversRawPending(t *testing.T) {
	for _, tc := range []struct {
		name     string
		deferred bool
	}{
		{"no_raw_pending", false},
		{"only_deferred_raw_pending", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			const file, types = "pkg/methods.go", "pkg/types.go"
			old := &graph.Edge{From: file + "::T.M", To: file + "::T", Kind: graph.EdgeMemberOf, FilePath: file, Line: 2}
			pending := &graph.Edge{From: "legacy/caller.py::run", To: "unresolved::Target", Kind: graph.EdgeCalls,
				FilePath: "legacy/caller.py", Line: 3, Meta: map[string]any{"payload": "deferred"}}
			build, err := store_pebble.OpenBuild(filepath.Join(t.TempDir(), "build"), store_pebble.FullProfile)
			require.NoError(t, err)
			t.Cleanup(func() { require.NoError(t, build.Close()) })
			build.AddBatch([]*graph.Node{
				{ID: old.From, Name: "M", Kind: graph.KindMethod, FilePath: file, Language: "go"},
				{ID: types + "::T", Name: "T", Kind: graph.KindType, FilePath: types, Language: "go"},
				{ID: pending.From, Name: "run", Kind: graph.KindFunction, FilePath: pending.FilePath, Language: "python"},
			}, []*graph.Edge{old})
			want := []*graph.Edge{old}
			wantSeen := 0
			if tc.deferred {
				wantSeen = 1
				build.AddEdge(pending)
				moved := *old
				moved.To = types + "::T"
				want = []*graph.Edge{pending, &moved}
			}
			lease := t.TempDir()
			const keepName = "gortex-resolve-guard-keep"
			keep := filepath.Join(lease, keepName)
			require.NoError(t, os.WriteFile(keep, []byte("unowned guard file"), 0600))
			// No Binary: even attempting to start a candidate worker fails.
			opts := store_pebble.RebuildLinkOptions{ScratchParent: lease, Partitions: 1, SpillBytes: 1}
			seen := 0
			require.NoError(t, RunRebuildMain(t.Context(), build, opts, RebuildMainInputs{
				SelectCallers: func(page []*graph.Edge) ([]*graph.Edge, error) {
					seen += len(page)
					require.Equal(t, []*graph.Edge{pending}, page)
					return nil, nil // the Python receipt is outside the admitted projection
				},
			}))
			require.Equal(t, wantSeen, seen)
			require.Equal(t, want, rebuildFnPointerSaved(t, build))
			entries, err := os.ReadDir(lease)
			require.NoError(t, err)
			require.Len(t, entries, 1)
			require.Equal(t, keepName, entries[0].Name())
			data, err := os.ReadFile(keep)
			require.NoError(t, err)
			require.Equal(t, "unowned guard file", string(data))
		})
	}
}
