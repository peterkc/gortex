package resolver

import (
	"cmp"
	"context"
	"fmt"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/graph/store_pebble"
	"github.com/zzet/gortex/internal/graph/store_sqlite"
)

// The same SQLite census rows in opposed insertion orders produce different
// native saved calls. The complete-key writer is compared to the unchanged
// native reducer with only the order of the census identities locked.
func TestRebuildFnPointerCopyOrder(t *testing.T) {
	for _, tc := range []struct {
		name                     string
		reverseKey               bool
		threshold                int
		wantTail                 string
		wantResolved, wantSweeps int
		wantFlushes              int
	}{
		{"forward-1B", false, 1, "handler", 2, 2, 3},
		{"reverse-1MiB", true, 1 << 20, "unresolved::*.fn", 1, 8, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			nodes, edges := rebuildFnPointerChain(tc.reverseKey)
			reverse := slices.Clone(edges)
			slices.Reverse(reverse)
			rawForward := rebuildFnPointerNative(t, nodes, edges, false, nil)
			rawReverse := rebuildFnPointerNative(t, nodes, reverse, false, nil)
			require.Equal(t, "handler", rebuildFnPointerSavedAt(t, rawForward, graph.EdgeCalls, "caller", 9).To)
			require.Equal(t, "unresolved::*.fn", rebuildFnPointerSavedAt(t, rawReverse, graph.EdgeCalls, "caller", 9).To)

			// The no-hit copies exceed 1 MiB together. The small moves
			// stay in memory and the copies do not count as spill.
			if tc.threshold > 1 {
				wide := strings.Repeat("x", 128<<10)
				for i := range 10 {
					e := rebuildFnPointerReg("padding", "unresolved::*.fn", i, "unused", "")
					e.Meta["fnptr_copy_struct"], e.Meta["fnptr_copy_field"] = wide, "fn"
					reverse = append(reverse, e)
				}
			}
			want := rebuildFnPointerNative(t, nodes, reverse, true, nil)
			b := rebuildFnPointerBuild(t)
			b.AddBatch(nodes, reverse)
			p, err := CaptureRebuildFnPointer(t.Context(), b, tc.threshold)
			require.NoError(t, err)
			stats, err := RunRebuildFnPointer(t.Context(), p)
			require.NoError(t, err)
			got := rebuildFnPointerSaved(t, b)
			require.Len(t, got, len(reverse))
			require.Equal(t, tc.wantTail, rebuildFnPointerSavedAt(t, got, graph.EdgeCalls, "caller", 9).To)
			require.Equal(t, "handler", rebuildFnPointerSavedAt(t, got, graph.EdgeCalls, "caller", 8).To)
			require.Equal(t, want, got)
			require.Equal(t, tc.wantResolved, stats.Resolved)
			require.Equal(t, tc.wantSweeps, stats.Sweeps)
			require.Equal(t, tc.reverseKey, stats.Capped)
			if tc.wantFlushes == 0 {
				require.Equal(t, store_pebble.FnPointerSpill{}, stats.Spill)
			} else {
				// At 1 B each of the three moves (one registration and
				// two calls) closes a one-row run.
				require.Equal(t, tc.wantFlushes, stats.Spill.Flushes)
				require.Positive(t, stats.Spill.InputBytes)
				require.Positive(t, stats.Spill.TableBytes)
			}
		})
	}
}

func TestRebuildFnPointerOverlayDoesNotSpill(t *testing.T) {
	nodes := []*graph.Node{
		rebuildFnPointerNode("caller", "run", "saved.c"),
		rebuildFnPointerNode("a", "A", "saved.c"),
		rebuildFnPointerNode("b", "B", "saved.c"),
	}
	copyReg := rebuildFnPointerReg("caller", "unresolved::*.fn", 3, "dispatch", "")
	copyReg.Meta["fnptr_copy_struct"], copyReg.Meta["fnptr_copy_field"] = "slot", "fn"
	// Both registrations and the primary call already point at their
	// targets. The copy yields a fanout edge without staging a move.
	edges := []*graph.Edge{
		rebuildFnPointerReg("caller", "a", 1, "slot", "A"),
		rebuildFnPointerReg("caller", "b", 2, "slot", "B"),
		copyReg,
		rebuildFnPointerCall("caller", "a", 4, "dispatch"),
	}
	want := rebuildFnPointerNative(t, nodes, edges, true, nil)
	b := rebuildFnPointerBuild(t)
	b.AddBatch(nodes, edges)
	p, err := CaptureRebuildFnPointer(t.Context(), b, 1)
	require.NoError(t, err)
	stats, err := RunRebuildFnPointer(t.Context(), p)
	require.NoError(t, err)
	got := rebuildFnPointerSaved(t, b)
	require.Len(t, got, len(edges)+1)
	require.Equal(t, want, got)
	require.Equal(t, 2, stats.Resolved)
	require.Equal(t, store_pebble.FnPointerSpill{}, stats.Spill)
}

