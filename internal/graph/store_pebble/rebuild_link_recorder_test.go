package store_pebble

import (
	"slices"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/zzet/gortex/internal/graph"
)

func TestRebuildLinkRecorderFollowsInstalledMoves(t *testing.T) {
	ctx := t.Context()
	tracked, control := newBuild(t, FullProfile), newBuild(t, FullProfile)
	raw := graph.Edge{From: "caller", To: "unresolved::a", Kind: graph.EdgeCalls, FilePath: "routes.ts", Line: 5, Via: "first"}
	loser, blocked, stationary := raw, raw, raw
	loser.To, loser.Via = "unresolved::b", "second"
	blocked.To, blocked.Via = "unresolved::c", "blocked"
	stationary.To, stationary.Kind, stationary.Via = "occupied", graph.EdgeReferences, "stationary"
	contract := &graph.Node{ID: "route", Kind: graph.KindContract, FilePath: raw.FilePath, Language: "typescript", Meta: map[string]any{"path": "/hello"}}
	framework := &graph.Edge{From: raw.From, To: contract.ID, Kind: graph.EdgeProvides, FilePath: raw.FilePath, Line: 2, Meta: map[string]any{"method": "GET"}}
	for _, b := range []*BuildStore{tracked, control} {
		b.AddBatch([]*graph.Node{contract}, []*graph.Edge{&raw, &loser, &blocked, &stationary, framework})
	}

	// ApplyLinkReindexes takes moves in complete old-key order.
	move := func(old graph.Edge, to string) graph.EdgeReindex {
		live := old
		live.To, live.Kind = to, graph.EdgeReferences
		return graph.EdgeReindex{Edge: &live, OldTo: old.To, OldKind: old.Kind}
	}
	winner := move(raw, "free")
	moves := []graph.EdgeReindex{winner, move(loser, "free"), move(blocked, stationary.To)}
	if err := tracked.ApplyLinkReindexes(ctx, moves); err != nil {
		t.Fatal(err)
	}
	if err := control.withWrites(func() error { return control.reindexChunkWithRecorders(moves, false) }); err != nil {
		t.Fatal(err)
	}

	// Strings retain the exact bytes and do not alias Pebble iterator buffers.
	readRows := func(b *BuildStore, prefix []byte) [][2]string {
		t.Helper()
		var rows [][2]string
		if err := pendingVisit(ctx, b.pending, prefix, func(r row) error {
			rows = append(rows, [2]string{string(r.key), string(r.value)})
			return nil
		}); err != nil {
			t.Fatal(err)
		}
		return rows
	}
	for _, family := range []struct {
		prefix []byte
		count  int
	}{
		{[]byte{familyNode}, 1},
		{[]byte{familyEdge, edgeOut}, 3},
		{[]byte{familyEdge, edgeIn}, 3},
	} {
		got, want := readRows(tracked, family.prefix), readRows(control, family.prefix)
		if len(want) != family.count || !slices.Equal(got, want) {
			t.Fatalf("pending rows under %q: got %q, want %d control rows %q", family.prefix, got, family.count, want)
		}
	}
	for _, edge := range []*graph.Edge{winner.Edge, &stationary} {
		got, err := tracked.pendingEdge(pendingEdgeKey(edge))
		if err != nil || got == nil || got.Via != edge.Via {
			t.Fatalf("installed edge at %q = %+v, %v; want Via %q", edge.To, got, err, edge.Via)
		}
	}

	view := tracked.RebuildView()
	lookup := func(v RebuildView, edge, original *graph.Edge) {
		t.Helper()
		live, want := graph.EdgeIdentityFor(edge), graph.EdgeIdentityFor(original)
		got, found, err := v.OriginalLinkRecorder(ctx, live)
		if err != nil || found != (original != nil) || (found && got != want) {
			t.Fatalf("original recorder for %+v = %+v, %t, %v; want %+v, found=%t", live, got, found, err, want, original != nil)
		}
	}
	lookup(control.RebuildView(), winner.Edge, nil)
	lookup(view, winner.Edge, &raw)
	lookup(view, &stationary, nil)
	lookup(view, framework, nil)

	second := move(*winner.Edge, "second-live")
	if err := tracked.ApplyLinkReindexes(ctx, []graph.EdgeReindex{second}); err != nil {
		t.Fatal(err)
	}
	lookup(view, winner.Edge, nil)
	lookup(view, second.Edge, &raw)

	last := move(*second.Edge, "generic-live")
	tracked.ReindexEdges([]graph.EdgeReindex{last})
	lookup(view, second.Edge, nil)
	lookup(view, last.Edge, &raw)
}

func TestRebuildLinkRecorderFollowsSameChunkChain(t *testing.T) {
	ctx := t.Context()
	b := newBuild(t, FullProfile)
	raw := graph.Edge{From: "caller", To: "unresolved::a", Kind: graph.EdgeCalls, FilePath: "routes.ts", Line: 5}
	live, middle, final := raw, raw, raw
	live.To, middle.To, final.To = "first", "unstored", "last"
	b.AddEdge(&raw)
	require.NoError(t, b.ApplyLinkReindexes(ctx, []graph.EdgeReindex{{Edge: &live, OldTo: raw.To}}))
	b.ReindexEdges([]graph.EdgeReindex{
		{Edge: &middle, OldTo: live.To},
		{Edge: &final, OldTo: middle.To},
	})
	view := b.RebuildView()
	for _, e := range []*graph.Edge{&live, &middle} {
		id := graph.EdgeIdentityFor(e)
		stored, err := view.Edge(ctx, id)
		require.NoError(t, err)
		require.Nil(t, stored)
		_, found, err := view.OriginalLinkRecorder(ctx, id)
		require.NoError(t, err)
		require.False(t, found)
	}
	id := graph.EdgeIdentityFor(&final)
	stored, err := view.Edge(ctx, id)
	require.NoError(t, err)
	require.NotNil(t, stored)
	got, found, err := view.OriginalLinkRecorder(ctx, id)
	require.NoError(t, err)
	require.True(t, found)
	require.Equal(t, graph.EdgeIdentityFor(&raw), got)
}

