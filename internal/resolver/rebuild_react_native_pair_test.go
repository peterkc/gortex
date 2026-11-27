package resolver

import (
	"slices"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/graph/store_pebble"
)

func TestRebuildReactNativeNativePairingSQLiteOracle(t *testing.T) {
	selection, err := NewFrameworkSynthesizerSelection([]string{SynthReactNativePair})
	require.NoError(t, err)
	for _, tc := range []struct {
		name    string
		spillAt int
	}{
		{"one byte", 1},
		{"one MiB", 1 << 20},
	} {
		t.Run(tc.name, func(t *testing.T) {
			module := "Calendar"
			if tc.spillAt > 1 {
				// The join key itself exceeds the threshold, even without row overhead.
				module += strings.Repeat("x", tc.spillAt)
			}
			ios := []*graph.Node{
				rebuildRNPairMethod("Calendar.m", "objc", "ObjCOpen", module, "open", 3),
				rebuildRNPairMethod("Calendar.swift", "swift", "SwiftOpen", module, "open", 5),
			}
			android := []*graph.Node{
				rebuildRNPairMethod("Calendar.java", "java", "JavaOpen", module, "open", 7),
				rebuildRNPairMethod("Calendar.kt", "kotlin", "KotlinOpen", module, "open", 11),
			}
			wrongModule := rebuildRNPairMethod("Calendar.java", "java", "OtherModule", "Other", "open", 17)
			wrongMethod := rebuildRNPairMethod("Calendar.java", "java", "OtherMethod", module, "Open", 21)
			wrongKind := rebuildRNPairMethod("Calendar.java", "java", "NotMethod", module, "open", 25)
			wrongKind.Kind = graph.KindFunction
			wrongPlatform := rebuildRNPairMethod("calendar.go", "go", "GoOpen", module, "open", 31)
			stationary := &graph.Edge{
				From: ios[0].ID, To: android[0].ID, Kind: graph.EdgeReferences, FilePath: ios[0].FilePath,
				Line: ios[0].StartLine, Confidence: 0.9, Origin: graph.OriginASTInferred,
				Meta: map[string]any{"fixture": "stationary"},
			}
			facts := []store_pebble.ParseFacts{
				rebuildEventFacts(ios[0].FilePath, ios[0].Language, ios[:1], stationary),
				rebuildEventFacts(ios[1].FilePath, ios[1].Language, ios[1:]),
				rebuildEventFacts(android[0].FilePath, android[0].Language, []*graph.Node{android[0], wrongModule, wrongMethod, wrongKind}),
				rebuildEventFacts(android[1].FilePath, android[1].Language, android[1:]),
				rebuildEventFacts(wrongPlatform.FilePath, wrongPlatform.Language, []*graph.Node{wrongPlatform}),
			}
			for i := range facts {
				facts[i] = roundTripRebuildRNBridgeFacts(t, facts[i])
			}
			native := openRebuildTemporalOracle(t, facts...)
			before := rebuildEventSQLiteRows(native)
			require.Equal(t, []*graph.Edge{stationary}, before.edges)

			// The four explicit cross-platform implementation pairs yield eight
			// directions, one of which already has a durable payload at its key.
			want := rebuildEventRows{nodes: before.nodes, edges: []*graph.Edge{stationary}}
			for i, apple := range ios {
				for j, jvm := range android {
					if i != 0 || j != 0 {
						want.edges = append(want.edges, rebuildRNPairExpectedEdge(apple, jvm, module, "android"))
					}
					want.edges = append(want.edges, rebuildRNPairExpectedEdge(jvm, apple, module, "ios"))
				}
			}
			want.sort()
			for range 2 {
				report := RunFrameworkSynthesizersWithSelection(native, selection)
				require.NoError(t, report.Err)
				require.Equal(t, 4, report.Total) // Native counts pairs, not new durable rows.
				require.Equal(t, want, rebuildEventSQLiteRows(native))
			}

			for _, order := range []string{"forward", "reversed"} {
				t.Run(order, func(t *testing.T) {
					ordered := slices.Clone(facts)
					if order == "reversed" {
						slices.Reverse(ordered)
					}
					private := openRebuildGRPCBuild(t, ordered...)
					require.Equal(t, before, rebuildEventSavedRows(t, private))
					for _, wantNew := range []uint64{7, 0} {
						stats, err := RunRebuildReactNativeNativePairing(t.Context(), private, rebuildGRPCReceipts(ordered...), selection, tc.spillAt)
						require.NoError(t, err)
						if wantNew != 0 {
							require.Positive(t, stats.SpillBytes)
						}
						require.Equal(t, wantNew, stats.Edges)
						require.Equal(t, want, rebuildEventSavedRows(t, private))
					}
				})
			}
		})
	}
}

