package resolver

import (
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/graph/store_pebble"
)

func TestRebuildCFamilyWitness(t *testing.T) {
	const file = "saved.go"
	caller := rebuildEventNode(file, "run", "go", "")
	callee := rebuildEventNode(file, "work", "go", "")
	macro := &graph.Node{ID: file + "::APPLY", Name: "APPLY", Kind: graph.KindMacro, Language: "go", FilePath: file,
		Meta: map[string]any{"macro_kind": macroFunctionKindMeta}}
	reg := rebuildFnPointerReg(caller.ID, "unresolved::*.work", 1, "slot", callee.Name)
	reg.FilePath = file
	dispatch := rebuildFnPointerCall(caller.ID, "unresolved::*.fn", 2, "slot")
	dispatch.FilePath = file
	body := &graph.Edge{From: macro.ID, To: callee.ID, Kind: graph.EdgeCalls, FilePath: file, Line: 3}
	use := &graph.Edge{From: caller.ID, To: macro.ID, Kind: graph.EdgeCalls, FilePath: file, Line: 4}
	for _, stage := range []struct {
		name  string
		edges []*graph.Edge
	}{
		{SynthFnPointerDispatch, []*graph.Edge{reg, dispatch}},
		{SynthMacroExpansion, []*graph.Edge{body, use}},
	} {
		t.Run(stage.name, func(t *testing.T) {
			sel, err := NewFrameworkSynthesizerSelection([]string{stage.name})
			require.NoError(t, err)
			for _, scenario := range []string{"skip only", "wrong path", "matched receipt", "no moves"} {
				t.Run(scenario, func(t *testing.T) {
					ctx := t.Context()
					edges := stage.edges
					if scenario == "no moves" {
						edges = nil
					}
					facts := rebuildEventFacts(file, "go", []*graph.Node{caller, callee, macro}, edges...)
					build := openRebuildGRPCBuild(t, facts)
					const cf = "witness.cpp"
					witness := &graph.Node{ID: cf, Name: cf, FilePath: cf, Kind: graph.KindFile, Language: "cpp"}
					if scenario != "matched receipt" {
						witness.Meta = map[string]any{"skipped_due_to_content": true}
					}
					build.AddNode(witness)
					frozen, err := FreezeRebuildFrameworkSelection(ctx, build.RebuildView(), sel)
					require.NoError(t, err)
					require.True(t, frozen.allows(stage.name), "the live C++ label opens the native census")
					inputs := []store_pebble.ParseFacts{facts}
					switch scenario {
					case "wrong path":
						wrong := *witness
						wrong.FilePath = "different.cpp"
						inputs = append(inputs, rebuildEventFacts(wrong.FilePath, "cpp", []*graph.Node{&wrong}))
					case "matched receipt":
						inputs = append(inputs, rebuildEventFacts(cf, "cpp", []*graph.Node{witness}))
					}
					before := rebuildEventSavedRows(t, build)
					receipts := rebuildGRPCReceipts(inputs...)
					var writes uint64
					switch stage.name {
					case SynthFnPointerDispatch:
						require.NoError(t, FreezeRebuildFnPointerInputs(ctx, build, frozen))
						stats, runErr := RunRebuildFnPointerStage(ctx, build, receipts, frozen)
						writes, err = stats.RefMoves+stats.CallMoves+stats.Adds, runErr
					case SynthMacroExpansion:
						require.NoError(t, FreezeRebuildMacroExpansionInputs(ctx, build, frozen))
						stats, runErr := RunRebuildMacroExpansion(ctx, build, receipts, frozen)
						writes, err = stats.Adds, runErr
					}
					if scenario == "matched receipt" {
						require.NoError(t, err)
						require.Positive(t, writes)
						return
					}
					if scenario == "no moves" {
						require.NoError(t, err)
					} else {
						require.ErrorIs(t, err, store_pebble.ErrRebuildUnknownProvenance)
						require.ErrorContains(t, err, "lacks a C-family receipt")
					}
					require.Zero(t, writes)
					require.Equal(t, before, rebuildEventSavedRows(t, build))
				})
			}
		})
	}
}