func TestRebuildLinkRecorderFollowsGuardWinner(t *testing.T) {
	for _, tc := range []struct {
		name      string
		incumbent bool
		alias     bool
		tracked   bool
	}{
		{name: "free-revert", tracked: true},
		{name: "stationary-incumbent", incumbent: true, tracked: true},
		{name: "replacement-takes-source-recorder", incumbent: true, alias: true, tracked: true},
		{name: "untracked-replacement-clears-old-recorder", incumbent: true, alias: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := t.Context()
			b := newBuild(t, FullProfile)
			raw := graph.Edge{From: "caller", To: "unresolved::original", Kind: graph.EdgeCalls, FilePath: "routes.ts", Line: 5, Via: "moved"}
			live := raw
			live.To, live.Origin = "resolved", graph.OriginASTInferred
			if tc.tracked {
				b.AddEdge(&raw)
				require.NoError(t, b.ApplyLinkReindexes(ctx, []graph.EdgeReindex{{Edge: &live, OldTo: raw.To}}))
			} else {
				b.AddEdge(&live)
			}
			wantRaw := &raw
			incumbentRaw := raw
			incumbentRaw.To, incumbentRaw.Via = "unresolved::other", "incumbent"
			const destination = "unresolved::reverted"
			if tc.incumbent {
				incumbent := incumbentRaw
				incumbent.To, incumbent.Origin = destination, graph.OriginASTInferred
				b.AddEdge(&incumbentRaw)
				require.NoError(t, b.ApplyLinkReindexes(ctx, []graph.EdgeReindex{{Edge: &incumbent, OldTo: incumbentRaw.To}}))
				if !tc.alias {
					wantRaw = &incumbentRaw
				}
			}
			// Guard duplicates borrow the same pointer. After the first revert
			// mutates To, the duplicate names the destination as its old key.
			reverted := live
			reverted.To = destination
			moves := []graph.EdgeReindex{{Edge: &reverted, OldTo: live.To}}
			if tc.alias {
				moves = append(moves, graph.EdgeReindex{Edge: &reverted, OldTo: destination})
			}
			_, err := b.ApplyRebuildGuardPage(ctx, moves, nil)
			require.NoError(t, err)
			view := b.RebuildView()
			liveID := graph.EdgeIdentityFor(&live)
			stored, err := view.Edge(ctx, liveID)
			require.NoError(t, err)
			require.Nil(t, stored)
			_, found, err := view.OriginalLinkRecorder(ctx, liveID)
			require.NoError(t, err)
			require.False(t, found)
			winnerID := graph.EdgeIdentityFor(&reverted)
			winner, err := view.Edge(ctx, winnerID)
			require.NoError(t, err)
			require.NotNil(t, winner)
			require.Equal(t, wantRaw.Via, winner.Via)
			require.Empty(t, winner.Origin)
			got, found, err := view.OriginalLinkRecorder(ctx, winnerID)
			require.NoError(t, err)
			require.Equal(t, tc.tracked, found)
			if tc.tracked {
				require.Equal(t, graph.EdgeIdentityFor(wantRaw), got)
			}
		})
	}
}

func TestRebuildLinkRecorderFollowsGuardSourceRestoration(t *testing.T) {
	ctx := t.Context()
	b := newBuild(t, FullProfile)
	call := graph.Edge{From: "caller", To: "resolved", Kind: graph.EdgeCalls, FilePath: "routes.ts", Line: 5}
	original := graph.Edge{From: call.To, To: "callee", Kind: graph.EdgeArgOf, FilePath: call.FilePath, Line: call.Line}
	live := original
	live.To = "callee#param:arg"
	b.AddEdge(&call)
	b.AddEdge(&original)
	require.NoError(t, b.ApplyLinkReindexes(ctx, []graph.EdgeReindex{{Edge: &live, OldTo: original.To}}))
	view := b.RebuildView()
	liveID := graph.EdgeIdentityFor(&live)
	got, found, err := view.OriginalLinkRecorder(ctx, liveID)
	require.NoError(t, err)
	require.True(t, found)
	require.Equal(t, graph.EdgeIdentityFor(&original), got)

	reverted := call
	reverted.To = "unresolved::callee"
	count, err := b.ApplyRebuildGuardPage(ctx, []graph.EdgeReindex{{Edge: &reverted, OldTo: call.To}}, []graph.PlaceholderRepoint{
		{OldFrom: call.To, NewFrom: reverted.To, FilePath: call.FilePath, Line: call.Line},
	})
	require.NoError(t, err)
	require.Equal(t, 1, count)
	stored, err := view.Edge(ctx, liveID)
	require.NoError(t, err)
	require.Nil(t, stored)
	_, found, err = view.OriginalLinkRecorder(ctx, liveID)
	require.NoError(t, err)
	require.False(t, found)
	restored := live
	restored.From = reverted.To
	restoredID := graph.EdgeIdentityFor(&restored)
	stored, err = view.Edge(ctx, restoredID)
	require.NoError(t, err)
	require.Equal(t, &restored, stored)
	got, found, err = view.OriginalLinkRecorder(ctx, restoredID)
	require.NoError(t, err)
	require.True(t, found)
	require.Equal(t, graph.EdgeIdentityFor(&original), got)
}
