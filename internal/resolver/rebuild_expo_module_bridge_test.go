package resolver

import (
	"fmt"
	"slices"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/graph/store_pebble"
	"github.com/zzet/gortex/internal/parser/languages"
)

func TestRebuildExpoModuleBridgeSQLiteOracle(t *testing.T) {
	facts := rebuildExpoModuleBridgeFacts(t)
	native := openRebuildTemporalOracle(t, facts...)
	pending := slices.Clone(facts)
	pending[0].Edges = slices.Clone(pending[0].Edges)
	slices.Reverse(pending[0].Edges)
	private := openRebuildGRPCBuild(t, pending...)
	before := rebuildEventSQLiteRows(native)
	require.Equal(t, before, rebuildEventSavedRows(t, private))

	first, incumbent := facts[0].Edges[2], facts[0].Edges[4]
	var sites []int
	for e := range native.EdgesByKind(graph.EdgeCalls) {
		if e.From == first.From && e.To == first.To && e.Meta["via"] == "rn.native" {
			sites = append(sites, e.Line)
		}
	}
	require.Equal(t, []int{30, 4}, sites)
	require.Contains(t, before.edges, incumbent)

	disabled, err := NewFrameworkSynthesizerSelection(nil)
	require.NoError(t, err)
	report := RunFrameworkSynthesizersWithSelection(native, disabled)
	require.NoError(t, report.Err)
	require.Zero(t, report.Total)
	stats, err := RunRebuildExpoModuleBridge(t.Context(), private, rebuildGRPCReceipts(facts...), disabled, 1)
	require.NoError(t, err)
	require.Zero(t, stats)
	require.Equal(t, before, rebuildEventSQLiteRows(native))
	require.Equal(t, before, rebuildEventSavedRows(t, private))

	want := rebuildEventRows{nodes: before.nodes, edges: slices.Clone(before.edges)}
	for _, target := range []struct{ id, language string }{
		{"ios/MathModule.swift::expo:Math:add", "swift"},
		{"android/MathModule.kt::expo:Math:add", "kotlin"},
	} {
		want.edges = append(want.edges, &graph.Edge{
			From:            first.From,
			To:              target.id,
			Kind:            graph.EdgeCalls,
			FilePath:        "app.ts",
			Line:            30,
			Confidence:      0.6,
			ConfidenceLabel: graph.ConfidenceLabelFor(graph.EdgeCalls, 0.6),
			Origin:          graph.OriginASTInferred,
			Meta: map[string]any{
				"via":             "expo.bridge",
				"expo_module":     "Math",
				"expo_method":     "add",
				"native_language": target.language,
				MetaSynthesizedBy: SynthExpoModules,
				MetaProvenance:    ProvenanceHeuristic,
			},
		})
	}
	want.sort()
	selection, err := NewFrameworkSynthesizerSelection([]string{SynthExpoModules})
	require.NoError(t, err)
	for _, wantNew := range []uint64{2, 0} {
		report := RunFrameworkSynthesizersWithSelection(native, selection)
		require.NoError(t, report.Err)
		require.Equal(t, 3, report.Total) // Native counts the incumbent attempt too.
		require.Equal(t, want, rebuildEventSQLiteRows(native))
		stats, err := RunRebuildExpoModuleBridge(t.Context(), private, rebuildGRPCReceipts(facts...), selection, 1)
		require.NoError(t, err)
		require.Equal(t, wantNew, stats.Edges)
		if wantNew != 0 {
			require.Positive(t, stats.SpillBytes)
		}
		require.Equal(t, want, rebuildEventSavedRows(t, private))
	}
}

