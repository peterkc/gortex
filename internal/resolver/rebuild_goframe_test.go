package resolver

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/graph/store_pebble"
)

func TestRebuildGoFrameRoutes(t *testing.T) {
	const file, req, pkg = "api/routes.go", "CreateReq", "example/api"
	route := &graph.Node{ID: "route::goframe::POST::/new", Kind: graph.KindContract, Language: "go", FilePath: file, WorkspaceID: "ws"}
	alias := &graph.Node{ID: "route::goframe::POST::/alias", Kind: graph.KindContract, Language: "go", FilePath: file, WorkspaceID: "ws"}
	handler := func(id, dir, pkg, workspace string, bound bool) *graph.Node {
		return &graph.Node{ID: id, Name: id, Kind: graph.KindMethod, Language: "go", FilePath: dir + "/" + id + ".go", WorkspaceID: workspace,
			Meta: map[string]any{"goframe_request_type": req, "goframe_request_pkg": pkg, "goframe_bound": bound}}
	}
	winner := handler("winner", "api", pkg, "ws", true)
	far := handler("far", "other", pkg, "ws", true)
	lower := handler("unbound", "api", pkg, "ws", false)
	otherPkg := handler("otherPkg", "api", "foreign", "ws", true)
	otherWorkspace := handler("otherWorkspace", "api", pkg, "foreign", true)
	call := &graph.Edge{From: route.ID, To: "unresolved::*." + req, Kind: graph.EdgeCalls, FilePath: file, Line: 3,
		Meta: map[string]any{"via": goframeRouteVia, "goframe_request_type": req, "goframe_request_pkg": pkg, "goframe_route": alias.ID}}
	callFacts := rebuildEventFacts(file, "go", []*graph.Node{route, alias}, call)
	factsFor := func(n *graph.Node) store_pebble.ParseFacts {
		return rebuildEventFacts(n.FilePath, "go", []*graph.Node{n})
	}
	freeze := func(t *testing.T, b *store_pebble.BuildStore) FrameworkSynthesizerSelection {
		t.Helper()
		selected, err := NewFrameworkSynthesizerSelection([]string{SynthGoFrameRoute})
		require.NoError(t, err)
		selection, err := FreezeRebuildFrameworkSelection(t.Context(), b.RebuildView(), selected)
		require.NoError(t, err)
		return selection
	}
	checkRefusal := func(t *testing.T, b *store_pebble.BuildStore, key *graph.Edge, receipts ...store_pebble.ParseFacts) {
		t.Helper()
		before := rebuildEventSavedRows(t, b)
		moves, err := RunRebuildGoFrame(t.Context(), b, rebuildGRPCReceipts(receipts...), freeze(t, b))
		require.ErrorIs(t, err, store_pebble.ErrRebuildGoFrame)
		var refused *store_pebble.RebuildUnknownProvenanceError
		require.ErrorAs(t, err, &refused)
		require.Equal(t, graph.EdgeIdentityFor(key), refused.First)
		require.Zero(t, moves)
		require.Equal(t, before, rebuildEventSavedRows(t, b))
	}

	t.Run("sqlite bind restub and occupied call", func(t *testing.T) {
		stable, _ := goframeRouteRebind(cloneFrameworkEdge(call), req, winner)
		stable.Edge.Line = 4
		stale, _ := goframeRouteRebind(cloneFrameworkEdge(call), req, far)
		stale.Edge.Line = 5
		stale.Edge.Meta["goframe_request_type"] = "MissingReq"
		incumbent := &graph.Edge{From: call.From, To: winner.ID, Kind: graph.EdgeCalls, FilePath: file, Line: call.Line,
			Meta: map[string]any{"incumbent": true}}
		routeFacts := rebuildEventFacts(file, "go", callFacts.Nodes, call, stable.Edge, stale.Edge, incumbent)
		// The collider is stationary and must survive the Call move. Native
		// still offers the mirror from the positive move at the same site.
		facts := []store_pebble.ParseFacts{routeFacts, factsFor(winner), factsFor(far), factsFor(lower), factsFor(otherPkg), factsFor(otherWorkspace)}
		build, native := openRebuildEventStores(t, facts...)
		selection := freeze(t, build)
		require.True(t, selection.allows(SynthGoFrameRoute))
		require.Equal(t, 2, ResolveGoFrameRoutes(native)) // Includes the already-bound Call.
		moves, err := RunRebuildGoFrame(t.Context(), build, rebuildGRPCReceipts(facts...), selection)
		require.NoError(t, err)
		require.EqualValues(t, 2, moves) // Counts only emitted moves, including restubs.
		want := rebuildEventSQLiteRows(native)
		require.Equal(t, want, rebuildEventSavedRows(t, build))
		require.Contains(t, want.edges, incumbent)
		var mirrors []*graph.Edge
		for _, e := range want.edges {
			if e.Kind == graph.EdgeHandlesRoute {
				mirrors = append(mirrors, e)
			}
		}
		require.Len(t, mirrors, 1)
		require.Equal(t, alias.ID, mirrors[0].To)
		require.Equal(t, call.Line, mirrors[0].Line)
	})

	t.Run("receipt-only and unproved handlers", func(t *testing.T) {
		competitor := handler("otherWinner", "api", pkg, "ws", true)
		inert := *competitor
		inert.Meta = nil
		base := []store_pebble.ParseFacts{callFacts, factsFor(winner)}
		build := openRebuildGRPCBuild(t, append(base, factsFor(&inert))...)
		checkRefusal(t, build, call, append(base, factsFor(competitor))...)
		build = openRebuildGRPCBuild(t, append(base, factsFor(competitor))...)
		checkRefusal(t, build, call, base...)
	})

	t.Run("two live rows claim one raw Call", func(t *testing.T) {
		receipts := []store_pebble.ParseFacts{callFacts, factsFor(winner)}
		build := openRebuildGRPCBuild(t, receipts...)
		moved := cloneFrameworkEdge(call)
		moved.To = "other-live-binding"
		require.NoError(t, build.ApplyLinkReindexes(t.Context(), []graph.EdgeReindex{{Edge: moved, OldTo: call.To}}))
		untagged := *call
		untagged.Meta = nil
		build.AddEdge(&untagged) // This stationary occupant also owns the raw key.
		checkRefusal(t, build, moved, receipts...)
	})

	t.Run("unrelated oversized Call", func(t *testing.T) {
		oversized := &graph.Edge{From: route.ID, To: "unresolved::unrelated", Kind: graph.EdgeCalls, FilePath: file, Line: 8,
			Meta: map[string]any{"unrelated": strings.Repeat("x", 16<<20)}}
		routeFacts := rebuildEventFacts(file, "go", callFacts.Nodes, call, oversized)
		build := openRebuildGRPCBuild(t, routeFacts, factsFor(winner), factsFor(lower))
		moves, err := RunRebuildGoFrame(t.Context(), build, rebuildGRPCReceipts(routeFacts, factsFor(winner)), freeze(t, build))
		require.NoError(t, err, "an unrelated Call and an optional lower-ranked candidate cannot change the outcome")
		require.EqualValues(t, 1, moves)
	})

	t.Run("oversized handler after census", func(t *testing.T) {
		receipts := []store_pebble.ParseFacts{callFacts, factsFor(winner)}
		build := openRebuildGRPCBuild(t, receipts...)
		selection := freeze(t, build)
		require.True(t, selection.allows(SynthGoFrameRoute))
		large := *winner
		large.Meta = map[string]any{
			"goframe_request_type": req, "goframe_request_pkg": pkg, "goframe_bound": true,
			"unrelated": strings.Repeat("x", 16<<20),
		}
		// Enlarge the live handler after the census so this exercises the stage's cap.
		build.AddNode(&large)
		moves, err := RunRebuildGoFrame(t.Context(), build, rebuildGRPCReceipts(receipts...), selection)
		require.ErrorIs(t, err, store_pebble.ErrRebuildGoFrame)
		require.ErrorIs(t, err, store_pebble.ErrRebuildLinkInputSize)
		require.ErrorContains(t, err, "goframe handler node row")
		require.Zero(t, moves)
		require.Equal(t, []*graph.Edge{call}, rebuildEventSavedRows(t, build).edges)
	})

	t.Run("frozen selection", func(t *testing.T) {
		build := openRebuildGRPCBuild(t, factsFor(winner))
		selection := freeze(t, build)
		require.False(t, selection.allows(SynthGoFrameRoute))
		require.NoError(t, build.AddParsedFile("", callFacts))
		moves, err := RunRebuildGoFrame(t.Context(), build, nil, selection)
		require.NoError(t, err)
		require.Zero(t, moves)
	})
}
