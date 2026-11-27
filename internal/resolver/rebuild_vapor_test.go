package resolver

import (
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/graph/store_pebble"
)

func vaporRebuildSelection(t *testing.T, build *store_pebble.BuildStore, names ...string) FrameworkSynthesizerSelection {
	t.Helper()
	selection, err := NewFrameworkSynthesizerSelection(append(names, SynthVaporResolve))
	require.NoError(t, err)
	frozen, err := FreezeRebuildFrameworkSelection(t.Context(), build.RebuildView(), selection)
	require.NoError(t, err)
	return frozen
}

func TestRebuildVaporSQLiteOracle(t *testing.T) {
	const file = "App/routes.swift"
	source := &graph.Node{ID: "route", Name: "routes", Kind: graph.KindFunction, Language: "swift", FilePath: file}
	controller := &graph.Node{ID: "controller", Name: "UserController", Kind: graph.KindType, Language: "swift", FilePath: "App/Controllers/User.swift"}
	foreign := &graph.Node{ID: "foreign", Name: controller.Name, Kind: graph.KindType, Language: "go", FilePath: "Other/Controllers/User.go"}
	middleware := &graph.Node{ID: "middleware", Name: "AuthMiddleware", Kind: graph.KindType, Language: "swift", FilePath: "App/Middleware/Auth.swift"}
	model := &graph.Node{ID: "model", Name: "LegacyViewController", Kind: graph.KindType, Language: "swift", FilePath: "App/Models/Legacy.swift"}
	rejected := &graph.Edge{From: source.ID, To: "unresolved::Token", Kind: graph.EdgeReferences, FilePath: file, Line: 5}
	token := &graph.Node{ID: "token", Name: "Token", Kind: graph.KindType, Language: "swift", FilePath: file}
	other := &graph.Node{ID: "other", Name: "routeNative", Kind: graph.KindFunction, Language: "objc", FilePath: "App/Route.m"}
	otherRef := &graph.Edge{From: other.ID, To: "unresolved::AuthMiddleware", Kind: graph.EdgeReferences, FilePath: other.FilePath, Line: 1}
	rows := []*graph.Edge{
		{From: source.ID, To: "unresolved::AuthMiddleware", Kind: graph.EdgeCalls, FilePath: file, Line: 1, Meta: map[string]any{"kept": true}},
		{From: source.ID, To: "unresolved::LegacyViewController", Kind: graph.EdgeTypedAs, FilePath: file, Line: 2},
		{From: source.ID, To: "unresolved::AuthMiddleware", Kind: graph.EdgeReferences, FilePath: file, Line: 3},
		{From: source.ID, To: "unresolved::UserController", Kind: graph.EdgeInstantiates, FilePath: file, Line: 4},
		rejected,
	}
	for _, extra := range []*graph.Node{nil, foreign} {
		facts := []store_pebble.ParseFacts{
			rebuildEventFacts(file, "swift", []*graph.Node{source, token}, rows...),
			rebuildEventFacts(controller.FilePath, "swift", []*graph.Node{controller}),
			rebuildEventFacts(middleware.FilePath, "swift", []*graph.Node{middleware}),
			rebuildEventFacts(model.FilePath, "swift", []*graph.Node{model}),
			rebuildEventFacts(other.FilePath, "objc", []*graph.Node{other}, otherRef),
		}
		want := 4
		if extra != nil {
			facts = append(facts, rebuildEventFacts(extra.FilePath, extra.Language, []*graph.Node{extra}))
			want--
		}
		build, native := openRebuildEventStores(t, facts...)
		selection := vaporRebuildSelection(t, build)
		require.True(t, selection.allows(SynthVaporResolve))
		for _, count := range []int{want, 0} {
			moves, err := RunRebuildVapor(t.Context(), build, rebuildGRPCReceipts(facts...), selection)
			require.NoError(t, err)
			require.EqualValues(t, count, moves)
			require.Equal(t, count, ResolveVaporRefs(native))
			saved := rebuildEventSavedRows(t, build).edges
			require.Equal(t, rebuildEventSQLiteRows(native).edges, saved)
			require.Contains(t, saved, rejected)
			require.Contains(t, saved, otherRef)
		}
	}
}

