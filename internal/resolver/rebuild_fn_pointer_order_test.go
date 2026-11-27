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

func TestRebuildFnPointerStageGetterWithCopy(t *testing.T) {
	const file, targetFile, lang = "saved.c", "target.c", "c"
	caller := rebuildEventNode(file, "run", lang, "")
	homonym := rebuildEventNode(file, "work", lang, "")
	target := rebuildEventNode(targetFile, homonym.Name, lang, "")
	getter := &graph.Node{ID: targetFile + "::getSlot", Name: "getSlot", Kind: graph.KindVariable, Language: lang, FilePath: targetFile}
	const stub = "unresolved::*.fn"
	reg := rebuildFnPointerReg(caller.ID, "unresolved::*.work", 1, "root", target.Name)
	reg.Meta["store_binding"] = getter.Name
	copyRef := rebuildFnPointerReg(caller.ID, stub, 2, "branch", "")
	copyRef.Meta["fnptr_copy_struct"], copyRef.Meta["fnptr_copy_field"] = "root", "fn"
	dispatch := rebuildFnPointerCall(caller.ID, stub, 3, "branch")
	facts := []store_pebble.ParseFacts{
		rebuildEventFacts(file, lang, []*graph.Node{caller, homonym}, reg, copyRef, dispatch),
		rebuildEventFacts(targetFile, lang, []*graph.Node{target, getter}),
	}
	b, native := openRebuildEventStores(t, facts...)
	selected, err := NewFrameworkSynthesizerSelection([]string{SynthFnPointerDispatch})
	require.NoError(t, err)
	frozen, err := FreezeRebuildFrameworkSelection(t.Context(), b.RebuildView(), selected)
	require.NoError(t, err)
	require.NoError(t, FreezeRebuildFnPointerInputs(t.Context(), b, frozen))
	report := RunFrameworkSynthesizersWithSelection(native, selected)
	require.NoError(t, report.Err)
	want := rebuildEventSQLiteRows(native)
	require.Equal(t, target.ID, rebuildFnPointerSavedAt(t, want.edges, graph.EdgeCalls, caller.ID, 3).To)
	stats, err := RunRebuildFnPointerStage(t.Context(), b, rebuildGRPCReceipts(facts...), frozen)
	require.NoError(t, err)
	require.EqualValues(t, 1, stats.RefMoves)
	require.EqualValues(t, 1, stats.CallMoves)
	require.Equal(t, want, rebuildEventSavedRows(t, b))
}