func TestRebuildFnPointerCensusAndSelection(t *testing.T) {
	caller := rebuildFnPointerNode("caller", "run", "caller.c")
	local := rebuildFnPointerNode("local", "action", "caller.c")
	definition := rebuildFnPointerNode("getter-action", "action", "getter.c")
	getter := rebuildFnPointerNode("getter", "useStore", "getter.c")
	getter.Kind = graph.KindVariable
	otherBoundary := rebuildFnPointerNode("remote", "action", "getter.c")
	otherBoundary.WorkspaceID = "other"
	nodes := []*graph.Node{caller, local, definition, getter, otherBoundary}
	reg := rebuildFnPointerReg("caller", "unresolved::*.action", 1, "slot", "action")
	reg.Meta["store_binding"] = "useStore"
	lost := rebuildFnPointerReg("caller", "unresolved::*.lost", 2, "lostSlot", "action")
	call := rebuildFnPointerCall("caller", "unresolved::*.fn", 3, "slot")
	lateCall := rebuildFnPointerCall("caller", "unresolved::*.fn", 4, "slot")
	lostCall := rebuildFnPointerCall("caller", "unresolved::*.fn", 5, "lostSlot")
	edges := []*graph.Edge{reg, lost, call, lostCall}
	mutate := func(g graph.Store) {
		g.RemoveEdge(lost.From, lost.To, lost.Kind)
		moved := *lost
		moved.To = local.ID
		g.AddBatch(nil, []*graph.Edge{&moved, lateCall})
	}
	want := rebuildFnPointerNative(t, nodes, edges, true, mutate)
	b := rebuildFnPointerBuild(t)
	b.AddBatch(nodes, edges)
	p, err := CaptureRebuildFnPointer(t.Context(), b, 1<<20)
	require.NoError(t, err)
	mutate(b)
	stats, err := RunRebuildFnPointer(t.Context(), p)
	require.NoError(t, err)
	got := rebuildFnPointerSaved(t, b)
	require.Len(t, got, 5)
	require.Equal(t, want, got)
	require.Equal(t, "getter-action", rebuildFnPointerSavedAt(t, got, graph.EdgeCalls, "caller", 3).To)
	for _, line := range []int{4, 5} {
		require.Equal(t, "unresolved::*.fn", rebuildFnPointerSavedAt(t, got, graph.EdgeCalls, "caller", line).To)
	}
	require.Equal(t, 1, stats.Resolved)

	// Cancellation after capture leaves no applied moves and releases census.
	before := rebuildFnPointerSaved(t, b)
	p, err = CaptureRebuildFnPointer(t.Context(), b, 1<<20)
	require.NoError(t, err)
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	_, err = RunRebuildFnPointer(ctx, p)
	require.ErrorIs(t, err, context.Canceled)
	require.Equal(t, before, rebuildFnPointerSaved(t, b))
	p, err = CaptureRebuildFnPointer(t.Context(), b, 1<<20)
	require.NoError(t, err)
	require.NoError(t, p.Close())
}

func TestRebuildFnPointerFanoutAndStaleCall(t *testing.T) {
	nodes := []*graph.Node{rebuildFnPointerNode("caller", "run", "saved.c")}
	var edges []*graph.Edge
	for i := range fnPointerFanoutCap + 2 {
		id := fmt.Sprintf("fn%02d", i)
		nodes = append(nodes, rebuildFnPointerNode(id, id, "saved.c"))
		edges = append(edges, rebuildFnPointerReg("caller", "unresolved::*."+id, i, "slot", id))
	}
	// The synthesized extra to fn01 must not replace a stationary native
	// row at the same identity. An unrelated stale call loses its stamp.
	existing := &graph.Edge{From: "caller", To: "fn01", Kind: graph.EdgeCalls, FilePath: "saved.c", Line: 90,
		Origin: graph.OriginASTInferred, Meta: map[string]any{"saved": "stationary"}}
	stale := rebuildFnPointerCall("caller", "fn02", 91, "unseeded")
	StampSynthesized(stale, SynthFnPointerDispatch)
	edges = append(edges, existing, stale, rebuildFnPointerCall("caller", "unresolved::*.fn", 90, "slot"))
	want := rebuildFnPointerNative(t, nodes, edges, true, nil)
	b := rebuildFnPointerBuild(t)
	b.AddBatch(nodes, edges)
	p, err := CaptureRebuildFnPointer(t.Context(), b, 1<<20)
	require.NoError(t, err)
	stats, err := RunRebuildFnPointer(t.Context(), p)
	require.NoError(t, err)
	got := rebuildFnPointerSaved(t, b)
	require.Equal(t, want, got)
	require.Len(t, got, fnPointerFanoutCap*2+3)
	require.Equal(t, fnPointerFanoutCap, stats.Resolved)
	reset := rebuildFnPointerSavedAt(t, got, graph.EdgeCalls, "caller", 91)
	require.Equal(t, "unresolved::*.fn", reset.To)
	require.NotContains(t, reset.Meta, MetaSynthesizedBy)
	var targets []string
	for _, e := range got {
		if e.Kind == graph.EdgeCalls && e.Line == 90 {
			targets = append(targets, e.To)
			if e.To == "fn01" {
				require.Equal(t, existing, e)
			}
		}
	}
	require.Len(t, targets, fnPointerFanoutCap)
	require.Equal(t, "fn00", targets[0])
	require.Equal(t, "fn63", targets[len(targets)-1])
}

