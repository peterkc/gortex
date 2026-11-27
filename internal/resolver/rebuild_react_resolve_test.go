package resolver

import (
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/graph/store_pebble"
)

func freezeReactResolveTest(t *testing.T, build *store_pebble.BuildStore, names ...string) FrameworkSynthesizerSelection {
	t.Helper()
	selected, err := NewFrameworkSynthesizerSelection(append(names, SynthReactResolve))
	require.NoError(t, err)
	frozen, err := FreezeRebuildFrameworkSelection(t.Context(), build.RebuildView(), selected)
	require.NoError(t, err)
	require.True(t, frozen.allows(SynthReactResolve))
	require.NoError(t, FreezeRebuildReactResolveInputs(t.Context(), build, frozen))
	return frozen
}

func TestRebuildReactResolveSQLiteOracle(t *testing.T) {
	const file = "web/view.tsx"
	source := &graph.Node{ID: "view", FilePath: file, Language: "typescript", Kind: graph.KindFunction}
	hook := &graph.Node{ID: "hook", Name: "useSession", FilePath: "web/hooks/session.ts", Language: "typescript", Kind: graph.KindFunction}
	contextNode := &graph.Node{ID: "context", Name: "Auth", FilePath: "web/providers/auth.ts", Language: "typescript", Kind: graph.KindVariable}
	foreign := &graph.Node{ID: "foreign", Name: "useSession", FilePath: "foreign/hooks/session.go", Language: "go", Kind: graph.KindFunction}
	edge := func(kind graph.EdgeKind, target string, line int, meta map[string]any) *graph.Edge {
		return &graph.Edge{From: source.ID, To: "unresolved::" + target, Kind: kind, FilePath: file, Line: line, Meta: meta}
	}
	call := edge(graph.EdgeCalls, hook.Name, 2, map[string]any{"kept": true})
	ref := edge(graph.EdgeReferences, "AuthContext", 3, map[string]any{"via": "react_context"})
	render := edge(graph.EdgeRendersChild, "AuthProvider.Provider", 4, nil)
	for _, extra := range []*graph.Node{nil, foreign} {
		facts := []store_pebble.ParseFacts{
			rebuildEventFacts(file, "typescript", []*graph.Node{source}, ref, call),
			rebuildEventFacts(hook.FilePath, hook.Language, []*graph.Node{hook}),
			rebuildEventFacts(contextNode.FilePath, contextNode.Language, []*graph.Node{contextNode}),
		}
		if extra != nil {
			facts = append(facts, rebuildEventFacts(extra.FilePath, extra.Language, []*graph.Node{extra}))
		}
		build, native := openRebuildEventStores(t, facts...)
		frozen := freezeReactResolveTest(t, build)
		// Only Calls and References are frozen. A RendersChild received
		// before the live turn still participates and uses its own receipt.
		build.AddEdge(render)
		native.AddEdge(render)
		facts[0].Edges = append(facts[0].Edges, render)
		want := 3
		if extra != nil {
			want = 2
		}
		for _, moves := range []int{want, 0} {
			changed, err := RunRebuildReactResolve(t.Context(), build, rebuildGRPCReceipts(facts...), frozen)
			require.NoError(t, err)
			require.EqualValues(t, moves, changed)
			require.Equal(t, moves, ResolveReactHooksContext(native))
			saved := rebuildEventSavedRows(t, build).edges
			require.Equal(t, rebuildEventSQLiteRows(native).edges, saved)
			if extra != nil {
				require.Contains(t, saved, call, "a foreign homonym can make the hook ambiguous")
			}
			if moves != 0 {
				frozen = freezeReactResolveTest(t, build)
			}
		}
	}
}

func TestRebuildReactResolveExpressPrecedence(t *testing.T) {
	const file, name = "web/routes.ts", "useAuth"
	source := &graph.Node{ID: "route", FilePath: file, Language: "typescript", Kind: graph.KindFunction}
	middleware := &graph.Node{ID: "mw", Name: name, FilePath: "web/middleware/auth.ts", Language: "typescript", Kind: graph.KindFunction}
	hook := &graph.Node{ID: "hook", Name: name, FilePath: "web/hooks/auth.ts", Language: "typescript", Kind: graph.KindFunction}
	call := &graph.Edge{From: source.ID, To: "unresolved::" + name, Kind: graph.EdgeCalls, FilePath: file, Line: 2,
		Meta: map[string]any{"express_handler_ref": true, "express_ref_name": name}}
	facts := []store_pebble.ParseFacts{
		rebuildEventFacts(file, "typescript", []*graph.Node{source}, call),
		rebuildEventFacts(middleware.FilePath, middleware.Language, []*graph.Node{middleware}),
		rebuildEventFacts(hook.FilePath, hook.Language, []*graph.Node{hook}),
	}
	build, native := openRebuildEventStores(t, facts...)
	receipts := rebuildGRPCReceipts(facts...)
	selection := freezeReactResolveTest(t, build, SynthExpressResolve)
	require.True(t, selection.allows(SynthExpressResolve))
	express, err := RunRebuildExpress(t.Context(), build, receipts, selection)
	require.NoError(t, err)
	require.EqualValues(t, 1, express.Moves)
	require.Equal(t, 1, ResolveExpressHandlers(native))
	require.Equal(t, 0, ResolveReactHooksContext(native))
	before := rebuildEventSavedRows(t, build)
	require.Equal(t, rebuildEventSQLiteRows(native).edges, before.edges)
	require.Equal(t, middleware.ID, before.edges[0].To)
	late := *call
	late.Line++
	build.AddEdge(&late)
	before = rebuildEventSavedRows(t, build)
	changed, err := RunRebuildReactResolve(t.Context(), build, receipts, selection)
	require.NoError(t, err)
	require.Zero(t, changed)
	require.Equal(t, before, rebuildEventSavedRows(t, build))
}

func TestRebuildReactResolveExactProof(t *testing.T) {
	const file = "web/view.tsx"
	source := &graph.Node{ID: "view", FilePath: file, Language: "typescript", Kind: graph.KindFunction}
	provider := &graph.Node{ID: "provider", Name: "Session", FilePath: file, Language: "typescript", Kind: graph.KindVariable}
	ref := &graph.Edge{From: source.ID, To: "unresolved::SessionContext.Provider", Kind: graph.EdgeRendersChild, FilePath: file, Line: 3}
	facts := rebuildEventFacts(file, "typescript", []*graph.Node{source, provider}, ref)
	build := openRebuildGRPCBuild(t, facts)
	before := rebuildEventSavedRows(t, build)
	incomplete := rebuildEventFacts(file, "typescript", []*graph.Node{source}, ref)
	changed, err := RunRebuildReactResolve(t.Context(), build, rebuildGRPCReceipts(incomplete), freezeReactResolveTest(t, build))
	require.Zero(t, changed)
	require.ErrorIs(t, err, store_pebble.ErrRebuildReactResolve)
	var refused *store_pebble.RebuildUnknownProvenanceError
	require.ErrorAs(t, err, &refused)
	require.EqualValues(t, 1, refused.Count)
	require.Equal(t, graph.EdgeIdentityFor(ref), refused.First)
	require.Equal(t, before, rebuildEventSavedRows(t, build))
}