func TestRebuildFnPointerStageProofFollowsDispatchSlots(t *testing.T) {
	const file, lang = "saved.c", "c"
	const stub = "unresolved::*.fn"
	caller := rebuildEventNode(file, "run", lang, "")
	handler := rebuildEventNode(file, "work", lang, "")
	spare := rebuildEventNode(file, "spare", lang, "")
	seed := rebuildFnPointerReg(caller.ID, handler.ID, 1, "root", handler.Name)
	isolated := rebuildFnPointerReg(caller.ID, spare.ID, 2, "isolated", spare.Name)
	copyRef := rebuildFnPointerReg(caller.ID, stub, 3, "branch", "")
	copyRef.Meta["fnptr_copy_struct"], copyRef.Meta["fnptr_copy_field"] = "root", "fn"
	self := rebuildFnPointerReg(caller.ID, stub, 4, "branch", "")
	self.Meta["fnptr_copy_struct"], self.Meta["fnptr_copy_field"] = "branch", "fn"
	dispatch := rebuildFnPointerCall(caller.ID, stub, 5, "branch")
	facts := rebuildEventFacts(file, lang, []*graph.Node{caller, handler, spare}, seed, isolated, copyRef, self, dispatch)
	selected, err := NewFrameworkSynthesizerSelection([]string{SynthFnPointerDispatch})
	require.NoError(t, err)
	for _, tc := range []struct {
		name   string
		omit   *graph.Edge
		refuse bool
	}{
		{"disconnected seed", isolated, false},
		{"self copy", self, false},
		{"connected seed", seed, true},
		{"connected copy", copyRef, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			b, native := openRebuildEventStores(t, facts)
			before := rebuildEventSavedRows(t, b)
			frozen, err := FreezeRebuildFrameworkSelection(t.Context(), b.RebuildView(), selected)
			require.NoError(t, err)
			require.NoError(t, FreezeRebuildFnPointerInputs(t.Context(), b, frozen))
			missing := facts
			missing.Edges = slices.DeleteFunc(slices.Clone(facts.Edges), func(e *graph.Edge) bool { return e == tc.omit })
			stats, err := RunRebuildFnPointerStage(t.Context(), b, rebuildGRPCReceipts(missing), frozen)
			if tc.refuse {
				var refused *store_pebble.RebuildUnknownProvenanceError
				require.ErrorAs(t, err, &refused)
				require.Equal(t, graph.EdgeIdentityFor(tc.omit), refused.First)
				require.Zero(t, stats)
				require.Equal(t, before, rebuildEventSavedRows(t, b))
				return
			}
			require.NoError(t, err)
			require.EqualValues(t, 1, stats.CallMoves)
			report := RunFrameworkSynthesizersWithSelection(native, selected)
			require.NoError(t, report.Err)
			want := rebuildEventSQLiteRows(native)
			require.Equal(t, handler.ID, rebuildFnPointerSavedAt(t, want.edges, graph.EdgeCalls, caller.ID, 5).To)
			require.Equal(t, want, rebuildEventSavedRows(t, b))
		})
	}
}

func TestRebuildFnPointerStageCountsSkippedReceiptClaims(t *testing.T) {
	const file, lang = "saved.c", "c"
	caller := rebuildEventNode(file, "run", lang, "")
	target := rebuildEventNode(file, "work", lang, "")
	reg := rebuildFnPointerReg(caller.ID, target.ID, 1, "slot", target.Name)
	call := rebuildFnPointerCall(caller.ID, "unresolved::*.fn", 2, "slot")
	selected, err := NewFrameworkSynthesizerSelection([]string{SynthFnPointerDispatch})
	require.NoError(t, err)
	for _, kind := range []graph.EdgeKind{graph.EdgeReferences, graph.EdgeCalls} {
		t.Run(string(kind), func(t *testing.T) {
			for _, duplicate := range []bool{false, true} {
				ctx := t.Context()
				input := call
				if kind == graph.EdgeReferences {
					input = reg
				}
				facts := rebuildEventFacts(file, lang, []*graph.Node{caller, target}, reg, call)
				b := openRebuildGRPCBuild(t, facts)
				derived := *input
				derived.To = "unresolved::alternate"
				require.NoError(t, b.ApplyLinkReindexes(ctx, []graph.EdgeReindex{{Edge: &derived, OldTo: input.To}}))
				if duplicate {
					old := *input
					old.Meta = map[string]any{"via": input.Meta["via"]}
					b.AddEdge(&old)
				}
				before := rebuildEventSavedRows(t, b)
				frozen, err := FreezeRebuildFrameworkSelection(ctx, b.RebuildView(), selected)
				require.NoError(t, err)
				require.NoError(t, FreezeRebuildFnPointerInputs(ctx, b, frozen))
				stats, err := RunRebuildFnPointerStage(ctx, b, rebuildGRPCReceipts(facts), frozen)
				if duplicate {
					require.ErrorIs(t, err, store_pebble.ErrRebuildUnknownProvenance)
					require.Zero(t, stats)
					require.Equal(t, before, rebuildEventSavedRows(t, b))
					continue
				}
				require.NoError(t, err)
				require.EqualValues(t, 1, stats.CallMoves)
				rows := rebuildEventSavedRows(t, b).edges
				require.Equal(t, target.ID, rebuildFnPointerSavedAt(t, rows, graph.EdgeCalls, caller.ID, 2).To)
			}
		})
	}
}
