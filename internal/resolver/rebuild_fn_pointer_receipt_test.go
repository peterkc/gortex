package resolver

import (
	"fmt"
	"slices"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/graph/store_pebble"
)

// Native reads store_binding only when a registration name has several
// candidates. A raw receipt whose binding differs from the live row proves a
// copy or a singleton registration; it must still refuse an election.
func TestRebuildFnPointerStageBindingOnlyForElection(t *testing.T) {
	const file, targetFile, lang = "saved.c", "target.c", "c"
	const stub = "unresolved::*.fn"
	caller := rebuildEventNode(file, "run", lang, "")
	homonym := rebuildEventNode(file, "work", lang, "")
	target := rebuildEventNode(targetFile, homonym.Name, lang, "")
	getter := &graph.Node{ID: targetFile + "::getSlot", Name: "getSlot", Kind: graph.KindVariable, Language: lang, FilePath: targetFile}
	selected, err := NewFrameworkSynthesizerSelection([]string{SynthFnPointerDispatch})
	require.NoError(t, err)
	for _, tc := range []struct {
		name      string
		viaCopy   bool // the binding is on a copy into the dispatch slot
		elected   bool // a caller-file homonym lets the getter file decide
		live, raw string
		refuse    bool
	}{
		{"copy", true, false, getter.Name, "other", false},
		{"singleton", false, false, getter.Name, "", false},
		{"election control", false, true, getter.Name, getter.Name, false},
		{"election", false, true, getter.Name, "", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			facts := func(binding string) (*graph.Edge, []store_pebble.ParseFacts) {
				reg := rebuildFnPointerReg(caller.ID, "unresolved::*.work", 1, "root", target.Name)
				bound, slot := reg, "root"
				edges := []*graph.Edge{reg}
				if tc.viaCopy {
					bound, slot = rebuildFnPointerReg(caller.ID, stub, 2, "branch", ""), "branch"
					bound.Meta["fnptr_copy_struct"], bound.Meta["fnptr_copy_field"] = "root", "fn"
					edges = append(edges, bound)
				}
				if binding != "" {
					bound.Meta["store_binding"] = binding
				}
				edges = append(edges, rebuildFnPointerCall(caller.ID, stub, 3, slot))
				nodes := []*graph.Node{caller}
				if tc.elected {
					nodes = append(nodes, homonym)
				}
				return reg, []store_pebble.ParseFacts{
					rebuildEventFacts(file, lang, nodes, edges...),
					rebuildEventFacts(targetFile, lang, []*graph.Node{target, getter}),
				}
			}
			reg, live := facts(tc.live)
			_, raw := facts(tc.raw)
			b, native := openRebuildEventStores(t, live...)
			before := rebuildEventSavedRows(t, b)
			frozen, err := FreezeRebuildFrameworkSelection(t.Context(), b.RebuildView(), selected)
			require.NoError(t, err)
			require.NoError(t, FreezeRebuildFnPointerInputs(t.Context(), b, frozen))
			report := RunFrameworkSynthesizersWithSelection(native, selected)
			require.NoError(t, report.Err)
			want := rebuildEventSQLiteRows(native)
			call := rebuildFnPointerSavedAt(t, want.edges, graph.EdgeCalls, caller.ID, 3)
			require.Equal(t, target.ID, call.To)
			stats, err := RunRebuildFnPointerStage(t.Context(), b, rebuildGRPCReceipts(raw...), frozen)
			if tc.refuse {
				require.Equal(t, target.ID, rebuildFnPointerSavedAt(t, want.edges, graph.EdgeReferences, reg.From, reg.Line).To)
				rawNative := rebuildGRPCNative(t, raw[0])
				require.NoError(t, rawNative.AddBatchChecked(raw[1].Nodes, raw[1].Edges))
				rawReport := RunFrameworkSynthesizersWithSelection(rawNative, selected)
				require.NoError(t, rawReport.Err)
				rawRows := rebuildEventSQLiteRows(rawNative)
				require.Equal(t, homonym.ID, rebuildFnPointerSavedAt(t, rawRows.edges, graph.EdgeReferences, reg.From, reg.Line).To)
				require.Equal(t, homonym.ID, rebuildFnPointerSavedAt(t, rawRows.edges, graph.EdgeCalls, caller.ID, 3).To)

				var refused *store_pebble.RebuildUnknownProvenanceError
				require.ErrorAs(t, err, &refused)
				require.ErrorIs(t, err, store_pebble.ErrRebuildFnPointer)
				// Count both Reference destinations and the affected Call. The
				// Call's destination key sorts first for the same source.
				require.Equal(t, &store_pebble.RebuildUnknownProvenanceError{Count: 3, First: graph.EdgeIdentityFor(call)}, refused)
				require.Zero(t, stats)
				require.Equal(t, before, rebuildEventSavedRows(t, b))
				return
			}
			require.NoError(t, err)
			require.EqualValues(t, 1, stats.CallMoves)
			require.Equal(t, want, rebuildEventSavedRows(t, b))
		})
	}
}

