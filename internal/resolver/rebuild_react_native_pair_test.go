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
		padding int
	}{
		{"one byte", 1},
		{"one MiB", 1 << 20},
	} {
		t.Run(tc.name, func(t *testing.T) {
			module := "Calendar"
			if tc.padding > 1 {
				// Exercise a join key larger than 1 MiB.
				module += strings.Repeat("x", tc.padding)
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
			// Each empty key has a matching declaration on the opposite platform.
			// A one-sided decoy would not pair even if the empty-key check failed.
			objcNodes := []*graph.Node{
				ios[0],
				rebuildRNPairMethod(ios[0].FilePath, ios[0].Language, "EmptyModule", "", "open", 37),
				rebuildRNPairMethod(ios[0].FilePath, ios[0].Language, "EmptyMethod", module, "", 41),
			}
			javaNodes := []*graph.Node{
				android[0], wrongModule, wrongMethod, wrongKind,
				rebuildRNPairMethod(android[0].FilePath, android[0].Language, "EmptyModule", "", "open", 37),
				rebuildRNPairMethod(android[0].FilePath, android[0].Language, "EmptyMethod", module, "", 41),
			}
			facts := []store_pebble.ParseFacts{
				rebuildEventFacts(ios[0].FilePath, ios[0].Language, objcNodes, stationary),
				rebuildEventFacts(ios[1].FilePath, ios[1].Language, ios[1:]),
				rebuildEventFacts(android[0].FilePath, android[0].Language, javaNodes),
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
						stats, err := RunRebuildReactNativeNativePairing(t.Context(), private, rebuildGRPCReceipts(ordered...), selection)
						require.NoError(t, err)
						require.Equal(t, wantNew, stats.Edges)
						require.Equal(t, want, rebuildEventSavedRows(t, private))
					}
				})
			}
		})
	}
}