func TestRebuildVaporReadsAfterAppleTurns(t *testing.T) {
	for _, tc := range []struct {
		stage, name, parentDir string
	}{
		{SynthSwiftUIResolve, "CartStore", "Stores"},
		{SynthUIKitResolve, "HomeViewController", "Controllers"},
	} {
		t.Run(tc.stage, func(t *testing.T) {
			const file = "App/routes.swift"
			source := &graph.Node{ID: "route", Name: "routes", Kind: graph.KindFunction, Language: "swift", FilePath: file}
			winner := &graph.Node{ID: "winner", Name: tc.name, Kind: graph.KindType, Language: "swift", FilePath: "App/" + tc.parentDir + "/Winner.swift"}
			model := &graph.Node{ID: "model", Name: tc.name, Kind: graph.KindType, Language: "swift", FilePath: "App/Models/Model.swift"}
			controller := &graph.Node{ID: "server", Name: "UserController", Kind: graph.KindType, Language: "swift", FilePath: file}
			shared := &graph.Edge{From: source.ID, To: "unresolved::" + tc.name, Kind: graph.EdgeReferences, FilePath: file, Line: 2}
			server := &graph.Edge{From: source.ID, To: "unresolved::UserController", Kind: graph.EdgeInstantiates, FilePath: file, Line: 3}
			facts := []store_pebble.ParseFacts{
				rebuildEventFacts(file, "swift", []*graph.Node{source, controller}, shared, server),
				rebuildEventFacts(winner.FilePath, "swift", []*graph.Node{winner}),
				rebuildEventFacts(model.FilePath, "swift", []*graph.Node{model}),
			}
			build, native := openRebuildEventStores(t, facts...)
			selection := vaporRebuildSelection(t, build, tc.stage)
			receipts := rebuildGRPCReceipts(facts...)
			require.True(t, selection.allows(SynthVaporResolve))
			if tc.stage == SynthSwiftUIResolve {
				moves, err := RunRebuildSwiftUI(t.Context(), build, receipts, selection)
				require.NoError(t, err)
				require.EqualValues(t, 1, moves)
				require.Equal(t, 1, ResolveSwiftUIRefs(native))
			} else {
				// Reproduce UIKit's exact saved move without depending on its private reducer.
				live := *shared
				require.NoError(t, build.ApplyLinkReindexes(t.Context(), []graph.EdgeReindex{uikitRefRebind(&live, winner.ID, 0.9)}))
				require.Equal(t, 1, ResolveUIKitRefs(native))
			}
			moves, err := RunRebuildVapor(t.Context(), build, receipts, selection)
			require.NoError(t, err)
			require.EqualValues(t, 1, moves)
			require.Equal(t, 1, ResolveVaporRefs(native))
			require.Equal(t, rebuildEventSQLiteRows(native).edges, rebuildEventSavedRows(t, build).edges)
		})
	}
}

func TestRebuildVaporExactProof(t *testing.T) {
	const file = "App/routes.swift"
	source := &graph.Node{ID: "route", Name: "routes", Kind: graph.KindFunction, Language: "swift", FilePath: file}
	target := &graph.Node{ID: "controller", Name: "UserController", Kind: graph.KindType, Language: "swift", FilePath: file}
	ref := &graph.Edge{From: source.ID, To: "unresolved::UserController", Kind: graph.EdgeTypedAs, FilePath: file, Line: 3}
	facts := rebuildEventFacts(file, "swift", []*graph.Node{source, target}, ref)
	build := openRebuildGRPCBuild(t, facts)
	before := rebuildEventSavedRows(t, build)
	incomplete := rebuildEventFacts(file, "swift", []*graph.Node{source}, ref)
	count, err := RunRebuildVapor(t.Context(), build, rebuildGRPCReceipts(incomplete), vaporRebuildSelection(t, build))
	require.Zero(t, count)
	require.ErrorIs(t, err, store_pebble.ErrRebuildVapor)
	var refused *store_pebble.RebuildUnknownProvenanceError
	require.ErrorAs(t, err, &refused)
	require.Equal(t, graph.EdgeIdentityFor(ref), refused.First)
	require.EqualValues(t, 1, refused.Count)
	require.Equal(t, before, rebuildEventSavedRows(t, build))
}

func TestRebuildVaporFrozenNodeGate(t *testing.T) {
	const file = "App/routes.swift"
	build := openRebuildGRPCBuild(t, rebuildEventFacts(file, "swift", []*graph.Node{
		{ID: "route", Name: "routes", Kind: graph.KindFunction, Language: "swift", FilePath: file},
	}))
	frozen := vaporRebuildSelection(t, build)
	require.False(t, frozen.allows(SynthVaporResolve))
	build.AddNode(&graph.Node{ID: "controller", Name: "UserController", Kind: graph.KindType, Language: "swift", FilePath: file})
	require.True(t, vaporRebuildSelection(t, build).allows(SynthVaporResolve))
	count, err := RunRebuildVapor(t.Context(), build, nil, frozen)
	require.NoError(t, err)
	require.Zero(t, count)
}

func TestRebuildVaporRejectsLowerCaseReference(t *testing.T) {
	const file = "App/routes.swift"
	source := &graph.Node{ID: "route", Kind: graph.KindFunction, FilePath: file, Language: "swift"}
	target := &graph.Node{ID: "configure", Name: "configure", Kind: graph.KindFunction, FilePath: file, Language: "swift"}
	edge := &graph.Edge{From: source.ID, To: "unresolved::configure", Kind: graph.EdgeCalls, FilePath: file, Line: 1}
	facts := rebuildEventFacts(file, "swift", []*graph.Node{source, target}, edge)
	b, native := openRebuildEventStores(t, facts)
	before := rebuildEventSavedRows(t, b)
	selection, err := NewFrameworkSynthesizerSelection([]string{SynthVaporResolve})
	require.NoError(t, err)
	moves, err := RunRebuildVapor(t.Context(), b, rebuildGRPCReceipts(facts), selection)
	require.NoError(t, err)
	require.Zero(t, moves)
	require.Zero(t, ResolveVaporRefs(native))
	require.Equal(t, before, rebuildEventSavedRows(t, b))
}
