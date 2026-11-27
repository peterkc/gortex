package resolver

import (
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/graph/store_pebble"
)

func freezeFastAPITest(t *testing.T, build *store_pebble.BuildStore) FrameworkSynthesizerSelection {
	t.Helper()
	selection, err := NewFrameworkSynthesizerSelection([]string{SynthFastAPIResolve})
	require.NoError(t, err)
	selection, err = FreezeRebuildFrameworkSelection(t.Context(), build.RebuildView(), selection)
	require.NoError(t, err)
	require.True(t, selection.allows(SynthFastAPIResolve))
	require.NoError(t, FreezeRebuildFastAPIInputs(t.Context(), build, selection))
	return selection
}

func TestRebuildFastAPISQLiteOracle(t *testing.T) {
	const file = "web/api.py"
	source := &graph.Node{ID: "caller", Kind: graph.KindFunction, FilePath: file, Language: "python"}
	candidate := func(id, name, path, lang string) *graph.Node {
		return &graph.Node{ID: id, Kind: graph.KindVariable, Name: name, FilePath: path, Language: lang}
	}
	dep := candidate("foreign-dep", "get_data", "app/dependencies/get.ts", "typescript")
	local := candidate("local-dep", dep.Name, file, "python")
	router := candidate("router", "api_router", "app/routes/api.py", "python")
	ambiguous := candidate("foreign-router", router.Name, "foreign/routes/api.go", "go")
	edge := func(kind graph.EdgeKind, to, via string, line int) *graph.Edge {
		return &graph.Edge{From: source.ID, Kind: kind, To: to, FilePath: file, Line: line, Meta: map[string]any{"via": via, "site": "kept"}}
	}
	call := edge(graph.EdgeCalls, "unresolved::"+dep.Name, "fastapi.Depends", 2)
	ref := edge(graph.EdgeReferences, "unresolved::"+router.Name, "fastapi.router", 3)
	settled := edge(graph.EdgeCalls, local.ID, "fastapi.Depends", 4)
	member := edge(graph.EdgeReferences, "unresolved::module.api_router", "fastapi.router", 5)
	for _, extra := range []*graph.Node{nil, ambiguous} {
		facts := []store_pebble.ParseFacts{
			rebuildEventFacts(file, "python", []*graph.Node{source, local}, ref, call, settled, member),
			rebuildEventFacts(dep.FilePath, dep.Language, []*graph.Node{dep}),
			rebuildEventFacts(router.FilePath, router.Language, []*graph.Node{router}),
		}
		if extra != nil {
			facts = append(facts, rebuildEventFacts(extra.FilePath, extra.Language, []*graph.Node{extra}))
		}
		build, native := openRebuildEventStores(t, facts...)
		want := 2
		if extra != nil {
			want = 1
		}
		for _, moves := range []int{want, 0} {
			selection := freezeFastAPITest(t, build)
			changed, err := RunRebuildFastAPI(t.Context(), build, rebuildGRPCReceipts(facts...), selection)
			require.NoError(t, err)
			require.EqualValues(t, moves, changed)
			require.Equal(t, moves, ResolveFastAPIDeps(native))
			saved := rebuildEventSavedRows(t, build).edges
			require.Equal(t, rebuildEventSQLiteRows(native).edges, saved)
			require.Contains(t, saved, settled)
			require.Contains(t, saved, member)
			bound := *call
			bound.Meta = map[string]any{"via": "fastapi.Depends", "site": "kept"}
			fastapiResolveRebind(&bound, dep.ID, 0.9)
			require.Contains(t, saved, &bound)
			if extra != nil {
				require.Contains(t, saved, ref, "the Go homonym suppresses the Python router move")
			}
		}
	}
}

func TestRebuildFastAPIExactProof(t *testing.T) {
	const path = "api.py"
	caller := &graph.Node{ID: "caller", Kind: graph.KindFunction, FilePath: path, Language: "python"}
	dep := &graph.Node{ID: "dep", Kind: graph.KindFunction, Name: "get_db", FilePath: path, Language: "python"}
	call := &graph.Edge{From: caller.ID, To: "unresolved::get_db", Kind: graph.EdgeCalls, FilePath: path, Line: 4,
		Meta: map[string]any{"via": "fastapi.Depends"}}
	facts := rebuildEventFacts(path, "python", []*graph.Node{caller, dep}, call)
	for _, incomplete := range []store_pebble.ParseFacts{
		rebuildEventFacts(path, "python", []*graph.Node{caller}, call),
		rebuildEventFacts(path, "python", []*graph.Node{caller, dep}),
	} {
		build := openRebuildGRPCBuild(t, facts)
		before := rebuildEventSavedRows(t, build)
		changed, err := RunRebuildFastAPI(t.Context(), build, rebuildGRPCReceipts(incomplete), freezeFastAPITest(t, build))
		require.Zero(t, changed)
		require.ErrorIs(t, err, store_pebble.ErrRebuildFastAPI)
		var refused *store_pebble.RebuildUnknownProvenanceError
		require.ErrorAs(t, err, &refused)
		require.EqualValues(t, 1, refused.Count)
		require.Equal(t, graph.EdgeIdentityFor(call), refused.First)
		require.Equal(t, before, rebuildEventSavedRows(t, build))
	}
}

func TestRebuildFastAPIFrozenInputIdentity(t *testing.T) {
	const path = "api.py"
	caller := &graph.Node{ID: "caller", Kind: graph.KindFunction, FilePath: path, Language: "python"}
	dep := &graph.Node{ID: "dep", Kind: graph.KindFunction, Name: "get_db", FilePath: path, Language: "python"}
	call := &graph.Edge{From: caller.ID, To: "unresolved::get_db", Kind: graph.EdgeCalls, FilePath: path, Line: 4,
		Meta: map[string]any{"via": "fastapi.Depends"}}
	facts := rebuildEventFacts(path, "python", []*graph.Node{caller, dep}, call)
	build := openRebuildGRPCBuild(t, facts)
	frozen := freezeFastAPITest(t, build)
	settled := *call
	settled.To = dep.ID
	require.NoError(t, build.ApplyLinkReindexes(t.Context(), []graph.EdgeReindex{{Edge: &settled, OldTo: call.To}}))
	late := *call
	late.Line++
	build.AddEdge(&late)
	before := rebuildEventSavedRows(t, build)
	changed, err := RunRebuildFastAPI(t.Context(), build, rebuildGRPCReceipts(facts), frozen)
	require.NoError(t, err)
	require.Zero(t, changed)
	require.Equal(t, before, rebuildEventSavedRows(t, build))
}
