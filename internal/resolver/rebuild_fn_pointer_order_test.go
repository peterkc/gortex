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
				want := graph.EdgeIdentityFor(dispatch)
				want.To = handler.ID
				require.Equal(t, want, refused.First)
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

func TestRebuildFnPointerStageMoveReceiptOrder(t *testing.T) {
	const file, lang = "saved.c", "c"
	caller := rebuildEventNode(file, "run", lang, "")
	target := rebuildEventNode(file, "work", lang, "")
	var edges []*graph.Edge
	for _, old := range []string{"unresolved::z", "unresolved::a"} {
		reg := rebuildFnPointerReg(caller.ID, old, 1, "slot", target.Name)
		call := rebuildFnPointerCall(caller.ID, old, 2, "slot")
		reg.Meta["witness"], call.Meta["witness"] = old, old
		edges = append(edges, reg, call)
	}
	facts := rebuildEventFacts(file, lang, []*graph.Node{caller, target}, edges...)
	selected, err := NewFrameworkSynthesizerSelection([]string{SynthFnPointerDispatch})
	require.NoError(t, err)
	for _, tc := range []struct {
		name    string
		reverse bool
		first   string
	}{
		{"z first", false, "unresolved::z"},
		{"a first", true, "unresolved::a"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			receipt := facts
			receipt.Edges = slices.Clone(facts.Edges)
			if tc.reverse {
				slices.Reverse(receipt.Edges)
			}
			// Keep the private key order fixed and change the SQLite row IDs.
			b := openRebuildGRPCBuild(t, facts)
			native := rebuildGRPCNative(t, rebuildEventFacts(file, lang, facts.Nodes))
			for _, e := range receipt.Edges {
				require.NoError(t, native.AddBatchChecked(nil, []*graph.Edge{e}))
			}
			require.Equal(t, rebuildEventSQLiteRows(native), rebuildEventSavedRows(t, b))
			frozen, err := FreezeRebuildFrameworkSelection(t.Context(), b.RebuildView(), selected)
			require.NoError(t, err)
			require.NoError(t, FreezeRebuildFnPointerInputs(t.Context(), b, frozen))
			report := RunFrameworkSynthesizersWithSelection(native, selected)
			require.NoError(t, report.Err)
			want := rebuildEventSQLiteRows(native)
			require.Len(t, want.edges, 2)
			for _, e := range want.edges {
				require.Equal(t, target.ID, e.To)
				require.Equal(t, tc.first, e.Meta["witness"], "%s", e.Kind)
			}
			stats, err := RunRebuildFnPointerStage(t.Context(), b, rebuildGRPCReceipts(receipt), frozen)
			require.NoError(t, err)
			require.EqualValues(t, 2, stats.RefMoves)
			require.EqualValues(t, 2, stats.CallMoves)
			require.Equal(t, want, rebuildEventSavedRows(t, b))
		})
	}
}

func TestRebuildFnPointerStageLimits(t *testing.T) {
	const file, lang, stub = "saved.c", "c", "unresolved::*.fn"
	caller := rebuildEventNode(file, "run", lang, "")
	target := rebuildEventNode(file, "seed", lang, "")
	nodes := []*graph.Node{caller, target}
	edges := []*graph.Edge{rebuildFnPointerReg(caller.ID, target.ID, 1, "s0", target.Name)}
	// All copy tuple fields except line are equal. Both native receipt order
	// and private tuple order visit the downstream hop first each sweep.
	for hop := 9; hop > 0; hop-- {
		copyRef := rebuildFnPointerReg(caller.ID, stub, 11-hop, fmt.Sprint("s", hop), "")
		copyRef.Meta["fnptr_copy_struct"], copyRef.Meta["fnptr_copy_field"] = fmt.Sprint("s", hop-1), "fn"
		edges = append(edges, copyRef)
	}
	var wantTargets []string
	for i := 64; i >= 0; i-- {
		n := rebuildEventNode(file, fmt.Sprintf("target%02d", i), lang, "")
		nodes = append(nodes, n)
		edges = append(edges, rebuildFnPointerReg(caller.ID, n.ID, 100+i, "crowd", n.Name))
		if i < 64 {
			wantTargets = append(wantTargets, n.ID)
		}
	}
	slices.Reverse(wantTargets)
	edges = append(edges,
		rebuildFnPointerCall(caller.ID, stub, 20, "s8"),
		rebuildFnPointerCall(caller.ID, stub, 21, "s9"),
		rebuildFnPointerCall(caller.ID, stub, 22, "crowd"),
	)
	facts := rebuildEventFacts(file, lang, nodes, edges...)
	b, native := openRebuildEventStores(t, facts)
	selected, err := NewFrameworkSynthesizerSelection([]string{SynthFnPointerDispatch})
	require.NoError(t, err)
	frozen, err := FreezeRebuildFrameworkSelection(t.Context(), b.RebuildView(), selected)
	require.NoError(t, err)
	require.NoError(t, FreezeRebuildFnPointerInputs(t.Context(), b, frozen))
	report := RunFrameworkSynthesizersWithSelection(native, selected)
	require.NoError(t, report.Err)
	want := rebuildEventSQLiteRows(native)
	require.Equal(t, target.ID, rebuildFnPointerSavedAt(t, want.edges, graph.EdgeCalls, caller.ID, 20).To)
	require.Equal(t, stub, rebuildFnPointerSavedAt(t, want.edges, graph.EdgeCalls, caller.ID, 21).To)
	var gotTargets []string
	for _, e := range want.edges {
		if e.Kind == graph.EdgeCalls && e.From == caller.ID && e.Line == 22 {
			gotTargets = append(gotTargets, e.To)
		}
	}
	require.Equal(t, wantTargets, gotTargets)
	stats, err := RunRebuildFnPointerStage(t.Context(), b, rebuildGRPCReceipts(facts), frozen)
	require.NoError(t, err)
	require.Equal(t, store_pebble.RebuildFnPointerStageStats{CallMoves: 2, Adds: 63, Resolved: 65, Sweeps: 8}, stats)
	require.Equal(t, want, rebuildEventSavedRows(t, b))
}

