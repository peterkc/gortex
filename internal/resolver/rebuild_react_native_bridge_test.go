package resolver

import (
	"slices"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/graph/store_pebble"
)

func TestRebuildReactNativeBridgeSQLiteOracle(t *testing.T) {
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
				// The join key alone exceeds this threshold, regardless of row overhead.
				module += strings.Repeat("x", tc.spillAt)
			}
			facts := rebuildRNBridgeFacts(t, module)
			native := openRebuildTemporalOracle(t, facts...)
			private := openRebuildRNBridgePrivate(t, facts)
			before := rebuildEventSQLiteRows(native)
			require.Equal(t, before, rebuildEventSavedRows(t, private))

			app, ios, other := facts[0], facts[2], facts[3]
			first, second, existing := app.Edges[0], app.Edges[1], app.Edges[2]
			var sites []int
			for e := range native.EdgesByKind(graph.EdgeCalls) {
				if e.From == first.From && e.To == first.To {
					sites = append(sites, e.Line)
				}
			}
			require.Equal(t, []int{first.Line, second.Line}, sites)
			selection, err := NewFrameworkSynthesizerSelection([]string{SynthReactNative})
			require.NoError(t, err)
			for _, wantNew := range []uint64{2, 0} {
				report := RunFrameworkSynthesizersWithSelection(native, selection)
				require.NoError(t, report.Err)
				require.Equal(t, 3, report.Total) // Native counts the attempt on the existing key too.
				stats, err := RunRebuildReactNativeBridge(t.Context(), private, rebuildGRPCReceipts(facts...), selection, tc.spillAt)
				require.NoError(t, err)
				require.Positive(t, stats.SpillBytes)
				require.Equal(t, wantNew, stats.Edges)

				want := rebuildEventSQLiteRows(native)
				require.Equal(t, before.nodes, want.nodes)
				require.Len(t, want.edges, len(before.edges)+2)
				require.Contains(t, want.edges, existing)
				for _, n := range []*graph.Node{ios.Nodes[0], other.Nodes[0]} {
					require.Contains(t, want.edges, &graph.Edge{
						From:            first.From,
						To:              n.ID,
						Kind:            graph.EdgeCalls,
						FilePath:        first.FilePath,
						Line:            first.Line,
						Confidence:      0.6,
						ConfidenceLabel: graph.ConfidenceLabelFor(graph.EdgeCalls, 0.6),
						Origin:          graph.OriginASTInferred,
						Meta: map[string]any{
							"via":             "rn.bridge",
							"rn_module":       module,
							"rn_method":       "open",
							"native_language": n.Language,
							MetaSynthesizedBy: SynthReactNative,
							MetaProvenance:    ProvenanceHeuristic,
						},
					})
				}
				require.Equal(t, want, rebuildEventSavedRows(t, private))
			}
		})
	}
}

func TestRebuildReactNativeBridgeRejectsMissingFirstSite(t *testing.T) {
	facts := rebuildRNBridgeFacts(t, "Calendar")
	private := openRebuildRNBridgePrivate(t, facts)
	before := rebuildEventSavedRows(t, private)
	// The pending graph still has the winner. The submitted portable receipt
	// still has a valid second site, which must not be promoted to winner.
	proof := slices.Clone(facts)
	proof[0].Edges = proof[0].Edges[1:]
	proof[0] = roundTripRebuildRNBridgeFacts(t, proof[0])

	selection, err := NewFrameworkSynthesizerSelection([]string{SynthReactNative})
	require.NoError(t, err)
	stats, err := RunRebuildReactNativeBridge(t.Context(), private, rebuildGRPCReceipts(proof...), selection, 1)
	var refused *store_pebble.RebuildUnknownProvenanceError
	require.ErrorAs(t, err, &refused)
	require.Equal(t, graph.EdgeIdentityFor(facts[0].Edges[0]), refused.First)
	require.Zero(t, stats)
	require.Equal(t, before, rebuildEventSavedRows(t, private))
}

func rebuildRNBridgeFacts(t *testing.T, module string) []store_pebble.ParseFacts {
	t.Helper()
	const app, android, ios, other = "app.ts", "Calendar.java", "Calendar.m", "calendar.go"
	const caller, method = app + "::show", "open"
	placeholder := "unresolved::rn::" + module + "::" + method
	call := &graph.Edge{
		From: caller, To: placeholder, Kind: graph.EdgeCalls, FilePath: app, Line: 30,
		Meta: map[string]any{"via": "rn.native", "rn_module": module, "rn_method": method},
	}
	second := *call
	second.Line = 4
	noModule := *call
	noModule.To = "unresolved::rn::::" + method
	noModule.Line = 2
	noModule.Meta = map[string]any{"via": "rn.native", "rn_module": "", "rn_method": method}

	nativeMethod := func(file, language, name, rnMethod string) *graph.Node {
		return &graph.Node{
			ID: file + "::" + name, Name: name, Kind: graph.KindMethod, Language: language, FilePath: file,
			Meta: map[string]any{"rn_module": module, "rn_method": rnMethod},
		}
	}
	androidMethod := nativeMethod(android, "java", method, method)
	ignoredKind := nativeMethod(other, "go", "openFunction", method)
	ignoredKind.Kind = graph.KindFunction
	existing := &graph.Edge{
		From: caller, To: androidMethod.ID, Kind: graph.EdgeCalls, FilePath: app, Line: call.Line,
		Confidence: 0.9, Origin: graph.OriginASTInferred, Meta: map[string]any{"fixture": "durable"},
	}
	facts := []store_pebble.ParseFacts{
		rebuildEventFacts(app, "typescript", []*graph.Node{
			{ID: caller, Name: "show", Kind: graph.KindFunction, Language: "typescript", FilePath: app},
		}, call, &second, existing, &noModule),
		rebuildEventFacts(android, "java", []*graph.Node{androidMethod}),
		rebuildEventFacts(ios, "objc", []*graph.Node{nativeMethod(ios, "objc", method, method), nativeMethod(ios, "objc", "close", "close")}),
		rebuildEventFacts(other, "go", []*graph.Node{nativeMethod(other, "go", method, method), ignoredKind}),
	}
	for i := range facts {
		facts[i] = roundTripRebuildRNBridgeFacts(t, facts[i])
	}
	return facts
}

func openRebuildRNBridgePrivate(t *testing.T, receipts []store_pebble.ParseFacts) *store_pebble.BuildStore {
	t.Helper()
	// Keep the frozen receipt order and SQLite insertion late-then-early.
	// Reverse private insertion as a separate control; complete keys also sort
	// the lower line first. Neither private order may select the bridge site.
	pending := slices.Clone(receipts)
	pending[0].Edges = slices.Clone(pending[0].Edges)
	slices.Reverse(pending[0].Edges)
	return openRebuildGRPCBuild(t, pending...)
}

func roundTripRebuildRNBridgeFacts(t *testing.T, facts store_pebble.ParseFacts) store_pebble.ParseFacts {
	t.Helper()
	encoded, err := store_pebble.EncodeParseFacts(facts)
	require.NoError(t, err)
	decoded, err := store_pebble.DecodeParseFacts(encoded)
	require.NoError(t, err)
	return decoded
}
