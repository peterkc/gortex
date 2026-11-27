package resolver

import (
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
			require.Equal(t, target.ID, rebuildFnPointerSavedAt(t, want.edges, graph.EdgeCalls, caller.ID, 3).To)
			stats, err := RunRebuildFnPointerStage(t.Context(), b, rebuildGRPCReceipts(raw...), frozen)
			if tc.refuse {
				var refused *store_pebble.RebuildUnknownProvenanceError
				require.ErrorAs(t, err, &refused)
				require.ErrorIs(t, err, store_pebble.ErrRebuildFnPointer)
				wantFirst := graph.EdgeIdentityFor(reg)
				wantFirst.To = target.ID
				require.Equal(t, wantFirst, refused.First)
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