func TestRebuildFnPointerStageSuppressesOccupiedFanout(t *testing.T) {
	const file, lang = "saved.c", "c"
	caller := rebuildEventNode(file, "run", lang, "")
	primary := rebuildEventNode(file, "a", lang, "")
	secondary := rebuildEventNode(file, "b", lang, "")
	third := rebuildEventNode(file, "c", lang, "")
	destination := rebuildEventNode(file, "d", lang, "")
	nodes := []*graph.Node{caller, primary, secondary, third, destination}
	selected, err := NewFrameworkSynthesizerSelection([]string{SynthFnPointerDispatch})
	require.NoError(t, err)
	for _, tc := range []struct {
		name      string
		vacate    bool
		duplicate bool
		count     uint64
		first     string
	}{
		{name: "stationary secondaries"},
		{name: "vacated secondary", vacate: true, count: 1, first: secondary.ID},
		{name: "unproved contenders count each destination once", duplicate: true, count: 3, first: primary.ID},
	} {
		t.Run(tc.name, func(t *testing.T) {
			const line = 10
			edges := []*graph.Edge{
				rebuildFnPointerReg(caller.ID, primary.ID, 1, "slot", primary.Name),
				rebuildFnPointerReg(caller.ID, secondary.ID, 2, "slot", secondary.Name),
				rebuildFnPointerReg(caller.ID, third.ID, 3, "slot", third.Name),
			}
			var omitted []*graph.Edge
			if tc.duplicate {
				omitted = []*graph.Edge{
					rebuildFnPointerCall(caller.ID, "unresolved::z", line, "slot"),
					rebuildFnPointerCall(caller.ID, "unresolved::a", line, "slot"),
				}
			} else {
				omitted = []*graph.Edge{rebuildFnPointerCall(caller.ID, primary.ID, line, "slot")}
				edges = append(edges, &graph.Edge{Kind: graph.EdgeCalls, From: caller.ID, To: third.ID, FilePath: file, Line: line})
				if tc.vacate {
					edges = append(edges,
						rebuildFnPointerReg(caller.ID, destination.ID, 4, "vacate", destination.Name),
						rebuildFnPointerCall(caller.ID, secondary.ID, line, "vacate"),
					)
				} else {
					edges = append(edges, &graph.Edge{Kind: graph.EdgeCalls, From: caller.ID, To: secondary.ID, FilePath: file, Line: line})
				}
			}
			receipt := rebuildEventFacts(file, lang, nodes, edges...)
			facts := rebuildEventFacts(file, lang, nodes, append(slices.Clone(edges), omitted...)...)
			b, native := openRebuildEventStores(t, facts)
			before := rebuildEventSavedRows(t, b)
			frozen, err := FreezeRebuildFrameworkSelection(t.Context(), b.RebuildView(), selected)
			require.NoError(t, err)
			require.NoError(t, FreezeRebuildFnPointerInputs(t.Context(), b, frozen))
			stats, err := RunRebuildFnPointerStage(t.Context(), b, rebuildGRPCReceipts(receipt), frozen)
			if tc.count != 0 {
				var refused *store_pebble.RebuildUnknownProvenanceError
				require.ErrorAs(t, err, &refused)
				require.Equal(t, tc.count, refused.Count)
				wantFirst := graph.EdgeIdentityFor(omitted[0])
				wantFirst.To = tc.first
				require.Equal(t, wantFirst, refused.First)
				require.Zero(t, stats)
				require.Equal(t, before, rebuildEventSavedRows(t, b))
				return
			}
			require.NoError(t, err)
			require.Zero(t, stats.Adds)
			report := RunFrameworkSynthesizersWithSelection(native, selected)
			require.NoError(t, report.Err)
			require.Equal(t, rebuildEventSQLiteRows(native), rebuildEventSavedRows(t, b))
		})
	}
}
