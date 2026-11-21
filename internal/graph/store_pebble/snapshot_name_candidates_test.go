package store_pebble

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/zzet/gortex/internal/graph"
)

func TestSnapshotNameCandidatesParity(t *testing.T) {
	nodes := []*graph.Node{
		{ID: "a", Kind: graph.KindFunction, Name: "Alpha", FilePath: "x.go", RepoPrefix: "r", Meta: map[string]any{"is_test": true}},
		{ID: "b", Kind: graph.KindFunction, Name: "Alpha", FilePath: "x.go", RepoPrefix: "r", Meta: map[string]any{"doc": "body"}},
		{ID: "c", Kind: graph.KindFunction, Name: "Alpha", FilePath: "x.go", RepoPrefix: "r"},
		{ID: "d", Kind: graph.KindFunction, Name: "Alpha", FilePath: "x.go", RepoPrefix: "other"},
		{ID: "e", Kind: graph.KindFunction, Name: "Älpha", FilePath: "x.go", RepoPrefix: "r"},
		{ID: "f", Kind: graph.KindFunction, Name: "[]alpha*", FilePath: "x.go", RepoPrefix: "r"},
	}
	sql, _, s := pairedSnapshot(t, nodes, nil)
	ctx := t.Context()
	scope := graph.LocalizationNodeScope{RepoAllow: map[string]bool{"r": true}, ExcludeTests: true, Kinds: map[graph.NodeKind]bool{graph.KindFunction: true}}
	for _, limit := range []int{1, 9} {
		want, err := sql.FindNodesByNameBounded(ctx, "Alpha", scope, limit)
		require.NoError(t, err)
		got, err := s.FindNodesByNameBounded(ctx, "Alpha", scope, limit)
		require.NoError(t, err)
		require.Equal(t, want, got)
		require.NotEmpty(t, got.Nodes)
		require.Equal(t, "b", got.Nodes[0].ID)
		require.Nil(t, got.Nodes[0].Meta)
		want, err = sql.FindFileNodesBounded(ctx, "x.go", scope, limit)
		require.NoError(t, err)
		got, err = s.FindFileNodesBounded(ctx, "x.go", scope, limit)
		require.NoError(t, err)
		require.Equal(t, want, got)
	}
	omitted, err := s.FindFileNodesBounded(ctx, "x.go", graph.LocalizationNodeScope{ExcludeFiles: map[string]bool{"x.go": true}}, 1)
	require.NoError(t, err)
	require.Empty(t, omitted)
	filter := graph.NameSearchFilter{RepoAllow: map[string]bool{"r": true}, Accept: func(n *graph.Node) bool {
		require.Empty(t, n.Meta)
		require.Empty(t, n.QualName)
		return n.ID != "a"
	}}
	for _, needle := range []string{"ALPHA", "älpha", "[]alpha*", "missing"} {
		want, err := sql.FindNodesByNameContainingFilteredContext(ctx, needle, 1, filter)
		require.NoError(t, err)
		got, err := s.FindNodesByNameContainingFilteredContext(ctx, needle, 1, filter)
		require.NoError(t, err)
		require.Equal(t, want, got)
		want, more, err := sql.FindFoldedNameCandidatesContext(ctx, needle, 1, filter)
		require.NoError(t, err)
		got, gotMore, err := s.FindFoldedNameCandidatesContext(ctx, needle, 1, filter)
		require.NoError(t, err)
		require.Equal(t, want, got)
		require.Equal(t, more, gotMore)
	}
	folded, more, err := s.FindFoldedNameCandidatesContext(ctx, "älpha", 1, filter)
	require.NoError(t, err)
	require.False(t, more)
	require.Len(t, folded, 1)
	require.Equal(t, "e", folded[0].ID)
	stopped, cancel := context.WithCancel(ctx)
	cancel()
	_, err = s.FindNodesByNameBounded(stopped, "Alpha", scope, 1)
	require.ErrorIs(t, err, context.Canceled)
	_, _, err = s.FindFoldedNameCandidatesContext(stopped, "alpha", 1, filter)
	require.ErrorIs(t, err, context.Canceled)
	require.NoError(t, s.ReadError())
}
