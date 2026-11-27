package store_pebble

import (
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/zzet/gortex/internal/graph"
)

func TestRebuildExpoModuleBridgeCensusAndCustody(t *testing.T) {
	caller := &graph.Node{ID: "app.ts::open", Kind: graph.KindFunction, Name: "open", FilePath: "app.ts", Language: "typescript"}
	target := &graph.Node{
		ID: "extra.go::Open", Kind: graph.KindMethod, Name: "Open", FilePath: "extra.go", Language: "go",
		Meta: map[string]any{"expo_module": "Widget", "expo_method": "open"},
	}
	// Native checks via, not whether To is unresolved. Keep the trigger
	// distinct from the Expo output by using an unrelated concrete target.
	marker := &graph.Edge{
		From: caller.ID, To: "app.ts::original", Kind: graph.EdgeCalls, FilePath: caller.FilePath, Line: 21,
		Meta: map[string]any{"via": "rn.native", "rn_module": "Widget", "rn_method": "open"},
	}
	b, receipts := seedSyntheticCustody(t,
		ParseFacts{EffectiveLanguage: caller.Language, File: graph.FileMetaRow{FilePath: caller.FilePath},
			Nodes: []*graph.Node{caller}, Edges: []*graph.Edge{marker}},
		ParseFacts{EffectiveLanguage: target.Language, File: graph.FileMetaRow{FilePath: target.FilePath}, Nodes: []*graph.Node{target}})
	family := func(language string) uint8 {
		switch language {
		case "typescript":
			return 1
		case "swift":
			return 2
		default:
			return 0
		}
	}
	isCall := func(e *graph.Edge) bool { return e.Kind == graph.EdgeCalls && e.Meta["via"] == "rn.native" }
	makeCall := func(e *graph.Edge, n *graph.Node) *graph.Edge {
		return &graph.Edge{From: e.From, To: n.ID, Kind: graph.EdgeCalls, FilePath: e.FilePath, Line: e.Line,
			Origin: graph.OriginASTInferred}
	}
	ctx := t.Context()
	stats, err := b.RebuildExpoModuleBridge(ctx, receipts, 1, family, isCall, makeCall)
	require.NoError(t, err)
	require.Zero(t, stats.Edges)

	// An unrelated native-family marker admits the pass, not just the
	// marker's own declarations. The tagged Go target can then win.
	b.AddNode(&graph.Node{ID: "marker.swift", Kind: graph.KindFile, FilePath: "marker.swift", Language: "swift"})
	stats, err = b.RebuildExpoModuleBridge(ctx, receipts, 1, family, isCall, makeCall)
	require.NoError(t, err)
	require.EqualValues(t, 1, stats.Edges)
	want := makeCall(marker, target)
	live, err := b.pendingEdge(pendingEdgeKey(want))
	require.NoError(t, err)
	require.Equal(t, want, live)
	requireSyntheticCustody(t, b, map[graph.EdgeIdentity]rebuildEdgeProof{graph.EdgeIdentityFor(want): rebuildProofExpoModules})
}