func TestRebuildReactNativeNativePairingRejectsUnprovedDeclaration(t *testing.T) {
	selection, err := NewFrameworkSynthesizerSelection([]string{SynthReactNativePair})
	require.NoError(t, err)
	apple := rebuildRNPairMethod("Calendar.m", "objc", "Open", "Calendar", "open", 3)
	target := rebuildRNPairMethod("Calendar.java", "java", "Open", "Calendar", "open", 7)
	// A separate pair sorts first. It cannot substitute for the unproved target,
	// and it must not be written even though its proof is intact.
	otherApple := rebuildRNPairMethod("Battery.m", "objc", "Open", "Battery", "open", 5)
	otherAndroid := rebuildRNPairMethod("Battery.java", "java", "Open", "Battery", "open", 11)
	unrelated := rebuildRNPairMethod(target.FilePath, target.Language, "Helper", "", "", 17)
	facts := []store_pebble.ParseFacts{
		rebuildEventFacts(otherApple.FilePath, otherApple.Language, []*graph.Node{otherApple}),
		rebuildEventFacts(otherAndroid.FilePath, otherAndroid.Language, []*graph.Node{otherAndroid}),
		rebuildEventFacts(apple.FilePath, apple.Language, []*graph.Node{apple}),
		rebuildEventFacts(target.FilePath, target.Language, []*graph.Node{target, unrelated}),
	}
	for i := range facts {
		facts[i] = roundTripRebuildRNBridgeFacts(t, facts[i])
	}
	native := openRebuildTemporalOracle(t, facts...)
	before := rebuildEventSQLiteRows(native)
	report := RunFrameworkSynthesizersWithSelection(native, selection)
	require.NoError(t, report.Err)
	require.Equal(t, 2, report.Total)
	want := rebuildEventSQLiteRows(native)
	require.Contains(t, want.edges, rebuildRNPairExpectedEdge(apple, target, "Calendar", "android"))

	for _, mode := range []string{"complete", "missing target", "changed target line"} {
		t.Run(mode, func(t *testing.T) {
			private := openRebuildGRPCBuild(t, facts...)
			require.Equal(t, before, rebuildEventSavedRows(t, private))
			proof := slices.Clone(facts)
			java := &proof[3]
			switch mode {
			case "missing target":
				java.Nodes = java.Nodes[1:]
				java.File.NodeCount = len(java.Nodes)
			case "changed target line":
				wrong := *java.Nodes[0]
				wrong.StartLine++ // Same declaration ID and RN key; changes a portable field used by the reverse edge.
				java.Nodes = []*graph.Node{&wrong, java.Nodes[1]}
			}
			*java = roundTripRebuildRNBridgeFacts(t, *java)
			stats, err := RunRebuildReactNativeNativePairing(t.Context(), private, rebuildGRPCReceipts(proof...), selection, 1)
			if mode == "complete" {
				require.NoError(t, err)
				require.Equal(t, uint64(4), stats.Edges)
				require.Equal(t, want, rebuildEventSavedRows(t, private))
				return
			}
			require.ErrorIs(t, err, store_pebble.ErrRebuildUnknownProvenance)
			require.Zero(t, stats)
			require.Equal(t, before, rebuildEventSavedRows(t, private))
		})
	}
}

func rebuildRNPairMethod(file, language, name, module, method string, line int) *graph.Node {
	return &graph.Node{
		ID: file + "::" + name, Name: name, Kind: graph.KindMethod, Language: language, FilePath: file,
		StartLine: line, EndLine: line + 2,
		Meta: map[string]any{"rn_module": module, "rn_method": method},
	}
}

func rebuildRNPairExpectedEdge(from, to *graph.Node, module, targetPlatform string) *graph.Edge {
	return &graph.Edge{
		From:            from.ID,
		To:              to.ID,
		Kind:            graph.EdgeReferences,
		FilePath:        from.FilePath,
		Line:            from.StartLine,
		Confidence:      0.6,
		ConfidenceLabel: graph.ConfidenceLabelFor(graph.EdgeReferences, 0.6),
		Origin:          graph.OriginASTInferred,
		Meta: map[string]any{
			"via":             "rn.native.pair",
			"rn_module":       module,
			"rn_method":       "open",
			"native_platform": targetPlatform,
			MetaSynthesizedBy: SynthReactNativePair,
			MetaProvenance:    ProvenanceHeuristic,
		},
	}
}