// The census admits a row by via alone. A row with no slot has no role, so
// native skips it and its size must not refuse; a row with a role still does.
func TestRebuildFnPointerStageCapsOnlyRoleRows(t *testing.T) {
	const file, lang = "saved.c", "c"
	const stub = "unresolved::*.fn"
	caller := rebuildEventNode(file, "run", lang, "")
	handler := rebuildEventNode(file, "work", lang, "")
	large := strings.Repeat("x", 16<<20)
	selected, err := NewFrameworkSynthesizerSelection([]string{SynthFnPointerDispatch})
	require.NoError(t, err)
	// Keep a failed comparison readable. The large context still compares by value.
	shrink := func(rows rebuildEventRows) rebuildEventRows {
		for _, e := range rows.edges {
			if e.Context == large {
				e.Context = "<16 MiB>"
			}
		}
		return rows
	}
	seed := func() *graph.Edge {
		return rebuildFnPointerReg(caller.ID, "unresolved::*.work", 1, "root", handler.Name)
	}
	for _, tc := range []struct {
		name    string
		kind    graph.EdgeKind
		role    bool
		context string
	}{
		{"via-only Reference", graph.EdgeReferences, false, large},
		{"via-only Call", graph.EdgeCalls, false, large},
		{"small Reference role", graph.EdgeReferences, true, ""},
		{"small Call role", graph.EdgeCalls, true, ""},
		{"oversized Reference role", graph.EdgeReferences, true, large},
		{"oversized Call role", graph.EdgeCalls, true, large},
	} {
		t.Run(tc.name, func(t *testing.T) {
			extra := rebuildFnPointerCall(caller.ID, stub, 3, "root")
			if tc.kind == graph.EdgeReferences {
				extra = rebuildFnPointerReg(caller.ID, handler.ID, 3, "root", handler.Name)
			}
			if !tc.role {
				extra.Meta = map[string]any{"via": extra.Meta["via"]}
			}
			extra.Context = tc.context
			// The extra row is a receipt too, so replay reads its live row.
			facts := rebuildEventFacts(file, lang, []*graph.Node{caller, handler},
				seed(), rebuildFnPointerCall(caller.ID, stub, 2, "root"), extra)
			b := openRebuildGRPCBuild(t, facts)
			before := rebuildEventSavedRows(t, b)
			frozen, err := FreezeRebuildFrameworkSelection(t.Context(), b.RebuildView(), selected)
			require.NoError(t, err)
			require.NoError(t, FreezeRebuildFnPointerInputs(t.Context(), b, frozen))
			stats, err := RunRebuildFnPointerStage(t.Context(), b, rebuildGRPCReceipts(facts), frozen)
			if tc.role && tc.context != "" {
				require.ErrorIs(t, err, store_pebble.ErrRebuildFnPointer)
				require.ErrorIs(t, err, store_pebble.ErrRebuildLinkInputSize)
				require.ErrorContains(t, err, "function pointer edge row")
				require.Zero(t, stats)
				require.Equal(t, shrink(before), shrink(rebuildEventSavedRows(t, b)))
				return
			}
			require.NoError(t, err)
			// Only the private store needs the cap-sized payload. First
			// verify the skipped row kept it, then compare with SQLite.
			got := rebuildEventSavedRows(t, b)
			saved := rebuildFnPointerSavedAt(t, got.edges, tc.kind, caller.ID, 3)
			require.True(t, saved.Context == tc.context, "saved context changed")
			saved.Context = ""
			oracleExtra := *extra
			oracleExtra.Context = ""
			oracle := rebuildEventFacts(file, lang, facts.Nodes, facts.Edges[0], facts.Edges[1], &oracleExtra)
			native := rebuildGRPCNative(t, oracle)
			report := RunFrameworkSynthesizersWithSelection(native, selected)
			require.NoError(t, report.Err)
			want := rebuildEventSQLiteRows(native)
			require.Equal(t, handler.ID, rebuildFnPointerSavedAt(t, want.edges, graph.EdgeCalls, caller.ID, 2).To)
			require.Equal(t, want, got)
		})
	}

	// A large via-only row that shares a raw key still claims it before the
	// role check, so the live dispatch refuses for provenance, not size.
	t.Run("via-only raw claim", func(t *testing.T) {
		ctx := t.Context()
		call := rebuildFnPointerCall(caller.ID, stub, 2, "root")
		facts := rebuildEventFacts(file, lang, []*graph.Node{caller, handler}, seed(), call)
		b := openRebuildGRPCBuild(t, facts)
		derived := *call
		derived.To = "unresolved::alternate"
		require.NoError(t, b.ApplyLinkReindexes(ctx, []graph.EdgeReindex{{Edge: &derived, OldTo: call.To}}))
		old := *call
		old.Meta = map[string]any{"via": call.Meta["via"]}
		old.Context = large
		b.AddEdge(&old)
		before := rebuildEventSavedRows(t, b)
		frozen, err := FreezeRebuildFrameworkSelection(ctx, b.RebuildView(), selected)
		require.NoError(t, err)
		require.NoError(t, FreezeRebuildFnPointerInputs(ctx, b, frozen))
		stats, err := RunRebuildFnPointerStage(ctx, b, rebuildGRPCReceipts(facts), frozen)
		require.ErrorIs(t, err, store_pebble.ErrRebuildUnknownProvenance)
		require.NotErrorIs(t, err, store_pebble.ErrRebuildLinkInputSize)
		require.Zero(t, stats)
		require.Equal(t, shrink(before), shrink(rebuildEventSavedRows(t, b)))
	})
}

