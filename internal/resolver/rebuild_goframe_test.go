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

	t.Run("oversized handler", func(t *testing.T) {
		receipts := []store_pebble.ParseFacts{callFacts, factsFor(winner)}
		build := openRebuildGRPCBuild(t, receipts...)
		large := *winner
		large.Meta = map[string]any{
			"goframe_request_type": req, "goframe_request_pkg": pkg, "goframe_bound": true,
			"unrelated": strings.Repeat("x", 16<<20),
		}
		build.AddNode(&large)
		selection := freeze(t, build)
		require.True(t, selection.allows(SynthGoFrameRoute))
		moves, err := RunRebuildGoFrame(t.Context(), build, rebuildGRPCReceipts(receipts...), selection)
		require.ErrorIs(t, err, store_pebble.ErrRebuildGoFrame)
		require.ErrorIs(t, err, store_pebble.ErrRebuildLinkInputSize)
		require.ErrorContains(t, err, "goframe handler node row")
		require.Zero(t, moves)
		require.Equal(t, []*graph.Edge{call}, rebuildEventSavedRows(t, build).edges)
	})

	t.Run("sqlite bound directory tie", func(t *testing.T) {
		tie := handler("tie", "api", pkg, "ws", true)
		stale := cloneFrameworkEdge(call)
		stale.To = far.ID
		facts := []store_pebble.ParseFacts{
			rebuildEventFacts(file, "go", callFacts.Nodes, stale), factsFor(winner), factsFor(tie), factsFor(far),
		}
		build, native := openRebuildEventStores(t, facts...)
		require.Zero(t, ResolveGoFrameRoutes(native))
		moves, err := RunRebuildGoFrame(t.Context(), build, rebuildGRPCReceipts(facts...), freeze(t, build))
		require.NoError(t, err)
		require.EqualValues(t, 1, moves)
		want := rebuildEventSQLiteRows(native)
		require.Len(t, want.edges, 1)
		require.Equal(t, call.To, want.edges[0].To)
		require.Equal(t, want, rebuildEventSavedRows(t, build))
	})

	t.Run("moved untagged receipt entrant", func(t *testing.T) {
		facts := []store_pebble.ParseFacts{callFacts, factsFor(winner)}
		build := openRebuildGRPCBuild(t, facts...)
		moved := cloneFrameworkEdge(call)
		moved.To, moved.Meta = "earlier-binding", nil
		require.NoError(t, build.ApplyLinkReindexes(t.Context(), []graph.EdgeReindex{{Edge: moved, OldTo: call.To}}))
		checkRefusal(t, build, moved, facts...)
	})

	t.Run("both raw claimants untagged", func(t *testing.T) {
		for _, haveHandler := range []bool{false, true} {
			facts := []store_pebble.ParseFacts{callFacts}
			if haveHandler {
				facts = append(facts, factsFor(winner))
			}
			build := openRebuildGRPCBuild(t, facts...)
			moved := cloneFrameworkEdge(call)
			moved.To, moved.Meta = "earlier-binding", nil
			require.NoError(t, build.ApplyLinkReindexes(t.Context(), []graph.EdgeReindex{{Edge: moved, OldTo: call.To}}))
			stationary := cloneFrameworkEdge(call)
			stationary.Meta = nil
			build.AddEdge(stationary)
			if haveHandler {
				checkRefusal(t, build, moved, facts...)
				continue
			}
			before := rebuildEventSavedRows(t, build)
			moves, err := RunRebuildGoFrame(t.Context(), build, rebuildGRPCReceipts(facts...), freeze(t, build))
			require.NoError(t, err)
			require.Zero(t, moves)
			require.Equal(t, before, rebuildEventSavedRows(t, build))
		}
	})

	t.Run("receipt-only route uses raw source workspace", func(t *testing.T) {
		plain := cloneFrameworkEdge(call)
		plain.Meta = nil
		source := *route
		source.Kind = graph.KindFunction // the route namespace, not the node kind, admits the turn
		otherSource := source
		otherSource.WorkspaceID = "foreign"
		for _, rawMatches := range []bool{false, true} {
			liveSource, rawSource := &source, &otherSource
			if rawMatches {
				liveSource, rawSource = rawSource, liveSource
			}
			live := rebuildEventFacts(file, "go", []*graph.Node{liveSource, alias}, plain)
			raw := rebuildEventFacts(file, "go", []*graph.Node{rawSource, alias}, call)
			build := openRebuildGRPCBuild(t, live, factsFor(winner))
			if rawMatches {
				checkRefusal(t, build, plain, raw, factsFor(winner))
				continue
			}
			before := rebuildEventSavedRows(t, build)
			moves, err := RunRebuildGoFrame(t.Context(), build, rebuildGRPCReceipts(raw, factsFor(winner)), freeze(t, build))
			require.NoError(t, err)
			require.Zero(t, moves)
			require.Equal(t, before, rebuildEventSavedRows(t, build))
		}
	})

	t.Run("duplicate raw Call role", func(t *testing.T) {
		for _, conflict := range []bool{false, true} {
			first, dup := cloneFrameworkEdge(call), cloneFrameworkEdge(call)
			if conflict {
				dup.Meta["goframe_route"] = route.ID
			} else {
				dup.Meta["observer"] = "not the first payload"
			}
			facts := []store_pebble.ParseFacts{callFacts, factsFor(winner)}
			receipts := []store_pebble.ParseFacts{
				rebuildEventFacts(file, "go", callFacts.Nodes, first), rebuildEventFacts(file, "go", nil, dup), factsFor(winner),
			}
			build, native := openRebuildEventStores(t, facts...)
			if conflict {
				checkRefusal(t, build, call, receipts...)
				continue
			}
			require.Equal(t, 1, ResolveGoFrameRoutes(native))
			moves, err := RunRebuildGoFrame(t.Context(), build, rebuildGRPCReceipts(receipts...), freeze(t, build))
			require.NoError(t, err)
			require.EqualValues(t, 1, moves)
			require.Equal(t, rebuildEventSQLiteRows(native), rebuildEventSavedRows(t, build))
		}
	})

	t.Run("stationary duplicate disagreement", func(t *testing.T) {
		bound := cloneFrameworkEdge(call)
		bound.To = winner.ID
		dup := cloneFrameworkEdge(bound)
		dup.Meta["goframe_request_pkg"] = "other-package"
		facts := rebuildEventFacts(file, "go", callFacts.Nodes, bound)
		build := openRebuildGRPCBuild(t, facts, factsFor(winner))
		before := rebuildEventSavedRows(t, build)
		receipts := rebuildGRPCReceipts(facts, rebuildEventFacts(file, "go", nil, dup), factsFor(winner))
		moves, err := RunRebuildGoFrame(t.Context(), build, receipts, freeze(t, build))
		require.NoError(t, err)
		require.Zero(t, moves)
		require.Equal(t, before, rebuildEventSavedRows(t, build))
	})

	t.Run("stationary foreign handler with source receipt", func(t *testing.T) {
		facts := rebuildEventFacts(file, "go", callFacts.Nodes, call)
		build := openRebuildGRPCBuild(t, facts, factsFor(otherWorkspace))
		before := rebuildEventSavedRows(t, build)
		receipts := rebuildGRPCReceipts(rebuildEventFacts(file, "go", callFacts.Nodes), factsFor(otherWorkspace))
		moves, err := RunRebuildGoFrame(t.Context(), build, receipts, freeze(t, build))
		require.NoError(t, err)
		require.Zero(t, moves)
		require.Equal(t, before, rebuildEventSavedRows(t, build))
	})

	t.Run("ambiguous receipt of a stationary tagged Call", func(t *testing.T) {
		bound := cloneFrameworkEdge(call)
		bound.To = winner.ID
		raw := cloneFrameworkEdge(bound)
		raw.Meta["goframe_request_pkg"] = "other-package"
		moved := cloneFrameworkEdge(bound)
		moved.To, moved.Meta = "z-earlier-binding", nil
		input := rebuildEventFacts(file, "go", callFacts.Nodes, bound)
		build := openRebuildGRPCBuild(t, input, factsFor(winner))
		require.NoError(t, build.ApplyLinkReindexes(t.Context(), []graph.EdgeReindex{{Edge: moved, OldTo: bound.To}}))
		build.AddEdge(bound)
		checkRefusal(t, build, bound, rebuildEventFacts(file, "go", callFacts.Nodes, raw), factsFor(winner))
	})

	t.Run("unassigned claimant keeps its live role", func(t *testing.T) {
		bound := cloneFrameworkEdge(call)
		bound.To = winner.ID
		other := cloneFrameworkEdge(bound)
		other.To = "unresolved::*.MissingReq"
		other.Meta["goframe_request_type"] = "MissingReq"
		plain := cloneFrameworkEdge(bound)
		plain.Meta = nil
		rawWinner := *winner
		rawWinner.Meta = otherPkg.Meta
		input := rebuildEventFacts(file, "go", callFacts.Nodes, bound)
		build := openRebuildGRPCBuild(t, input, factsFor(winner))
		require.NoError(t, build.ApplyLinkReindexes(t.Context(), []graph.EdgeReindex{{Edge: other, OldTo: bound.To}}))
		build.AddEdge(bound)
		// Assigning the untagged receipt to the other row would leave the
		// bound row tagged, but its handler receipt no longer matches.
		checkRefusal(t, build, bound, rebuildEventFacts(file, "go", callFacts.Nodes, plain), factsFor(&rawWinner))
	})

	t.Run("raw role and receipt scope guards", func(t *testing.T) {
		for _, mode := range []string{"language", "owner", "removed role", "missing source", "no handler receipt"} {
			build := openRebuildGRPCBuild(t, callFacts, factsFor(winner), factsFor(otherWorkspace))
			raw, rawHandler := callFacts, factsFor(winner)
			switch mode {
			case "language":
				raw.EffectiveLanguage = ""
			case "owner":
				raw.File.FilePath = "wrong.go"
			case "removed role":
				plain := cloneFrameworkEdge(call)
				plain.Meta = nil
				raw.Edges = []*graph.Edge{plain}
			case "missing source":
				raw.Nodes = []*graph.Node{alias}
			case "no handler receipt":
				rawHandler.Nodes = nil
			}
			if mode == "missing source" {
				checkRefusal(t, build, call, raw, rawHandler, factsFor(otherWorkspace))
			} else {
				checkRefusal(t, build, call, raw, rawHandler)
			}
		}
	})

	t.Run("only optional handler cannot change a stationary Call", func(t *testing.T) {
		bound := cloneFrameworkEdge(call)
		bound.To = winner.ID
		boundFacts := rebuildEventFacts(file, "go", callFacts.Nodes, bound)
		build := openRebuildGRPCBuild(t, boundFacts, factsFor(winner))
		before := rebuildEventSavedRows(t, build)
		moves, err := RunRebuildGoFrame(t.Context(), build, rebuildGRPCReceipts(boundFacts), freeze(t, build))
		require.NoError(t, err)
		require.Zero(t, moves)
		require.Equal(t, before, rebuildEventSavedRows(t, build))
	})

	t.Run("occupied mirror keeps its payload", func(t *testing.T) {
		_, mirror := goframeRouteRebind(cloneFrameworkEdge(call), req, winner)
		mirror.Meta["incumbent"] = true
		facts := rebuildEventFacts(file, "go", callFacts.Nodes, call, mirror)
		build, native := openRebuildEventStores(t, facts, factsFor(winner))
		require.Equal(t, 1, ResolveGoFrameRoutes(native))
		moves, err := RunRebuildGoFrame(t.Context(), build, rebuildGRPCReceipts(facts, factsFor(winner)), freeze(t, build))
		require.NoError(t, err)
		require.EqualValues(t, 1, moves)
		want := rebuildEventSQLiteRows(native)
		require.Contains(t, want.edges, mirror)
		require.Equal(t, want, rebuildEventSavedRows(t, build))
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
