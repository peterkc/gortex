package store_pebble

import (
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/zzet/gortex/internal/graph"
)

func TestRebuildClosureCollectionInstalledCustody(t *testing.T) {
	const file = "bus.swift"
	dispatch := &graph.Node{ID: "dispatch", Kind: graph.KindMethod, FilePath: file, Language: "swift",
		Meta: map[string]any{"cc_dispatch_field": "listeners"}}
	registrar := &graph.Node{ID: "registrar", Kind: graph.KindMethod, FilePath: file, Language: "swift",
		Meta: map[string]any{"cc_append_field": "listeners"}}
	makeCall := func(d, r *graph.Node, field string) *graph.Edge {
		return &graph.Edge{From: d.ID, To: r.ID, Kind: graph.EdgeCalls, FilePath: d.FilePath, Line: d.StartLine,
			Origin: graph.OriginSpeculative, Meta: map[string]any{"channel_field": field}}
	}
	b, receipts := seedSyntheticCustody(t, ParseFacts{
		EffectiveLanguage: "swift",
		File:              graph.FileMetaRow{FilePath: file},
		Nodes:             []*graph.Node{dispatch, registrar},
	})
	stats, err := b.RebuildClosureCollection(t.Context(), receipts, 1, 8,
		func(d, r *graph.Node) bool { return d.WorkspaceID == r.WorkspaceID }, makeCall)
	require.NoError(t, err)
	require.EqualValues(t, 1, stats.Edges)
	want := makeCall(dispatch, registrar, "listeners")
	live, err := b.pendingEdge(pendingEdgeKey(want))
	require.NoError(t, err)
	require.Equal(t, want, live)
	requireSyntheticCustody(t, b, map[graph.EdgeIdentity]rebuildEdgeProof{graph.EdgeIdentityFor(want): rebuildProofClosureCollection})
}