// A receipt may put a callable in an otherwise empty registration cohort.
// Native then enters the dispatch loop and resets even an unrelated slot.
func TestRebuildFnPointerStagePossibleSeedResetsUnrelatedSlot(t *testing.T) {
	const file, lang, stub = "saved.c", "c", "unresolved::*.fn"
	caller := rebuildEventNode(file, "run", lang, "")
	current := rebuildEventNode(file, "current", lang, "")
	seed := rebuildEventNode(file, "seed", lang, "")
	variable := *seed
	variable.Kind = graph.KindVariable
	reg := rebuildFnPointerReg(caller.ID, seed.ID, 1, "root", seed.Name)
	selected, err := NewFrameworkSynthesizerSelection([]string{SynthFnPointerDispatch})
	require.NoError(t, err)
	for _, tc := range []struct {
		name    string
		to      string
		rawSeed bool
		refuse  bool
	}{
		{name: "no seed in either graph", to: current.ID},
		{name: "possible seed resets settled call", to: current.ID, rawSeed: true, refuse: true},
		{name: "possible seed leaves unresolved call", to: stub, rawSeed: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := t.Context()
			call := rebuildFnPointerCall(caller.ID, tc.to, 2, "unrelated")
			live := rebuildEventFacts(file, lang, []*graph.Node{caller, current, &variable}, reg, call)
			raw := live
			want := tc.to
			if tc.rawSeed {
				raw.Nodes = []*graph.Node{caller, current, seed}
				want = stub
			}
			calls := rebuildFnPointerReceiptCalls(t, raw, selected)
			require.Len(t, calls, 1)
			require.Equal(t, want, calls[0].To)

			b := openRebuildGRPCBuild(t, live)
			before := rebuildEventSavedRows(t, b)
			frozen, err := FreezeRebuildFrameworkSelection(ctx, b.RebuildView(), selected)
			require.NoError(t, err)
			require.NoError(t, FreezeRebuildFnPointerInputs(ctx, b, frozen))
			stats, err := RunRebuildFnPointerStage(ctx, b, rebuildGRPCReceipts(raw), frozen)
			if tc.refuse {
				var refused *store_pebble.RebuildUnknownProvenanceError
				require.ErrorIs(t, err, store_pebble.ErrRebuildFnPointer)
				require.ErrorAs(t, err, &refused)
				require.Equal(t, &store_pebble.RebuildUnknownProvenanceError{Count: 1, First: graph.EdgeIdentityFor(call)}, refused)
			} else {
				require.NoError(t, err)
			}
			require.Zero(t, stats)
			require.Equal(t, before, rebuildEventSavedRows(t, b))
		})
	}
}

