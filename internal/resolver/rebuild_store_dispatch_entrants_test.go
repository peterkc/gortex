package resolver

import (
	"maps"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/graph/store_pebble"
)

func TestRebuildStoreDispatchReceiptEntrants(t *testing.T) {
	const callerFile, storeFile, extraFile = "caller.ts", "store.ts", "extra.ts"
	for _, tc := range []struct {
		synth, role string
	}{
		{SynthStoreFactory, "action cohort"},
		{SynthStoreFactory, "getter role"},
		{SynthReduxThunk, "action cohort"},
		{SynthReduxThunk, "thunk role"},
		{SynthReduxThunk, "getter role"},
		{SynthVuexDispatch, "action cohort"},
		{SynthVuexDispatch, "getter role"},
		{SynthNgRxEffect, "getter role"},
		{SynthCelery, "task cohort"},
		{SynthCelery, "getter role"},
	} {
		t.Run(tc.synth+" "+tc.role, func(t *testing.T) {
			lang := "typescript"
			if tc.synth == SynthCelery {
				lang = "python"
			}
			caller := rebuildEventNode(callerFile, "call", lang, "")
			action := func(file string) *graph.Node {
				switch tc.synth {
				case SynthVuexDispatch:
					return rebuildVuexTestNode(file, "reset", "", "reset", "action", graph.KindFunction)
				case SynthNgRxEffect:
					return rebuildEventNode(file, "reset", lang, "")
				case SynthCelery:
					return celeryTestNode(file, "reset", "reset", "")
				default:
					return rebuildStoreFactoryAction(file, "")
				}
			}
			local, winner := action(callerFile), action(storeFile)
			call := rebuildStoreFactoryMarker(callerFile, caller.ID, "retired", "reset")
			switch tc.synth {
			case SynthReduxThunk:
				call = rebuildReduxThunkMarker(caller, "reset", 9)
			case SynthVuexDispatch:
				call = rebuildVuexTestCall(caller.ID, "retired", callerFile, "reset", "action", 9)
			case SynthNgRxEffect:
				call = rebuildNgRxEffectCall(caller.ID, callerFile, "reset", 9)
			case SynthCelery:
				call = celeryTestCall(caller.ID, "retired", callerFile, "reset", "", 9)
			}
			call.Meta["store_binding"] = "useStore"
			leftNodes, rightNodes := []*graph.Node{caller}, []*graph.Node{winner}
			if tc.synth == SynthReduxThunk {
				leftNodes = append(leftNodes, rebuildReduxThunkNode(callerFile, "unrelated"))
			}
			entrant := action(extraFile)
			entrant.ID += "@other"
			raw := *entrant
			raw.Meta = maps.Clone(entrant.Meta)
			wantReceiptTarget := "unresolved::*.reset"
			switch tc.role {
			case "getter role":
				getter := rebuildEventNode(storeFile, "useStore", lang, "")
				getter.Kind = graph.KindVariable
				leftNodes, rightNodes = append(leftNodes, local), append(rightNodes, getter)
				entrant.Name, entrant.Kind = "useStore", graph.KindVariable
				entrant.Meta = map[string]any{"store_factory": false}
				raw = *entrant
				raw.Meta = nil
				wantReceiptTarget = local.ID
			case "thunk role":
				entrant.Meta["store_member"] = "other"
				raw.Meta["store_member"] = "other"
				raw.Meta["redux_thunk"] = "reset"
				wantReceiptTarget = raw.ID
			case "task cohort":
				entrant.Meta["celery_task"] = "other"
			case "action cohort":
				if tc.synth == SynthVuexDispatch {
					entrant.Meta["vuex_action"] = "other"
				} else {
					entrant.Meta["store_member"] = "other"
				}
			}
			left := rebuildEventFacts(callerFile, lang, leftNodes, call)
			right := rebuildEventFacts(storeFile, lang, rightNodes)
			live := rebuildEventFacts(extraFile, lang, []*graph.Node{entrant})
			changed := rebuildEventFacts(extraFile, lang, []*graph.Node{&raw})
			b := openRebuildGRPCBuild(t, left, right, live)
			before := rebuildEventSavedRows(t, b)
			selection, err := NewFrameworkSynthesizerSelection([]string{tc.synth})
			require.NoError(t, err)
			frozen, err := FreezeRebuildFrameworkSelection(t.Context(), b.RebuildView(), selection)
			require.NoError(t, err)
			require.True(t, frozen.allows(tc.synth))
			apply := func(facts store_pebble.ParseFacts) (uint64, error) {
				receipts := rebuildGRPCReceipts(left, right, facts)
				switch tc.synth {
				case SynthStoreFactory:
					stats, err := RunRebuildStoreFactory(t.Context(), b, receipts, frozen)
					return stats.Moves, err
				case SynthReduxThunk:
					stats, err := RunRebuildReduxThunk(t.Context(), b, receipts, frozen)
					return stats.Moves, err
				case SynthVuexDispatch:
					stats, err := RunRebuildVuexDispatch(t.Context(), b, receipts, frozen)
					return stats.Moves, err
				case SynthNgRxEffect:
					stats, err := RunRebuildNgRxEffect(t.Context(), b, receipts, frozen)
					return stats.Moves, err
				default:
					stats, err := RunRebuildCelery(t.Context(), b, receipts, frozen)
					return stats.Moves, err
				}
			}
			native := openRebuildTemporalOracle(t, left, right, changed)
			report := RunFrameworkSynthesizersWithSelection(native, selection)
			require.NoError(t, report.Err)
			nativeRows := rebuildEventSQLiteRows(native)
			require.Len(t, nativeRows.edges, 1)
			require.Equal(t, wantReceiptTarget, nativeRows.edges[0].To)

			moves, err := apply(changed)
			first := graph.EdgeIdentityFor(call)
			first.To = winner.ID
			var refusal *store_pebble.RebuildUnknownProvenanceError
			require.ErrorAs(t, err, &refusal)
			require.Equal(t, &store_pebble.RebuildUnknownProvenanceError{Count: 1, First: first}, refusal)
			require.Zero(t, moves)
			require.Equal(t, before, rebuildEventSavedRows(t, b))

			moves, err = apply(live)
			require.NoError(t, err)
			require.Equal(t, uint64(1), moves)
			saved := rebuildEventSavedRows(t, b)
			require.Len(t, saved.edges, 1)
			require.Equal(t, winner.ID, saved.edges[0].To)
		})
	}
}
