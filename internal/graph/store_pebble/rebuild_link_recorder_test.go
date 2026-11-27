package store_pebble

import (
	"errors"
	"slices"
	"testing"

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
	stationaryID := graph.EdgeIdentityFor(&stationary)
	_, err := view.RequireOriginalLinkRecorder(ctx, stationaryID)
	var unknown *RebuildUnknownProvenanceError
	if !errors.As(err, &unknown) || unknown.Count != 1 || unknown.First != stationaryID {
		t.Fatalf("required original recorder for %+v = %v; want unknown provenance for that key (count 1)", stationaryID, err)
	}

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
