package resolver

import (
	"fmt"
	"slices"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/graph/store_pebble"
)

func TestRebuildFnPointerStageSQLiteReceiptOrder(t *testing.T) {
	const file, other, lang = "saved.c", "other.c", "c"
	const stub = "unresolved::*.fn"
	caller := rebuildEventNode(file, "run", lang, "")
	handler := rebuildEventNode(file, "handler", lang, "")
	secondary := rebuildEventNode(file, "secondary", lang, "")
	third := rebuildEventNode(file, "third", lang, "")
	otherCaller := rebuildEventNode(other, "register", lang, "")
	// Both handlers are retained callables. The registration's own file
	// wins, even though the other same-name node sorts before it.
	homonym := rebuildEventNode(other, handler.Name, lang, "")
	occupied := &graph.Edge{From: caller.ID, To: secondary.ID, Kind: graph.EdgeCalls, FilePath: file, Line: 22,
		Origin: graph.OriginASTInferred, Meta: map[string]any{"witness": "stationary secondary"}}
	fan := rebuildFnPointerCall(caller.ID, stub, 22, "fan")
	fan.Meta["witness"] = "raw call"

	edges := []*graph.Edge{
		rebuildFnPointerReg(caller.ID, "unresolved::*.handler", 1, "s0", handler.Name),
		rebuildFnPointerReg(caller.ID, handler.ID, 2, "fan", handler.Name),
		rebuildFnPointerReg(caller.ID, secondary.ID, 3, "fan", secondary.Name),
		rebuildFnPointerReg(caller.ID, third.ID, 4, "fan", third.Name),
		rebuildFnPointerCall(caller.ID, stub, 20, "s8"),
		rebuildFnPointerCall(caller.ID, stub, 21, "s9"),
		occupied, fan,
	}
	var otherEdges []*graph.Edge
	for hop := 1; hop <= 9; hop++ {
		e := rebuildFnPointerReg(caller.ID, stub, 10+hop, fmt.Sprintf("s%d", hop), "")
		e.Meta["fnptr_copy_struct"], e.Meta["fnptr_copy_field"] = fmt.Sprintf("s%d", hop-1), "fn"
		if hop <= 4 {
			edges = append(edges, e)
			continue
		}
		e.From, e.FilePath = otherCaller.ID, other
		otherEdges = append(otherEdges, e)
	}
	facts := []store_pebble.ParseFacts{
		roundTripRebuildRNBridgeFacts(t, rebuildEventFacts(file, lang, []*graph.Node{caller, handler, secondary, third}, edges...)),
		roundTripRebuildRNBridgeFacts(t, rebuildEventFacts(other, lang, []*graph.Node{otherCaller, homonym}, otherEdges...)),
	}
	selection, err := NewFrameworkSynthesizerSelection([]string{SynthFnPointerDispatch})
	require.NoError(t, err)
	for _, tc := range []struct {
		name     string
		reverse  bool
		wantTail string
	}{
		{"forward", false, handler.ID},
		{"reverse", true, stub},
	} {
		t.Run(tc.name, func(t *testing.T) {
			receipts := slices.Clone(facts)
			if tc.reverse {
				slices.Reverse(receipts)
				for i := range receipts {
					receipts[i].Edges = slices.Clone(receipts[i].Edges)
					slices.Reverse(receipts[i].Edges)
				}
			}
			b := openRebuildGRPCBuild(t, facts...)
			native := rebuildGRPCNative(t, rebuildEventFacts(file, lang, facts[0].Nodes))
			require.NoError(t, native.AddBatchChecked(facts[1].Nodes, nil))
			// Keep the private input keys fixed. Give SQLite row IDs in raw
			// receipt order; only sort the saved rows after the native turn.
			for _, f := range receipts {
				for _, e := range f.Edges {
					require.NoError(t, native.AddBatchChecked(nil, []*graph.Edge{e}))
				}
			}
			require.Equal(t, rebuildEventSQLiteRows(native), rebuildEventSavedRows(t, b), "before Function pointer")
			frozen, err := FreezeRebuildFrameworkSelection(t.Context(), b.RebuildView(), selection)
			require.NoError(t, err)
			require.NoError(t, FreezeRebuildFnPointerInputs(t.Context(), b, frozen))

			report := RunFrameworkSynthesizersWithSelection(native, selection)
			require.NoError(t, report.Err)
			rawRows := rebuildEventSQLiteRows(native)
			// Forward reaches s9 in one in-place sweep. Reverse can advance
			// only one hop per sweep and stops with s9 unresolved.
			require.Equal(t, handler.ID, rebuildFnPointerSavedAt(t, rawRows.edges, graph.EdgeCalls, caller.ID, 20).To)
			require.Equal(t, tc.wantTail, rebuildFnPointerSavedAt(t, rawRows.edges, graph.EdgeCalls, caller.ID, 21).To)
			// The approved oracle fixes copy tuple order at the native writer.
			ordered := rebuildGRPCNative(t, facts[0])
			require.NoError(t, ordered.AddBatchChecked(facts[1].Nodes, facts[1].Edges))
			streams := newFrameworkStreamCandidates(ordered, map[string]int{"c": 1}, nil)
			collectFrameworkEdgeCensus(ordered, streams)
			keys := streams.perPass[SynthFnPointerDispatch]
			require.NotNil(t, keys)
			slices.SortFunc(keys.refs, rebuildFnPointerCompare)
			bundle := refetchFrameworkCandidates(ordered, keys)
			require.NotNil(t, bundle)
			runLegacyFrameworkSynth(ordered, func(g graph.Store) int { return resolveFnPointerDispatch(g, bundle) })
			want := rebuildEventSQLiteRows(ordered)
			require.Equal(t, handler.ID, rebuildFnPointerSavedAt(t, want.edges, graph.EdgeCalls, caller.ID, 21).To)
			var fanout []*graph.Edge
			for _, e := range want.edges {
				if e.Kind == graph.EdgeCalls && e.From == caller.ID && e.Line == fan.Line {
					fanout = append(fanout, e)
				}
			}
			require.Len(t, fanout, 3)
			require.Equal(t, handler.ID, fanout[0].To)
			require.Equal(t, "raw call", fanout[0].Meta["witness"])
			require.Equal(t, occupied, fanout[1])
			require.Equal(t, third.ID, fanout[2].To)
			require.NotContains(t, fanout[2].Meta, "witness")
			require.Equal(t, SynthFnPointerDispatch, fanout[2].Meta[MetaSynthesizedBy])

			// Omitting a copy receipt must not permit either write phase.
			suppressed := slices.Clone(receipts)
			for i := range suppressed {
				if suppressed[i].File.FilePath == other {
					suppressed[i].Edges = suppressed[i].Edges[1:]
				}
			}
			before := rebuildEventSavedRows(t, b)
			stats, err := RunRebuildFnPointerStage(t.Context(), b, rebuildGRPCReceipts(suppressed...), frozen)
			require.ErrorIs(t, err, store_pebble.ErrRebuildFnPointer)
			require.ErrorIs(t, err, store_pebble.ErrRebuildUnknownProvenance)
			require.Zero(t, stats)
			require.Equal(t, before, rebuildEventSavedRows(t, b))

			require.NoError(t, FreezeRebuildFnPointerInputs(t.Context(), b, frozen))
			_, err = RunRebuildFnPointerStage(t.Context(), b, rebuildGRPCReceipts(receipts...), frozen)
			require.NoError(t, err)
			require.Equal(t, want, rebuildEventSavedRows(t, b), "after one Function pointer turn")
		})
	}
}
