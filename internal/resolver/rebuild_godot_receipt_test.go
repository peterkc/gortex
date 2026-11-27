package resolver

import (
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/graph/store_pebble"
)

func TestRebuildGodotAutoloadReceiptElection(t *testing.T) {
	for _, scenario := range []string{"competing manifest", "member name", "member kind", "receipt file", "extractor language", "call key", "shared raw claim"} {
		t.Run(scenario, func(t *testing.T) {
			facts, call, method := rebuildGodotAutoloadFacts()
			receipts := append([]store_pebble.ParseFacts(nil), facts...)
			switch scenario {
			case "competing manifest":
				const file = "other/project.godot"
				live := *facts[0].Nodes[0]
				live.ID, live.FilePath, live.Kind = file+"::Clock", file, graph.KindVariable
				raw := live
				raw.Kind = graph.KindType
				raw.Meta = map[string]any{"gd_kind": "autoload", "gd_script": "other/Clock.gd"}
				facts = append(facts, rebuildEventFacts(file, "godot_project", []*graph.Node{&live}))
				receipts = append(receipts, rebuildEventFacts(file, "godot_project", []*graph.Node{&raw}))
			case "member name", "member kind":
				live := *method
				live.ID = method.FilePath + "::0-tick"
				if scenario == "member name" {
					live.Name = "unrelated"
				} else {
					live.Kind = graph.KindFunction
				}
				raw := live
				raw.Name, raw.Kind = method.Name, method.Kind
				facts[1] = rebuildEventFacts(method.FilePath, "gdscript", append(facts[1].Nodes, &live))
				receipts[1] = rebuildEventFacts(method.FilePath, "gdscript", append(receipts[1].Nodes[:3:3], &raw))
			case "receipt file":
				receipts[0].File.FilePath = "not-project.godot"
			case "extractor language":
				receipts[0].EffectiveLanguage = "gdscript-other"
			case "call key":
				raw := *call
				raw.Meta = map[string]any{"receiver_type": "Different"}
				receipts[2].Edges = []*graph.Edge{&raw}
			}
			b := openRebuildGRPCBuild(t, facts...)
			if scenario == "shared raw claim" {
				seedGodotSharedCallClaim(t, b, call)
			}
			before := rebuildEventSavedRows(t, b)
			count, err := RunRebuildGodotAutoload(t.Context(), b, rebuildGRPCReceipts(receipts...), rebuildGodotAutoloadSelection(t, b))
			require.ErrorIs(t, err, store_pebble.ErrRebuildGodotAutoload)
			require.ErrorIs(t, err, store_pebble.ErrRebuildUnknownProvenance)
			require.Zero(t, count)
			require.Equal(t, before, rebuildEventSavedRows(t, b))
		})
	}
}

func TestRebuildGodotPreloadReceiptElection(t *testing.T) {
	const source, script, language = "game/main.gd", "game/Clock.gd", "gdscript"
	caller := rebuildEventNode(source, "caller", language, "")
	alias := rebuildEventNode(source, "Clock", language, "")
	alias.Kind = graph.KindVariable
	alias.Meta = map[string]any{"gd_preload": "res://Clock.gd"}
	call := &graph.Edge{
		From: caller.ID, To: "unresolved::*.tick", Kind: graph.EdgeCalls, FilePath: source, Line: 4,
		Meta: map[string]any{"receiver_type": alias.Name},
	}
	scriptFile := &graph.Node{ID: script, Kind: graph.KindFile, Language: language, FilePath: script}
	member := rebuildEventNode(script, "tick", language, "")
	member.Kind = graph.KindMethod
	selection, err := NewFrameworkSynthesizerSelection([]string{SynthGodotPreloadAlias})
	require.NoError(t, err)
	for _, scenario := range []string{"receipt-only alias", "missing script receipt", "missing member receipt", "self preload", "disabled", "shared raw claim"} {
		t.Run(scenario, func(t *testing.T) {
			sourceAlias := *alias
			if scenario == "receipt-only alias" {
				sourceAlias.Meta = nil
			}
			if scenario == "self preload" {
				sourceAlias.Meta = map[string]any{"gd_preload": "res://main.gd"}
			}
			rootFile := &graph.Node{ID: source, Kind: graph.KindFile, Language: language, FilePath: source}
			localMember := rebuildEventNode(source, "tick", language, "")
			localMember.Kind = graph.KindMethod
			facts := []store_pebble.ParseFacts{
				rebuildEventFacts(source, language, []*graph.Node{caller, &sourceAlias, rootFile, localMember}, call),
				rebuildEventFacts(script, language, []*graph.Node{scriptFile, member}),
			}
			receipts := append([]store_pebble.ParseFacts(nil), facts...)
			selected := selection
			switch scenario {
			case "receipt-only alias":
				receipts[0].Nodes = []*graph.Node{caller, alias, rootFile, localMember}
			case "missing script receipt":
				receipts[1].Nodes = []*graph.Node{member}
			case "missing member receipt":
				receipts[1].Nodes = []*graph.Node{scriptFile}
			case "disabled":
				disabled, err := NewFrameworkSynthesizerSelection(nil)
				require.NoError(t, err)
				selected = disabled
			}
			b, native := openRebuildEventStores(t, facts...)
			if scenario == "shared raw claim" {
				seedGodotSharedCallClaim(t, b, call)
			}
			before := rebuildEventSavedRows(t, b)
			count, err := RunRebuildGodotPreloadAlias(t.Context(), b, rebuildGRPCReceipts(receipts...), selected)
			require.Zero(t, count)
			require.Equal(t, before, rebuildEventSavedRows(t, b))
			switch scenario {
			case "self preload":
				require.NoError(t, err)
				require.Zero(t, ResolveGDScriptPreloadAliases(native))
				require.Equal(t, before, rebuildEventSQLiteRows(native))
			case "disabled":
				require.NoError(t, err)
			default:
				require.ErrorIs(t, err, store_pebble.ErrRebuildGodotPreloadAlias)
				require.ErrorIs(t, err, store_pebble.ErrRebuildUnknownProvenance)
			}
		})
	}
}

func seedGodotSharedCallClaim(t *testing.T, b *store_pebble.BuildStore, raw *graph.Edge) {
	t.Helper()
	mover := *raw
	mover.To = "unresolved::tick" // same native Call key, different identity
	require.NoError(t, b.ApplyLinkReindexes(t.Context(), []graph.EdgeReindex{{Edge: &mover, OldTo: raw.To}}))
	stationary := *raw
	stationary.Meta = map[string]any{"incumbent": true}
	b.AddEdge(&stationary)
}