// A different receipt role can introduce a target into the source of an
// unproved copy. The proved fanout is unchanged, but the possible one grows.
func TestRebuildFnPointerStageReceiptFanoutComparison(t *testing.T) {
	const file, lang, stub = "saved.c", "c", "unresolved::*.fn"
	caller := rebuildEventNode(file, "run", lang, "")
	primary := rebuildEventNode(file, "a", lang, "")
	secondary := rebuildEventNode(file, "b", lang, "")
	third := rebuildEventNode(file, "c", lang, "")
	nodes := []*graph.Node{caller, primary, secondary, third}
	selected, err := NewFrameworkSynthesizerSelection([]string{SynthFnPointerDispatch})
	require.NoError(t, err)
	for _, tc := range []struct {
		name        string
		includeCopy bool
	}{
		{name: "unchanged secondary"},
		{name: "possible additional secondary", includeCopy: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := t.Context()
			call := rebuildFnPointerCall(caller.ID, primary.ID, 5, "slot")
			liveReg := rebuildFnPointerReg(caller.ID, third.ID, 3, "elsewhere", third.Name)
			rawReg := rebuildFnPointerReg(caller.ID, third.ID, 3, "source", third.Name)
			live := rebuildEventFacts(file, lang, nodes,
				rebuildFnPointerReg(caller.ID, primary.ID, 1, "slot", primary.Name),
				rebuildFnPointerReg(caller.ID, secondary.ID, 2, "slot", secondary.Name),
				liveReg, call)
			raw := live
			raw.Edges = slices.Clone(live.Edges)
			raw.Edges[2] = rawReg
			if tc.includeCopy {
				copyRef := rebuildFnPointerReg(caller.ID, stub, 4, "slot", "")
				copyRef.Meta["fnptr_copy_struct"], copyRef.Meta["fnptr_copy_field"] = "source", "fn"
				live.Edges = append(live.Edges, copyRef)
				// If the omitted copy belonged to the raw world too, native
				// would add a third target without changing the primary.
				possible := raw
				possible.Edges = append(slices.Clone(raw.Edges), copyRef)
				calls := rebuildFnPointerReceiptCalls(t, possible, selected)
				require.Len(t, calls, 3)
				slices.SortFunc(calls, rebuildGRPCCallLess)
				require.Equal(t, primary.ID, calls[0].To)
				require.Equal(t, secondary.ID, calls[1].To)
				require.Equal(t, third.ID, calls[2].To)
			}
			calls := rebuildFnPointerReceiptCalls(t, raw, selected)
			require.Len(t, calls, 2)
			slices.SortFunc(calls, rebuildGRPCCallLess)
			require.Equal(t, call, calls[0])
			require.Equal(t, secondary.ID, calls[1].To)

			b, native := openRebuildEventStores(t, live)
			before := rebuildEventSavedRows(t, b)
			frozen, err := FreezeRebuildFrameworkSelection(ctx, b.RebuildView(), selected)
			require.NoError(t, err)
			require.NoError(t, FreezeRebuildFnPointerInputs(ctx, b, frozen))
			stats, err := RunRebuildFnPointerStage(ctx, b, rebuildGRPCReceipts(raw), frozen)
			if tc.includeCopy {
				var refused *store_pebble.RebuildUnknownProvenanceError
				require.ErrorIs(t, err, store_pebble.ErrRebuildFnPointer)
				require.ErrorAs(t, err, &refused)
				want := graph.EdgeIdentityFor(call)
				want.To = third.ID
				require.Equal(t, &store_pebble.RebuildUnknownProvenanceError{Count: 1, First: want}, refused)
				require.Zero(t, stats)
				require.Equal(t, before, rebuildEventSavedRows(t, b))
				return
			}
			require.NoError(t, err)
			require.Equal(t, store_pebble.RebuildFnPointerStageStats{Adds: 1, Resolved: 2, Sweeps: 1}, stats)
			report := RunFrameworkSynthesizersWithSelection(native, selected)
			require.NoError(t, report.Err)
			require.Equal(t, rebuildEventSQLiteRows(native), rebuildEventSavedRows(t, b))
		})
	}
}