func TestRebuildExpoModuleBridgeFamilyGroups(t *testing.T) {
	required, family := expoBridgeFamilyMask()
	for _, tc := range []struct {
		name      string
		languages []string
		want      bool
	}{
		{"web and Apple", []string{"TypeScript", " Swift "}, true},
		{"web and JVM", []string{"tsx", "Java"}, true},
		{"web only", []string{"javascript", "go"}, false},
		{"Apple and JVM without web", []string{"swift", "kotlin", "go"}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			present := map[string]int{}
			var seen uint8
			for _, language := range tc.languages {
				seen |= family(language)
				present[frameworkLanguageFamily(language)]++
			}
			require.Equal(t, tc.want, frameworkSynthNodeGatesPass(SynthExpoModules, present, nil))
			require.Equal(t, tc.want, seen&required == required)
		})
	}
}

func TestRebuildExpoModuleBridgePreservesReactNativeCollision(t *testing.T) {
	caller := &graph.Node{ID: "app.ts::open", Kind: graph.KindFunction, Name: "open", FilePath: "app.ts", Language: "typescript"}
	target := &graph.Node{ID: "native.swift::open", Kind: graph.KindMethod, Name: "open", FilePath: "native.swift", Language: "swift",
		Meta: map[string]any{"rn_module": "Widget", "rn_method": "open", "expo_module": "Widget", "expo_method": "open"}}
	marker := &graph.Edge{From: caller.ID, To: "unresolved::rn::Widget::open", Kind: graph.EdgeCalls, FilePath: caller.FilePath, Line: 4,
		Meta: map[string]any{"via": "rn.native", "rn_module": "Widget", "rn_method": "open"}}
	facts := []store_pebble.ParseFacts{
		rebuildEventFacts(caller.FilePath, caller.Language, []*graph.Node{caller}, marker),
		rebuildEventFacts(target.FilePath, target.Language, []*graph.Node{target}),
	}
	native := openRebuildTemporalOracle(t, facts...)
	private := openRebuildGRPCBuild(t, facts...)
	before := rebuildEventSavedRows(t, private)
	require.Equal(t, rebuildEventSQLiteRows(native), before)
	selection, err := NewFrameworkSynthesizerSelection([]string{SynthReactNative, SynthExpoModules})
	require.NoError(t, err)
	frozen, err := FreezeRebuildFrameworkSelection(t.Context(), private.RebuildView(), selection)
	require.NoError(t, err)
	require.True(t, frozen.allows(SynthReactNative))
	require.True(t, frozen.allows(SynthExpoModules))

	report := RunFrameworkSynthesizersWithSelection(native, selection)
	require.NoError(t, report.Err)
	require.Equal(t, 2, report.Total) // Both passes try the same key.
	want := rebuildEventSQLiteRows(native)
	require.Len(t, want.edges, len(before.edges)+1)
	var winner *graph.Edge
	for _, e := range want.edges {
		if e.To == target.ID {
			winner = e
		}
	}
	require.NotNil(t, winner)
	require.Equal(t, rnBridgeVia, winner.Meta["via"])
	require.Equal(t, SynthReactNative, winner.Meta[MetaSynthesizedBy])

	rn, err := RunRebuildReactNativeBridge(t.Context(), private, rebuildGRPCReceipts(facts...), frozen, 1)
	require.NoError(t, err)
	require.EqualValues(t, 1, rn.Edges)
	require.Equal(t, want, rebuildEventSavedRows(t, private))
	expo, err := RunRebuildExpoModuleBridge(t.Context(), private, rebuildGRPCReceipts(facts...), frozen, 1)
	require.NoError(t, err)
	require.Zero(t, expo.Edges)
	require.Equal(t, want, rebuildEventSavedRows(t, private))
}