func TestRebuildReactNativeNativePairingStationaryMethodRowCap(t *testing.T) {
	const module, method = "Calendar", "open"
	ios := rebuildRNPairMethod("Calendar.swift", "swift", "Open", module, method, 3)
	android := rebuildRNPairMethod("Calendar.java", "java", "Open", module, method, 7)
	forward := rnNativePairEdge(ios, android, module, method)
	reverse := rnNativePairEdge(android, ios, module, method)
	forward.Meta, reverse.Meta = map[string]any{"fixture": "ios incumbent"}, map[string]any{"fixture": "android incumbent"}
	padding := strings.Repeat("x", 16<<20)
	for _, tc := range []struct {
		name          string
		oversized     *graph.Node
		occupyReverse bool
	}{
		{name: "stationary on iOS", oversized: ios, occupyReverse: true},
		{name: "stationary on Android", oversized: android, occupyReverse: true},
		{name: "missing reverse", oversized: android},
	} {
		t.Run(tc.name, func(t *testing.T) {
			apple := rebuildEventFacts(ios.FilePath, ios.Language, []*graph.Node{ios}, forward)
			jvm := rebuildEventFacts(android.FilePath, android.Language, []*graph.Node{android})
			if tc.occupyReverse {
				jvm.Edges = []*graph.Edge{reverse}
			}
			b, native := openRebuildEventStores(t, apple, jvm)
			large := *tc.oversized
			large.Meta = map[string]any{"rn_module": module, "rn_method": method, "padding": padding}
			b.AddNode(&large)
			selected, err := NewFrameworkSynthesizerSelection([]string{SynthReactNativePair})
			require.NoError(t, err)
			ctx := t.Context()
			selection, err := FreezeRebuildFrameworkSelection(ctx, b.RebuildView(), selected)
			require.NoError(t, err)
			require.True(t, selection.allows(SynthReactNativePair))
			require.NoError(t, RunFrameworkSynthesizersWithSelection(native, selected).Err)
			want := rebuildEventSQLiteRows(native).edges
			require.Len(t, want, 2)
			require.Contains(t, want, forward)
			noReceipts := func(func(store_pebble.ParseFacts, error) bool) {
				t.Fatal("stationary pairs do not need receipts; new pairs must check size before proof")
			}
			stats, err := RunRebuildReactNativeNativePairing(ctx, b, noReceipts, selection)
			require.Zero(t, stats)
			got := rebuildEventSavedRows(t, b).edges
			if tc.occupyReverse {
				require.NoError(t, err)
				require.Contains(t, want, reverse)
				require.Equal(t, want, got)
				return
			}
			require.ErrorIs(t, err, store_pebble.ErrRebuildReactNativeNativePairing)
			require.ErrorIs(t, err, store_pebble.ErrRebuildLinkInputSize)
			require.Equal(t, []*graph.Edge{forward}, got)
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
	t.Run("pairing not selected", func(t *testing.T) {
		excluded, err := NewFrameworkSynthesizerSelection([]string{SynthReactNative})
		require.NoError(t, err)
		report := RunFrameworkSynthesizersWithSelection(native, excluded)
		require.NoError(t, report.Err)
		require.Zero(t, report.Total)
		require.Equal(t, before, rebuildEventSQLiteRows(native))

		private := openRebuildGRPCBuild(t, facts...)
		stats, err := RunRebuildReactNativeNativePairing(t.Context(), private, rebuildGRPCReceipts(facts...), excluded)
		require.NoError(t, err)
		require.Zero(t, stats)
		require.Equal(t, before, rebuildEventSavedRows(t, private))
	})

	report := RunFrameworkSynthesizersWithSelection(native, selection)
	require.NoError(t, report.Err)
	require.Equal(t, 2, report.Total)
	want := rebuildEventSQLiteRows(native)
	toAndroid := rebuildRNPairExpectedEdge(apple, target, "Calendar", "android")
	toIOS := rebuildRNPairExpectedEdge(target, apple, "Calendar", "ios")
	require.Contains(t, want.edges, toAndroid)
	require.Contains(t, want.edges, toIOS)

	for _, mode := range []string{
		"complete",
		"missing target",
		"missing both endpoints",
		"stationary first direction",
		"changed target ID",
		"changed receipt language",
		"changed target language",
		"changed receipt file",
		"changed target file",
		"changed target kind",
		"changed target name",
		"changed target line",
		"changed target repo prefix",
		"changed target module",
		"changed target method",
	} {
		t.Run(mode, func(t *testing.T) {
			prior := before
			live := facts
			count := uint64(2)
			// Calendar.java sorts before Calendar.m; the separate Battery pair is proved.
			first := toIOS
			if mode == "stationary first direction" {
				live = slices.Clone(facts)
				prior.edges = []*graph.Edge{toIOS}
				live[3].Edges = prior.edges
				count, first = 1, toAndroid
			}
			private := openRebuildGRPCBuild(t, live...)
			require.Equal(t, prior, rebuildEventSavedRows(t, private))

			proof := slices.Clone(facts)
			java := &proof[3]
			*java = roundTripRebuildRNBridgeFacts(t, *java)
			wrong := java.Nodes[0]
			switch mode {
			case "missing target", "missing both endpoints", "stationary first direction":
				java.Nodes = java.Nodes[1:]
				java.File.NodeCount = len(java.Nodes)
				if mode == "missing both endpoints" {
					proof[2].Nodes = nil
					proof[2].File.NodeCount = 0
				}
			case "changed target ID":
				wrong.ID += ".other"
			case "changed receipt language":
				java.EffectiveLanguage = "kotlin"
			case "changed target language":
				// Keep the receipt consistent so this checks the live language.
				wrong.Language = "kotlin"
				java.EffectiveLanguage = wrong.Language
			case "changed receipt file":
				java.File.FilePath = "other/Calendar.java"
			case "changed target file":
				wrong.FilePath = "other/Calendar.java"
				java.File.FilePath = wrong.FilePath
			case "changed target kind":
				wrong.Kind = graph.KindFunction
			case "changed target name":
				wrong.Name = "DifferentName"
			case "changed target line":
				wrong.StartLine++
			case "changed target repo prefix":
				// The wire codec rejects RepoPrefix; exercise the direct stage input.
				wrong.RepoPrefix = "other"
			case "changed target module":
				wrong.Meta["rn_module"] = "Contacts"
			case "changed target method":
				wrong.Meta["rn_method"] = "close"
			}
			stats, err := RunRebuildReactNativeNativePairing(t.Context(), private, rebuildGRPCReceipts(proof...), selection)
			if mode == "complete" {
				require.NoError(t, err)
				require.Equal(t, uint64(4), stats.Edges)
				require.Equal(t, want, rebuildEventSavedRows(t, private))
				return
			}
			require.ErrorIs(t, err, store_pebble.ErrRebuildUnknownProvenance)
			var refused *store_pebble.RebuildUnknownProvenanceError
			require.ErrorAs(t, err, &refused)
			require.Equal(t, count, refused.Count)
			require.Equal(t, graph.EdgeIdentityFor(first), refused.First)
			require.Zero(t, stats)
			require.Equal(t, prior, rebuildEventSavedRows(t, private))
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