func TestRebuildFnPointerReindexWindow(t *testing.T) {
	for _, first := range []int{256, 5000} {
		t.Run(fmt.Sprint(first), func(t *testing.T) {
			nodes := []*graph.Node{
				rebuildFnPointerNode("a", "filler", "saved.c"), rebuildFnPointerNode("z", "reg", "saved.c"),
				rebuildFnPointerNode("b", "B", "b.c"), rebuildFnPointerNode("c", "C", "c.c"),
			}
			var edges []*graph.Edge
			for i := 1; i < first; i++ {
				edges = append(edges, rebuildFnPointerReg("a", "unresolved::*.B", i, "slot", "B"))
			}
			edges = append(edges, rebuildFnPointerReg("z", "a", 1, "slot", "B"), rebuildFnPointerReg("z", "b", 1, "slot", "C"))
			want := rebuildFnPointerNative(t, nodes, edges, true, nil)
			b := rebuildFnPointerBuild(t)
			b.AddBatch(nodes, edges)
			p, err := CaptureRebuildFnPointer(t.Context(), b, 32<<10)
			require.NoError(t, err)
			stats, err := RunRebuildFnPointer(t.Context(), p)
			require.NoError(t, err)
			got := rebuildFnPointerSaved(t, b)
			require.Equal(t, want, got)
			wantRows := first + 1
			if first == 5000 {
				wantRows-- // b is stationary for the first native chunk.
				require.Greater(t, stats.Spill.Flushes, 1) // The native window spans runs.
			}
			require.Len(t, got, wantRows)
			require.Equal(t, 1, stats.Sweeps)
			require.Equal(t, "c", got[len(got)-1].To)
		})
	}
}

func TestRebuildFnPointerHighNameCardinality(t *testing.T) {
	b := rebuildFnPointerBuild(t)
	const matches = 4096
	// The serialized candidate page would exceed its 128 MiB ceiling,
	// even before including the caller's file's sole matching candidate.
	payload := strings.Repeat("x", 32<<10)
	for i := range matches {
		n := rebuildFnPointerNode(fmt.Sprintf("candidate%04d", i), "action", "remote.c")
		n.Meta = map[string]any{"payload": payload}
		b.AddNode(n)
	}
	b.AddNode(rebuildFnPointerNode("caller", "run", "caller.c"))
	b.AddNode(rebuildFnPointerNode("winner", "action", "caller.c"))
	reg := rebuildFnPointerReg("caller", "unresolved::*.action", 1, "slot", "action")
	b.AddBatch(nil, []*graph.Edge{reg, rebuildFnPointerCall("caller", "unresolved::*.fn", 2, "slot")})
	p, err := CaptureRebuildFnPointer(t.Context(), b, 1<<20)
	require.NoError(t, err)
	stats, err := RunRebuildFnPointer(t.Context(), p)
	require.NoError(t, err)
	got := rebuildFnPointerSaved(t, b)
	require.Len(t, got, 2)
	require.Equal(t, "winner", rebuildFnPointerSavedAt(t, got, graph.EdgeCalls, "caller", 2).To)
	require.Equal(t, 1, stats.Resolved)
}

func rebuildFnPointerChain(reverseKey bool) ([]*graph.Node, []*graph.Edge) {
	nodes := []*graph.Node{
		rebuildFnPointerNode("caller", "run", "caller.c"), rebuildFnPointerNode("handler", "handler", "caller.c"),
		rebuildFnPointerNode("reg", "reg", "saved.c"), rebuildFnPointerNode("padding", "pad", "saved.c"),
	}
	edges := []*graph.Edge{rebuildFnPointerReg("reg", "unresolved::*.handler", 0, "s0", "handler")}
	for hop := 1; hop <= 9; hop++ {
		line := hop
		if reverseKey {
			line = 10 - hop
		}
		e := rebuildFnPointerReg("reg", "unresolved::*.fn", line, fmt.Sprintf("s%d", hop), "")
		e.Meta["fnptr_copy_struct"], e.Meta["fnptr_copy_field"] = fmt.Sprintf("s%d", hop-1), "fn"
		edges = append(edges, e)
	}
	return nodes, append(edges, rebuildFnPointerCall("caller", "unresolved::*.fn", 8, "s8"), rebuildFnPointerCall("caller", "unresolved::*.fn", 9, "s9"))
}