func TestRebuildFnPointerStageCFamilyMarkerNodeCap(t *testing.T) {
	const file, lang = "saved.js", "javascript"
	caller := rebuildEventNode(file, "run", lang, "")
	target := rebuildEventNode(file, "work", lang, "")
	reg := rebuildFnPointerReg(caller.ID, "unresolved::*.work", 1, "slot", target.Name)
	call := rebuildFnPointerCall(caller.ID, "unresolved::*.fn", 2, "slot")
	reg.FilePath, call.FilePath = file, file
	marker := &graph.Node{ID: "a.c", Kind: graph.KindFile, Language: "c", FilePath: "a.c"}
	spare := &graph.Node{ID: "b.c", Kind: graph.KindFile, Language: "c", FilePath: "b.c"}
	facts := []store_pebble.ParseFacts{
		rebuildEventFacts(file, lang, []*graph.Node{caller, target}, reg, call),
		rebuildEventFacts(marker.FilePath, marker.Language, []*graph.Node{marker}),
		rebuildEventFacts(spare.FilePath, spare.Language, []*graph.Node{spare}),
	}
	selected, err := NewFrameworkSynthesizerSelection([]string{SynthFnPointerDispatch})
	require.NoError(t, err)
	for _, count := range []int{2, 3} {
		t.Run(fmt.Sprintf("%d receipts", count), func(t *testing.T) {
			ctx := t.Context()
			b, native := openRebuildEventStores(t, facts...)
			large := *marker
			large.Meta = map[string]any{"padding": strings.Repeat("x", 16<<20)}
			b.AddNode(&large)
			frozen, err := FreezeRebuildFrameworkSelection(ctx, b.RebuildView(), selected)
			require.NoError(t, err)
			require.True(t, frozen.allows(SynthFnPointerDispatch))
			require.NoError(t, FreezeRebuildFnPointerInputs(ctx, b, frozen))
			stats, err := RunRebuildFnPointerStage(ctx, b, rebuildGRPCReceipts(facts[:count]...), frozen)
			var edges []*graph.Edge
			require.NoError(t, b.RebuildView().VisitEdges(ctx, func(e *graph.Edge) error {
				edges = append(edges, e)
				return nil
			}))
			if count == 2 {
				require.ErrorIs(t, err, store_pebble.ErrRebuildFnPointer)
				require.ErrorIs(t, err, store_pebble.ErrRebuildLinkInputSize)
				require.ErrorContains(t, err, "function pointer C-family marker node")
				require.Zero(t, stats)
				require.ElementsMatch(t, facts[0].Edges, edges)
				return
			}
			require.NoError(t, err)
			require.Equal(t, store_pebble.RebuildFnPointerStageStats{RefMoves: 1, CallMoves: 1, Resolved: 1, Sweeps: 1}, stats)
			report := RunFrameworkSynthesizersWithSelection(native, selected)
			require.NoError(t, report.Err)
			want := rebuildEventSQLiteRows(native)
			require.Equal(t, target.ID, rebuildFnPointerSavedAt(t, want.edges, graph.EdgeReferences, caller.ID, reg.Line).To)
			require.Equal(t, target.ID, rebuildFnPointerSavedAt(t, want.edges, graph.EdgeCalls, caller.ID, call.Line).To)
			require.ElementsMatch(t, want.edges, edges)
		})
	}
}
