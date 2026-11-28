package resolver

import (
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/zzet/gortex/internal/graph"
)

func TestRebuildFrameworkFamilyGateSQLiteOracle(t *testing.T) {
	web := rebuildEventNode("view.ts", "View", "typescript", "")
	webPeer := rebuildEventNode("peer.jsx", "Peer", "javascript", "")
	jvm := rebuildEventNode("Backend.java", "Backend", "java", "")
	goNode := rebuildEventNode("back.go", "Back", "go", "")
	makeEdge := func(to string, kind graph.EdgeKind, line int, synth string) *graph.Edge {
		return &graph.Edge{
			From: web.ID, To: to, Kind: kind, FilePath: web.FilePath, Line: line,
			Meta: map[string]any{MetaSynthesizedBy: synth, "payload": "preserve if kept"},
		}
	}
	b, sqlite := openRebuildEventStores(t,
		rebuildEventFacts(web.FilePath, web.Language, []*graph.Node{web},
			makeEdge(jvm.ID, graph.EdgeReferences, 1, SynthReactResolve),
			makeEdge(jvm.ID, graph.EdgeImports, 1, SynthReactResolve),
			makeEdge(jvm.ID, graph.EdgeReferences, 2, SynthReactNative),
			makeEdge(jvm.ID, graph.EdgeReferences, 3, ""),
			makeEdge(jvm.ID, graph.EdgeCalls, 1, SynthReactResolve),
			makeEdge(webPeer.ID, graph.EdgeReferences, 1, SynthReactResolve),
			makeEdge(goNode.ID, graph.EdgeImports, 1, SynthReactResolve),
		),
		rebuildEventFacts(jvm.FilePath, jvm.Language, []*graph.Node{jvm}),
		rebuildEventFacts(webPeer.FilePath, webPeer.Language, []*graph.Node{webPeer}),
		rebuildEventFacts(goNode.FilePath, goNode.Language, []*graph.Node{goNode}),
	)
	require.Equal(t, rebuildEventSQLiteRows(sqlite), rebuildEventSavedRows(t, b))
	needed, err := FreezeRebuildFrameworkFamilyGate(t.Context(), b.RebuildView(), FrameworkSynthesizerSelection{})
	require.NoError(t, err)
	require.True(t, needed)
	removed, err := RunRebuildFrameworkFamilyGate(t.Context(), b, needed)
	require.NoError(t, err)
	require.EqualValues(t, 2, removed)
	require.Equal(t, 2, applyFrameworkFamilyGate(sqlite))
	after := rebuildEventSQLiteRows(sqlite)
	require.Len(t, after.edges, 5)
	require.Equal(t, after, rebuildEventSavedRows(t, b))
	removed, err = RunRebuildFrameworkFamilyGate(t.Context(), b, needed)
	require.NoError(t, err)
	require.Zero(t, removed)
	selection, err := NewFrameworkSynthesizerSelection([]string{})
	require.NoError(t, err)
	needed, err = FreezeRebuildFrameworkFamilyGate(t.Context(), b.RebuildView(), selection)
	require.NoError(t, err)
	require.False(t, needed)
}

func TestRebuildFrameworkFamilyGateColdCensus(t *testing.T) {
	web := rebuildEventNode("view.ts", "view", "typescript", "")
	for _, tc := range []struct {
		name, language string
		replaced, want bool
	}{
		{"one strict family", "jsx", false, false},
		{"normalized second family", " JaVa ", false, true},
		{"replaced second family", "java", true, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			other := rebuildEventNode("other", "other", tc.language, "")
			b, native := openRebuildEventStores(t,
				rebuildEventFacts(web.FilePath, web.Language, []*graph.Node{web}),
				rebuildEventFacts(other.FilePath, other.Language, []*graph.Node{other}),
			)
			selection := AllFrameworkSynthesizers()
			if tc.replaced {
				require.NoError(t, b.BeginRebuildContractOverlay(t.Context(), map[string]string{other.ID: other.FilePath}))
				require.NoError(t, native.AddBatchChecked([]*graph.Node{{
					ID: other.ID, Name: other.Name, Kind: graph.KindContract, FilePath: other.FilePath, Language: "contract",
				}}, nil))
			}
			want := frameworkFamilyGateNeeded(nil, summarizeFrameworkCandidates(native, nil))
			require.Equal(t, tc.want, want)
			got, err := FreezeRebuildFrameworkFamilyGate(t.Context(), b.RebuildView(), selection)
			require.NoError(t, err)
			require.Equal(t, want, got)
			if tc.replaced {
				require.NoError(t, b.EndRebuildContractOverlay(t.Context()))
			}
		})
	}
}