func rebuildFnPointerNode(id, name, path string) *graph.Node {
	return &graph.Node{ID: id, Name: name, Kind: graph.KindFunction, FilePath: path, Language: "c"}
}

func rebuildFnPointerReg(from, to string, line int, slot, name string) *graph.Edge {
	return &graph.Edge{From: from, To: to, Kind: graph.EdgeReferences, Line: line, FilePath: "saved.c", Origin: graph.OriginASTInferred,
		Meta: map[string]any{"via": fnPtrRegVia, "fnptr_struct": slot, "fnptr_field": "fn", "fnptr_fn": name}}
}

func rebuildFnPointerCall(from, to string, line int, slot string) *graph.Edge {
	return &graph.Edge{From: from, To: to, Kind: graph.EdgeCalls, Line: line, FilePath: "saved.c", Origin: graph.OriginASTInferred,
		Meta: map[string]any{"via": fnPtrDispatchVia, "fnptr_struct": slot, "fnptr_field": "fn"}}
}

func rebuildFnPointerBuild(t *testing.T) *store_pebble.BuildStore {
	t.Helper()
	b, err := store_pebble.OpenBuild(filepath.Join(t.TempDir(), "build"), store_pebble.FullProfile)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, b.Close()) })
	return b
}

func rebuildFnPointerNative(t *testing.T, nodes []*graph.Node, edges []*graph.Edge, lockOrder bool, mutate func(graph.Store)) []*graph.Edge {
	t.Helper()
	g, err := store_sqlite.Open(filepath.Join(t.TempDir(), "oracle.sqlite"))
	require.NoError(t, err)
	defer func() { require.NoError(t, g.Close()) }()
	if lockOrder {
		require.NoError(t, g.AddBatchChecked(nodes, edges))
	} else {
		require.NoError(t, g.AddBatchChecked(nodes, nil))
		for _, e := range edges {
			// Individual writes force row-ID order without batch reordering.
			require.NoError(t, g.AddBatchChecked(nil, []*graph.Edge{e}))
		}
	}
	streams := newFrameworkStreamCandidates(g, map[string]int{"c": 1}, nil)
	collectFrameworkEdgeCensus(g, streams)
	ids := streams.perPass[SynthFnPointerDispatch]
	require.NotNil(t, ids)
	if lockOrder {
		slices.SortFunc(ids.refs, rebuildFnPointerCompare)
		slices.SortFunc(ids.calls, rebuildFnPointerCompare)
	}
	if mutate != nil {
		mutate(g)
	}
	bundle := refetchFrameworkCandidates(g, ids)
	require.NotNil(t, bundle)
	runLegacyFrameworkSynth(g, func(s graph.Store) int { return resolveFnPointerDispatch(s, bundle) })
	var rows []*graph.Edge
	for _, kind := range []graph.EdgeKind{graph.EdgeReferences, graph.EdgeCalls} {
		for e := range g.EdgesByKind(kind) {
			rows = append(rows, e)
		}
	}
	rebuildFnPointerSortRows(rows)
	return rows
}

func rebuildFnPointerSaved(t *testing.T, b *store_pebble.BuildStore) []*graph.Edge {
	t.Helper()
	var rows []*graph.Edge
	require.NoError(t, b.RebuildView().VisitEdges(t.Context(), func(e *graph.Edge) error {
		rows = append(rows, e)
		return nil
	}))
	rebuildFnPointerSortRows(rows)
	return rows
}

func rebuildFnPointerSortRows(rows []*graph.Edge) {
	slices.SortFunc(rows, func(a, b *graph.Edge) int {
		return rebuildFnPointerCompare(graph.EdgeIdentityFor(a), graph.EdgeIdentityFor(b))
	})
}

func rebuildFnPointerCompare(a, b graph.EdgeIdentity) int {
	return cmp.Or(cmp.Compare(a.From, b.From), cmp.Compare(a.Kind, b.Kind), cmp.Compare(a.To, b.To), cmp.Compare(a.FilePath, b.FilePath), cmp.Compare(a.Line, b.Line))
}

func rebuildFnPointerSavedAt(t *testing.T, rows []*graph.Edge, kind graph.EdgeKind, from string, line int) *graph.Edge {
	t.Helper()
	for _, e := range rows {
		if e.Kind == kind && e.From == from && e.Line == line {
			return e
		}
	}
	t.Fatalf("missing saved edge %s from %s at %d", kind, from, line)
	return nil
}
