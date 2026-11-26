package store_pebble

import (
	"context"
	"fmt"
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

func TestSnapshotNameGramIntersectionBounds(t *testing.T) {
	nodes := []*graph.Node{
		{ID: "a", Kind: graph.KindFunction, Name: "ABC-bcd", RepoPrefix: "repo"}, // both grams, not the substring
		{ID: "b", Kind: graph.KindFunction, Name: "xAbCd", RepoPrefix: "repo"},
		{ID: "c", Kind: graph.KindFunction, Name: "ABCD", RepoPrefix: "repo/sub"},
		{ID: "d", Kind: graph.KindFunction, Name: "ABAB"},
		{ID: "e", Kind: graph.KindFunction, Name: "ABABA"},
		{ID: "f", Kind: graph.KindFunction, Name: "[ÄλÄ]"},
	}
	for i := range 40 {
		nodes = append(nodes, &graph.Node{ID: fmt.Sprintf("0-noise-%02d", i), Kind: graph.KindFunction, Name: "abc-only", RepoPrefix: "repo"})
	}
	sql, _, s := pairedSnapshot(t, nodes, nil)
	reads := trackSnapshotReads(s)
	key := func(id string) string { return string(keyStrings([]byte{familyNode}, id)) }
	ctx := t.Context()
	for _, limit := range []int{1, 0, -1} {
		reads.points = nil
		want := sql.FindNodesByNameContaining("aBcD", limit)
		got := s.FindNodesByNameContaining("aBcD", limit)
		require.Equal(t, want, got)
		wantReads := []string{key("a"), key("b"), key("c")}
		if limit == 1 {
			wantReads = wantReads[:2]
		}
		require.Equal(t, wantReads, reads.points)
	}
	filter := graph.NameSearchFilter{RepoAllow: map[string]bool{"repo": true}}
	reads.points = nil
	want, err := sql.FindNodesByNameContainingFilteredContext(ctx, "abcd", 1, filter)
	require.NoError(t, err)
	got, err := s.FindNodesByNameContainingFilteredContext(ctx, "abcd", 1, filter)
	require.NoError(t, err)
	require.Equal(t, want, got)
	require.Equal(t, []string{key("a"), key("b"), key("b")}, reads.points)
	reads.points = nil
	want, more, err := sql.FindFoldedNameCandidatesContext(ctx, "abcd", 1, graph.NameSearchFilter{})
	require.NoError(t, err)
	got, gotMore, err := s.FindFoldedNameCandidatesContext(ctx, "abcd", 1, graph.NameSearchFilter{})
	require.NoError(t, err)
	require.Equal(t, want, got)
	require.Equal(t, more, gotMore)
	require.Equal(t, []string{key("a"), key("b"), key("c"), key("c")}, reads.points)
	for _, needle := range []string{"a", "ab", "ababa", "[", "missing", ""} {
		require.Equal(t, sql.FindNodesByNameContaining(needle, 0), s.FindNodesByNameContaining(needle, 0))
	}
	unicodeMatch := s.FindNodesByNameContaining("äλä", 0)
	require.Len(t, unicodeMatch, 1)
	require.Equal(t, "f", unicodeMatch[0].ID)
	reads.points = nil
	require.Empty(t, s.FindNodesByNameContaining("abc-not-found", 0))
	require.Empty(t, reads.points)
	stopped, cancel := context.WithCancel(ctx)
	cancel()
	_, err = s.FindNodesByNameContainingFilteredContext(stopped, "abcd", 1, filter)
	require.ErrorIs(t, err, context.Canceled)
	_, _, err = s.FindFoldedNameCandidatesContext(ctx, "abcd", 0, filter)
	require.Error(t, err)
	require.Empty(t, reads.points)
	require.NoError(t, s.ReadError())
}
