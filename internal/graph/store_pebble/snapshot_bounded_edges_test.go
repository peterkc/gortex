package store_pebble

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/zzet/gortex/internal/graph"
)

func TestSnapshotBoundedEdgesParity(t *testing.T) {
	nodes := []*graph.Node{
		{ID: "a", Name: "a", Kind: graph.KindFunction},
		{ID: "b", Name: "b", Kind: graph.KindFunction},
		{ID: "c", Name: "c", Kind: graph.KindFunction},
		{ID: "d", Name: "d", Kind: graph.KindFunction},
	}
	edges := []*graph.Edge{
		{From: "a", To: "b", Kind: graph.EdgeCalls, Line: 1, FilePath: "x.go", Meta: map[string]any{"evidence": "literal", "semantic_source": "go"}},
		{From: "a", To: "b", Kind: graph.EdgeCalls, Line: 2, FilePath: "x.go"},
		{From: "a", To: "c", Kind: graph.EdgeReferences, Line: 3, FilePath: "x.go"},
		{From: "a", To: "d", Kind: graph.EdgeContains, Line: 4, FilePath: "x.go"},
		{From: "c", To: "b", Kind: graph.EdgeCalls, Line: 5, FilePath: "y.go"},
	}
	sql, b, s := pairedSnapshot(t, nodes, edges)
	ctx := t.Context()
	present := graph.TypedEdgeEndpoint{From: "a", To: "b", Kind: graph.EdgeCalls}
	missing := graph.TypedEdgeEndpoint{From: "b", To: "a", Kind: graph.EdgeCalls}
	endpoints := []graph.TypedEdgeEndpoint{present, {}, missing, present}
	wantFound, err := sql.FindExistingEdgeEndpoints(ctx, endpoints, 2)
	require.NoError(t, err)
	found, err := s.FindExistingEdgeEndpoints(ctx, endpoints, 2)
	require.NoError(t, err)
	require.Equal(t, wantFound, found)
	require.Len(t, found, 1)
	require.Contains(t, found, present)
	_, err = s.FindExistingEdgeEndpoints(ctx, endpoints, 1)
	require.ErrorAs(t, err, new(*graph.BoundedLocalizationLimitError))

	keys := []string{"b", "a", "", "missing", "b"}
	kinds := []graph.EdgeKind{graph.EdgeReferences, graph.EdgeCalls, graph.EdgeCalls, ""}
	sites := []graph.EdgeSourceSite{{From: "a", Line: 2}, {From: "a", Line: 1}, {From: "a", Line: 1}, {From: "missing", Line: 3}, {Line: 1}}
	for _, limit := range []int{0, 1, 3} {
		wp, err := sql.FindIncomingSourcesBounded(ctx, keys, graph.EdgeCalls, limit)
		require.NoError(t, err)
		gp, err := s.FindIncomingSourcesBounded(ctx, keys, graph.EdgeCalls, limit)
		require.NoError(t, err)
		require.Equal(t, wp, gp)
		if limit == 0 {
			continue
		}
		wOut, err := sql.FindOutgoingEdgeIdentitiesBounded(ctx, keys, kinds, limit)
		require.NoError(t, err)
		gOut, err := s.FindOutgoingEdgeIdentitiesBounded(ctx, keys, kinds, limit)
		require.NoError(t, err)
		require.Equal(t, wOut, gOut)
		wIn, err := sql.FindIncomingEdgeIdentitiesBounded(ctx, keys, kinds, limit)
		require.NoError(t, err)
		gIn, err := s.FindIncomingEdgeIdentitiesBounded(ctx, keys, kinds, limit)
		require.NoError(t, err)
		require.Equal(t, wIn, gIn)
		wSite, err := sql.FindOutgoingSiteEdgeIdentitiesBounded(ctx, sites, kinds, limit)
		require.NoError(t, err)
		gSite, err := s.FindOutgoingSiteEdgeIdentitiesBounded(ctx, sites, kinds, limit)
		require.NoError(t, err)
		require.Equal(t, wSite, gSite)
		require.Len(t, gSite.BySite[graph.EdgeSourceSite{From: "a", Line: 2}], 1)
	}
	_, err = s.FindOutgoingSiteEdgeIdentitiesBounded(ctx, sites, kinds, 0)
	require.ErrorAs(t, err, new(*graph.BoundedLocalizationLimitError))
	_, err = s.FindIncomingEdgeIdentitiesBounded(ctx, make([]string, graph.MaxBoundedAdjacencyKeys+1), kinds, 1)
	require.ErrorAs(t, err, new(*graph.BoundedLocalizationLimitError))

	outgoing := []string{"a", "", "a", "missing"}
	wantCSR, err := sql.GetCallReferenceOutEdgesContext(ctx, outgoing)
	require.NoError(t, err)
	gotCSR, err := s.GetCallReferenceOutEdgesContext(ctx, outgoing)
	require.NoError(t, err)
	require.Equal(t, wantCSR, gotCSR)
	require.Len(t, gotCSR["a"], 3)
	require.Equal(t, "literal", gotCSR["a"][0].Meta["evidence"])
	require.Equal(t, "go", gotCSR["a"][0].Meta["semantic_source"])
	for _, limit := range []int{0, 4} {
		want, truncWant, err := sql.GetOutEdgesByNodeIDsWithMetadataContext(ctx, outgoing, limit)
		require.NoError(t, err)
		got, truncGot, err := s.GetOutEdgesByNodeIDsWithMetadataContext(ctx, outgoing, limit)
		require.NoError(t, err)
		require.Equal(t, truncWant, truncGot)
		// Metadata lookup has no SQLite sort guarantee. Compare all complete rows.
		require.ElementsMatch(t, want["a"], got["a"])
	}
	bounded, truncated, err := s.GetOutEdgesByNodeIDsWithMetadataContext(ctx, outgoing, 1)
	require.NoError(t, err)
	require.True(t, truncated)
	require.Len(t, bounded["a"], 1)
	require.True(t, b.RemoveEdge("a", "b", graph.EdgeCalls))
	require.NoError(t, b.Publish(ctx))
	still, err := s.FindExistingEdgeEndpoints(ctx, []graph.TypedEdgeEndpoint{present}, 1)
	require.NoError(t, err)
	require.Contains(t, still, present)
	gone, err := snapshot(t, b).FindExistingEdgeEndpoints(ctx, []graph.TypedEdgeEndpoint{present}, 1)
	require.NoError(t, err)
	require.Empty(t, gone)
	stopped, cancel := context.WithCancel(ctx)
	cancel()
	stoppedResult, err := s.FindOutgoingEdgeIdentitiesBounded(stopped, keys, kinds, 1)
	require.ErrorIs(t, err, context.Canceled)
	require.Empty(t, stoppedResult)
	require.NoError(t, s.ReadError())
}