func TestRebuildExpoModuleBridgeRejectsUnprovedParticipants(t *testing.T) {
	facts := rebuildExpoModuleBridgeFacts(t)
	selection, err := NewFrameworkSynthesizerSelection([]string{SynthExpoModules})
	require.NoError(t, err)
	for _, name := range []string{"missing Swift receipt", "changed Swift method", "missing first raw site"} {
		t.Run(name, func(t *testing.T) {
			private := openRebuildGRPCBuild(t, facts...)
			before := rebuildEventSavedRows(t, private)
			proof := slices.Clone(facts)
			// Kotlin sorts before Swift and still has proof. Refusal must not
			// leave its new row or change the incumbent.
			switch name {
			case "missing Swift receipt":
				proof = slices.Delete(proof, 1, 2)
			case "changed Swift method":
				proof[1] = roundTripRebuildRNBridgeFacts(t, proof[1])
				for _, n := range proof[1].Nodes {
					if n.ID == "ios/MathModule.swift::expo:Math:add" {
						n.Meta["expo_method"] = "subtract"
					}
				}
				proof[1] = roundTripRebuildRNBridgeFacts(t, proof[1])
			case "missing first raw site":
				proof[0].Edges = slices.Delete(slices.Clone(proof[0].Edges), 2, 3)
				proof[0] = roundTripRebuildRNBridgeFacts(t, proof[0])
			}
			stats, err := RunRebuildExpoModuleBridge(t.Context(), private, rebuildGRPCReceipts(proof...), selection, 1)
			require.ErrorIs(t, err, store_pebble.ErrRebuildExpoModuleBridge)
			var refused *store_pebble.RebuildUnknownProvenanceError
			require.ErrorAs(t, err, &refused)
			firstSite := facts[0].Edges[2]
			want := store_pebble.RebuildUnknownProvenanceError{Count: 1, First: graph.EdgeIdentityFor(firstSite)}
			want.First.To = "ios/MathModule.swift::expo:Math:add"
			if name == "missing first raw site" {
				// Lost receipt order affects all three pairs, even the incumbent.
				want.Count = 3
				want.First.To = "android/MathModule.kt::expo:Math:add"
			}
			require.Equal(t, &want, refused)
			require.Zero(t, stats)
			require.Equal(t, before, rebuildEventSavedRows(t, private))
		})
	}
}

func TestRebuildExpoModuleBridgeSQLiteSpillAtMiB(t *testing.T) {
	const extraTargets = 12
	facts := rebuildExpoModuleBridgeFacts(t)
	const file, lang = "overflow.kt", "kotlin"
	nodes := make([]*graph.Node, 0, extraTargets)
	pad := strings.Repeat("x", 32<<10)
	for i := range extraTargets {
		nodes = append(nodes, &graph.Node{
			ID: fmt.Sprintf("%s::target%d%s", file, i, pad), Name: "add", Kind: graph.KindMethod, FilePath: file, Language: lang,
			Meta: map[string]any{"expo_module": "Math", "expo_method": "add"},
		})
	}
	facts = append(facts, rebuildEventFacts(file, lang, nodes))
	private := openRebuildGRPCBuild(t, facts...)
	native := openRebuildTemporalOracle(t, facts...)
	require.Equal(t, rebuildEventSQLiteRows(native), rebuildEventSavedRows(t, private))
	selection, err := NewFrameworkSynthesizerSelection([]string{SynthExpoModules})
	require.NoError(t, err)
	report := RunFrameworkSynthesizersWithSelection(native, selection)
	require.NoError(t, report.Err)
	require.Equal(t, 3+extraTargets, report.Total)
	stats, err := RunRebuildExpoModuleBridge(t.Context(), private, rebuildGRPCReceipts(facts...), selection, 1<<20)
	require.NoError(t, err)
	require.EqualValues(t, 2+extraTargets, stats.Edges)
	require.Positive(t, stats.SpillBytes)
	require.Equal(t, rebuildEventSQLiteRows(native), rebuildEventSavedRows(t, private))
}

