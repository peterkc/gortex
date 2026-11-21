package store_pebble

import (
	"context"
	"slices"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/zzet/gortex/internal/graph"
)

func TestSnapshotScopeWalkParity(t *testing.T) {
	nodes := []*graph.Node{
		{ID: "a", Name: "A", Kind: graph.KindFunction, FilePath: "a.go", RepoPrefix: "r", WorkspaceID: "w"},
		{ID: "b", Name: "B", Kind: graph.KindFunction, FilePath: "b.go", RepoPrefix: "r", WorkspaceID: "w"},
		{ID: "c", Name: "C", Kind: graph.KindFunction, FilePath: "c.go", RepoPrefix: "other", WorkspaceID: "elsewhere"},
		{ID: "d", Name: "D", Kind: graph.KindFunction, FilePath: "d.go", RepoPrefix: "r", WorkspaceID: "w"},
		{ID: "e", Name: "E", Kind: graph.KindFunction, FilePath: "e.go", RepoPrefix: "r", WorkspaceID: "w", Meta: map[string]any{"external": true}},
	}
	edges := []*graph.Edge{
		{From: "a", To: "b", Kind: graph.EdgeCalls, Line: 1},
		{From: "b", To: "a", Kind: graph.EdgeCalls, Line: 2},
		{From: "a", To: "b", Kind: graph.EdgeCalls, Line: 3},
		{From: "c", To: "b", Kind: graph.EdgeReferences, Line: 4},
		{From: "b", To: "c", Kind: graph.EdgeCalls, Line: 5},
	}
	sql, _, s := pairedSnapshot(t, nodes, edges)
	ctx := t.Context()
	ids := []string{"b", "b", ""}
	wantAnc, err := sql.CallAncestorsContext(ctx, ids)
	require.NoError(t, err)
	gotAnc, err := s.CallAncestorsContext(ctx, ids)
	require.NoError(t, err)
	require.ElementsMatch(t, wantAnc, gotAnc)
	require.Len(t, gotAnc, 5)
	require.Empty(t, slices.Collect(s.NodesByKindInScope(graph.KindFunction, []string{}, graph.NodeScope{})))
	scope := graph.NodeScope{WorkspaceID: "w", RepoAllow: map[string]bool{"r": true}}
	repos := []string{"r", "r"}
	kinds := []graph.NodeKind{graph.KindFunction}
	usage := map[graph.NodeKind][]graph.EdgeKind{graph.KindFunction: {graph.EdgeCalls, graph.EdgeReferences}}
	exclusions := graph.DeadCodeCandidateExclusions{Stubs: true, External: true}
	require.Equal(t, slices.Collect(sql.NodesByKindInScope(graph.KindFunction, repos, scope)), slices.Collect(s.NodesByKindInScope(graph.KindFunction, repos, scope)))
	wantDead := sql.DeadCodeCandidatesInScope(kinds, usage, repos, scope, exclusions)
	gotDead := s.DeadCodeCandidatesInScope(kinds, usage, repos, scope, exclusions)
	require.Equal(t, wantDead, gotDead)
	require.Len(t, gotDead, 1)
	require.Equal(t, "d", gotDead[0].ID)
	masks := graph.OutlineEdgeFilter{ExcludeNodeIDs: map[string]bool{"b": true}}
	wantAnc, err = sql.CallAncestorsFilteredContext(ctx, ids, masks)
	require.NoError(t, err)
	gotAnc, err = s.CallAncestorsFilteredContext(ctx, ids, masks)
	require.NoError(t, err)
	require.Equal(t, wantAnc, gotAnc)
	require.Empty(t, gotAnc)
	wantDead, err = sql.DeadCodeCandidatesInScopeFiltered(kinds, usage, repos, scope, masks, exclusions)
	require.NoError(t, err)
	gotDead, err = s.DeadCodeCandidatesInScopeFiltered(kinds, usage, repos, scope, masks, exclusions)
	require.NoError(t, err)
	require.Equal(t, wantDead, gotDead)
	require.Len(t, gotDead, 3)
	for _, dir := range []graph.Direction{graph.DirectionForward, graph.DirectionBackward} {
		req := graph.BFSAdmissionRequest{Seed: "a", Direction: dir, Kinds: []graph.EdgeKind{graph.EdgeCalls}, Depth: 2, Limit: 9, Scope: scope}
		want, ok, err := sql.BFSAdmissionFilteredContext(ctx, req, graph.OutlineEdgeFilter{})
		require.NoError(t, err)
		require.True(t, ok)
		got, ok, err := s.BFSAdmissionFilteredContext(ctx, req, graph.OutlineEdgeFilter{})
		require.NoError(t, err)
		require.True(t, ok)
		require.Equal(t, want.Nodes, got.Nodes)
		require.Len(t, got.Adjacency, len(want.Adjacency))
		for id, adj := range want.Adjacency {
			require.Contains(t, got.Adjacency, id)
			require.Equal(t, adj.Total, got.Adjacency[id].Total)
			if dir == graph.DirectionBackward {
				// Equal-kind inbound edges use SQLite rowid or the Pebble
				// canonical identity tie breaker (D61).
				require.ElementsMatch(t, adj.Edges, got.Adjacency[id].Edges)
			} else {
				require.Equal(t, adj.Edges, got.Adjacency[id].Edges)
			}
		}
		require.Contains(t, got.Nodes, "b")
		require.NotContains(t, got.Nodes, "c")
		req.Depth = 17
		_, ok, err = s.BFSAdmissionFilteredContext(ctx, req, graph.OutlineEdgeFilter{})
		require.NoError(t, err)
		require.False(t, ok)
	}
	cancelled, cancel := context.WithCancel(ctx)
	cancel()
	gotAnc, err = s.CallAncestorsContext(cancelled, ids)
	require.ErrorIs(t, err, context.Canceled)
	require.Empty(t, gotAnc)
	require.NoError(t, s.ReadError())
}
