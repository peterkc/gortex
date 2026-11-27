package resolver

import (
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/graph/store_pebble"
)

func swiftUIRebuildSelection(t *testing.T, build *store_pebble.BuildStore) FrameworkSynthesizerSelection {
	t.Helper()
	selection, err := NewFrameworkSynthesizerSelection([]string{SynthSwiftUIResolve})
	require.NoError(t, err)
	frozen, err := FreezeRebuildFrameworkSelection(t.Context(), build.RebuildView(), selection)
	require.NoError(t, err)
	return frozen
}

func TestRebuildSwiftUISQLiteOracle(t *testing.T) {
	const file = "App/Panel.swift"
	source := &graph.Node{ID: "panel", Name: "PanelView", Kind: graph.KindType, Language: "swift", FilePath: file}
	view := &graph.Node{ID: "view", Name: "ProfileView", Kind: graph.KindType, Language: "swift", FilePath: "App/Views/Profile.swift"}
	foreign := &graph.Node{ID: "foreign", Name: view.Name, Kind: graph.KindType, Language: "go", FilePath: "Other/Views/Profile.go"}
	button := &graph.Node{ID: "button", Name: "Button", Kind: graph.KindType, Language: "swift", FilePath: file}
	model := &graph.Node{ID: "model", Name: "Profile", Kind: graph.KindType, Language: "swift", FilePath: "App/Models/Profile.swift"}
	edge := func(kind graph.EdgeKind, target string, line int) *graph.Edge {
		return &graph.Edge{From: source.ID, To: target, Kind: kind, FilePath: file, Line: line, Meta: map[string]any{"kept": true}}
	}
	var rows []*graph.Edge
	for i, kind := range [...]graph.EdgeKind{graph.EdgeCalls, graph.EdgeTypedAs, graph.EdgeReferences, graph.EdgeInstantiates} {
		rows = append(rows, edge(kind, "unresolved::"+view.Name, i+1))
	}
	rejected := edge(graph.EdgeReferences, "unresolved::Button", 5)
	already := edge(graph.EdgeReferences, button.ID, 6)
	rows = append(rows, rejected, already, edge(graph.EdgeTypedAs, "unresolved::"+model.Name, 7))
	for _, extra := range []*graph.Node{nil, foreign} {
		facts := []store_pebble.ParseFacts{
			rebuildEventFacts(file, "swift", []*graph.Node{source, button}, rows...),
			rebuildEventFacts(view.FilePath, "swift", []*graph.Node{view}),
			rebuildEventFacts(model.FilePath, "swift", []*graph.Node{model}),
		}
		if extra != nil {
			facts = append(facts, rebuildEventFacts(extra.FilePath, extra.Language, []*graph.Node{extra}))
		}
		build, native := openRebuildEventStores(t, facts...)
		selection := swiftUIRebuildSelection(t, build)
		require.True(t, selection.allows(SynthSwiftUIResolve))
		want := 5
		if extra != nil {
			want = 1
		}
		for _, count := range []int{want, 0} {
			moves, err := RunRebuildSwiftUI(t.Context(), build, rebuildGRPCReceipts(facts...), selection)
			require.NoError(t, err)
			require.EqualValues(t, count, moves)
			require.Equal(t, count, ResolveSwiftUIRefs(native))
			saved := rebuildEventSavedRows(t, build).edges
			require.Equal(t, rebuildEventSQLiteRows(native).edges, saved)
			require.Contains(t, saved, rejected)
			require.Contains(t, saved, already)
			if extra != nil {
				require.Contains(t, saved, rows[0], "the foreign homonym suppresses the Swift move")
			}
		}
	}
}

func TestRebuildSwiftUIExactProof(t *testing.T) {
	const file = "App/Panel.swift"
	source := &graph.Node{ID: "panel", Name: "PanelView", Kind: graph.KindType, Language: "swift", FilePath: file}
	target := &graph.Node{ID: "view", Name: "DetailView", Kind: graph.KindType, Language: "swift", FilePath: file}
	ref := &graph.Edge{From: source.ID, To: "unresolved::DetailView", Kind: graph.EdgeTypedAs, FilePath: file, Line: 3}
	facts := rebuildEventFacts(file, "swift", []*graph.Node{source, target}, ref)
	build := openRebuildGRPCBuild(t, facts)
	before := rebuildEventSavedRows(t, build)
	incomplete := rebuildEventFacts(file, "swift", []*graph.Node{source}, ref)
	count, err := RunRebuildSwiftUI(t.Context(), build, rebuildGRPCReceipts(incomplete), swiftUIRebuildSelection(t, build))
	require.Zero(t, count)
	require.ErrorIs(t, err, store_pebble.ErrRebuildSwiftUI)
	var refused *store_pebble.RebuildUnknownProvenanceError
	require.ErrorAs(t, err, &refused)
	require.Equal(t, graph.EdgeIdentityFor(ref), refused.First)
	require.EqualValues(t, 1, refused.Count)
	require.Equal(t, before, rebuildEventSavedRows(t, build))
}

func TestRebuildSwiftUIFrozenNodeGate(t *testing.T) {
	const file = "App/Panel.swift"
	build := openRebuildGRPCBuild(t, rebuildEventFacts(file, "swift", []*graph.Node{
		{ID: "panel", Name: "Panel", Kind: graph.KindType, Language: "swift", FilePath: file},
	}))
	frozen := swiftUIRebuildSelection(t, build)
	require.False(t, frozen.allows(SynthSwiftUIResolve))
	build.AddNode(&graph.Node{ID: "view", Name: "DetailView", Kind: graph.KindType, Language: "swift", FilePath: file})
	require.True(t, swiftUIRebuildSelection(t, build).allows(SynthSwiftUIResolve))
	count, err := RunRebuildSwiftUI(t.Context(), build, nil, frozen)
	require.NoError(t, err)
	require.Zero(t, count)
}