func rebuildExpoModuleBridgeFacts(t *testing.T) []store_pebble.ParseFacts {
	t.Helper()
	const app, ios, android, extra = "app.ts", "ios/MathModule.swift", "android/MathModule.kt", "extra.kt"
	ts, err := languages.NewTypeScriptExtractor().Extract(app, []byte(`import { requireNativeModule } from 'expo-modules-core';
const NativeMath = requireNativeModule('Math');
export function add() {
  NativeMath.add(1, 2);
`+strings.Repeat("\n", 25)+`  NativeMath.add(3, 4);
}
`))
	require.NoError(t, err)
	t.Cleanup(ts.ReleaseTree)
	swift, err := languages.NewSwiftExtractor().Extract(ios, []byte(`import ExpoModulesCore

public class MathModule: Module {
  public func definition() -> ModuleDefinition {
    Name("Math")
    Function("add") { (a: Int, b: Int) -> Int in
      return a + b
    }
  }
}
`))
	require.NoError(t, err)
	t.Cleanup(swift.ReleaseTree)
	kotlin, err := languages.NewKotlinExtractor().Extract(android, []byte(`package expo.modules.math

import expo.modules.kotlin.modules.Module
import expo.modules.kotlin.modules.ModuleDefinition

class MathModule : Module() {
  override fun definition() = ModuleDefinition {
    Name("Math")
    AsyncFunction("add") { a: Int, b: Int -> a + b }
    Function("subtract") { a: Int, b: Int -> a - b }
  }
}
`))
	require.NoError(t, err)
	t.Cleanup(kotlin.ReleaseTree)

	var early, late *graph.Edge
	for _, e := range ts.Edges {
		if e.Kind != graph.EdgeCalls || e.To != "unresolved::rn::Math::add" || e.Meta["via"] != "rn.native" {
			continue
		}
		switch e.Line {
		case 4:
			require.Nil(t, early)
			early = e
		case 30:
			require.Nil(t, late)
			late = e
		default:
			t.Fatalf("unexpected Expo marker: %+v", e)
		}
	}
	require.NotNil(t, early)
	require.NotNil(t, late)
	require.Equal(t, late.From, early.From)

	incumbentTarget := &graph.Node{
		ID: extra + "::expo:Math:add", Name: "add", Kind: graph.KindMethod, FilePath: extra, Language: "kotlin",
		Meta: map[string]any{"expo_module": "Math", "expo_method": "add"},
	}
	wrongKindTarget := *incumbentTarget
	wrongKindTarget.ID, wrongKindTarget.Kind = extra+"::property", graph.KindField
	incumbent := &graph.Edge{
		From: late.From, To: incumbentTarget.ID, Kind: graph.EdgeCalls, FilePath: app, Line: 30,
		Confidence: 0.9, Origin: graph.OriginASTInferred, Meta: map[string]any{"fixture": "keep incumbent"},
	}
	wrongKind := *late
	wrongKind.Kind, wrongKind.Line = graph.EdgeReferences, 2
	wrongKind.To = incumbentTarget.ID
	wrongTag := *late
	wrongTag.Line = 3
	wrongTag.Meta = map[string]any{"via": "not-rn.native", "rn_module": "Math", "rn_method": "add"}

	// Raw native order puts an ineligible kind and tag before the two sites.
	// The first eligible Call remains line 30, even when private insertion is reversed.
	edges := []*graph.Edge{&wrongKind, &wrongTag, late, early, incumbent}
	for _, e := range ts.Edges {
		if e != late && e != early {
			edges = append(edges, e)
		}
	}
	facts := []store_pebble.ParseFacts{
		rebuildEventFacts(app, "typescript", ts.Nodes, edges...),
		rebuildEventFacts(ios, "swift", swift.Nodes, swift.Edges...),
		rebuildEventFacts(android, "kotlin", kotlin.Nodes, kotlin.Edges...),
		rebuildEventFacts(extra, "kotlin", []*graph.Node{incumbentTarget, &wrongKindTarget}),
	}
	for i := range facts {
		facts[i] = roundTripRebuildRNBridgeFacts(t, facts[i])
	}
	return facts
}
