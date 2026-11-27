package resolver

import (
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/graph/store_pebble"
	"github.com/zzet/gortex/internal/graph/store_sqlite"
	"github.com/zzet/gortex/internal/parser"
	"github.com/zzet/gortex/internal/parser/languages"
)

func TestRebuildMainReactNativeExtractedMarker(t *testing.T) {
	t.Setenv("GORTEX_RESOLVE_FROM_RECONCILE", "1")
	const fullName = "rn::Calendar::createEvent"
	const target = "unresolved::" + fullName
	const javaFile = "Calendar.java"
	const methodID = javaFile + "::Calendar.createEvent"
	for _, tc := range []struct {
		name, file string
		extractor  parser.Extractor
	}{
		{"JavaScript miss", "app.js", languages.NewJavaScriptExtractor()},
		{"TypeScript miss", "app.ts", languages.NewTypeScriptExtractor()},
		{"full-name hit", "app.ts", languages.NewTypeScriptExtractor()},
		{"malformed target", "app.ts", languages.NewTypeScriptExtractor()},
		{"mismatched method", "app.ts", languages.NewTypeScriptExtractor()},
	} {
		t.Run(tc.name, func(t *testing.T) {
			// The extractor recognizes the inline receiver without an import.
			// Keep all its rows, including the ordinary wildcard call.
			app := rebuildRNSourceFacts(t, tc.extractor, tc.file, `function show() {
  NativeModules.Calendar.createEvent();
}
`)
			android := rebuildRNSourceFacts(t, languages.NewJavaExtractor(), javaFile, `public class Calendar {
  @ReactMethod
  public void createEvent() {}
}
`)
			var markers []*graph.Edge
			for _, e := range app.Edges {
				if e.Meta["via"] == "rn.native" {
					markers = append(markers, e)
				}
			}
			require.Len(t, markers, 1)
			marker := markers[0]
			require.Equal(t, graph.EdgeCalls, marker.Kind)
			require.Equal(t, target, marker.To)
			require.Equal(t, "Calendar", marker.Meta["rn_module"])
			require.Equal(t, "createEvent", marker.Meta["rn_method"])

			candidateID := tc.file + "::fullNameCandidate"
			switch tc.name {
			case "full-name hit":
				// This cannot be declared with JS/TS identifier syntax. Give
				// a synthetic same-file function the exact SQLite lookup Name.
				app.Nodes = append(app.Nodes, &graph.Node{
					ID:       candidateID,
					Name:     fullName,
					Kind:     graph.KindFunction,
					FilePath: tc.file,
					Language: tc.extractor.Language(),
				})
				app.File.NodeCount++
			case "malformed target":
				marker.To = "unresolved::rn::Calendar::"
			case "mismatched method":
				marker.Meta["rn_method"] = "anotherMethod"
			}

			appReceipt := roundTripRebuildRNBridgeFacts(t, app)
			androidReceipt := roundTripRebuildRNBridgeFacts(t, android)
			private := openRebuildGRPCBuild(t, appReceipt, androidReceipt)
			before := rebuildEventSavedRows(t, private)
			id := graph.EdgeIdentityFor(marker)
			wantMiss := tc.name == "JavaScript miss" || tc.name == "TypeScript miss"
			if wantMiss || tc.name == "full-name hit" {
				native := openRebuildTemporalOracle(t, app, android)
				require.Equal(t, rebuildEventSQLiteRows(native), before)
				original := native.FindEdgesByIdentities([]graph.EdgeIdentity{id})[id]
				require.NotNil(t, original)
				_, err := New(native).ResolveAllContext(t.Context())
				require.NoError(t, err)
				if wantMiss {
					require.Equal(t, original, native.FindEdgesByIdentities([]graph.EdgeIdentity{id})[id])
					checkRebuildRNSourceCuts(t, native, private, id, methodID, appReceipt, androidReceipt)
					return
				}
				bound := id
				bound.To = candidateID
				rows := native.FindEdgesByIdentities([]graph.EdgeIdentity{id, bound})
				require.NotContains(t, rows, id)
				require.Contains(t, rows, bound, "native must bind the exact non-bare Name")
			}

			require.ErrorIs(t, runRebuildRNSourceMain(t, private), errRebuildMainRule)
			require.Equal(t, before, rebuildEventSavedRows(t, private))
		})
	}
}

func rebuildRNSourceFacts(t *testing.T, extractor parser.Extractor, file, source string) store_pebble.ParseFacts {
	t.Helper()
	extracted, err := extractor.Extract(file, []byte(source))
	require.NoError(t, err)
	t.Cleanup(extracted.ReleaseTree)
	return rebuildEventFacts(file, extractor.Language(), extracted.Nodes, extracted.Edges...)
}

func runRebuildRNSourceMain(t *testing.T, private *store_pebble.BuildStore) error {
	t.Helper()
	return RunRebuildMain(t.Context(), private, store_pebble.RebuildLinkOptions{
		ScratchParent: t.TempDir(),
		SpillBytes:    1,
	}, RebuildMainInputs{
		SelectCallers: func(page []*graph.Edge) ([]*graph.Edge, error) {
			return page, nil
		},
	})
}

func checkRebuildRNSourceCuts(t *testing.T, native *store_sqlite.Store, private *store_pebble.BuildStore,
	marker graph.EdgeIdentity, methodID string, receipts ...store_pebble.ParseFacts,
) {
	t.Helper()
	main := rebuildEventSQLiteRows(native)
	require.NoError(t, runRebuildRNSourceMain(t, private))
	require.Equal(t, main, rebuildEventSavedRows(t, private), "main cut")

	selection, err := NewFrameworkSynthesizerSelection([]string{SynthReactNative})
	require.NoError(t, err)
	report := RunFrameworkSynthesizersWithSelection(native, selection)
	require.NoError(t, report.Err)
	require.Equal(t, 1, report.Total)
	bridged := marker
	bridged.To = methodID
	rows := native.FindEdgesByIdentities([]graph.EdgeIdentity{marker, bridged})
	require.Contains(t, rows, marker, "bridge must retain the extractor marker")
	require.Contains(t, rows, bridged)
	require.Equal(t, "rn.bridge", rows[bridged].Meta["via"])

	stats, err := RunRebuildReactNativeBridge(t.Context(), private, rebuildGRPCReceipts(receipts...), selection, 1)
	require.NoError(t, err)
	require.Equal(t, uint64(1), stats.Edges)
	require.Equal(t, rebuildEventSQLiteRows(native), rebuildEventSavedRows(t, private), "bridge cut")
}
