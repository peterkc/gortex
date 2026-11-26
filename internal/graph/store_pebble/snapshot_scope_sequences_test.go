package store_pebble

import (
	"fmt"
	"slices"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/zzet/gortex/internal/graph"
)

func TestSnapshotScopeSequencesParity(t *testing.T) {
	nodes := []*graph.Node{
		{ID: "a", Kind: graph.KindFunction, Name: "A", FilePath: "shared.go", RepoPrefix: "one", Meta: map[string]any{"doc": "payload"}},
		{ID: "b", Kind: graph.KindFunction, Name: "B", FilePath: "shared.go", RepoPrefix: "two"},
		{ID: "c", Kind: graph.KindFile, Name: "other.go", FilePath: "other.go", RepoPrefix: "one", WorkspaceID: "w", Language: "go"},
		{ID: "d", Kind: graph.KindFile, Name: "shared.go", FilePath: "shared.go", Language: "go"},
	}
	edges := []*graph.Edge{
		{From: "a", To: "c", Kind: graph.EdgeCalls, FilePath: "shared.go", Line: 1},
		{From: "a", To: "c", Kind: graph.EdgeCalls, FilePath: "shared.go", Line: 4},
		{From: "c", To: "a", Kind: graph.EdgeCalls, FilePath: "other.go", Line: 5},
	}
	sql, _, s := pairedSnapshot(t, nodes, edges)
	for _, repos := range [][]string{nil, {"one"}, {"two", "one", "one", ""}, {"missing"}} {
		for _, files := range [][]string{nil, {"shared.go", "shared.go"}, {"missing.go"}} {
			require.Equal(t, slices.Collect(sql.NodesInScopeSeq(repos, files)), slices.Collect(s.NodesInScopeSeq(repos, files)))
			require.Equal(t, slices.Collect(sql.NodesLightInScopeSeq(repos, files)), slices.Collect(s.NodesLightInScopeSeq(repos, files)))
			kinds := []graph.NodeKind{graph.KindFile, "", graph.KindFunction, graph.KindFile}
			require.Equal(t, slices.Collect(sql.NodesLightByKindsInScopeSeq(repos, files, kinds...)), slices.Collect(s.NodesLightByKindsInScopeSeq(repos, files, kinds...)))
		}
		require.Equal(t, slices.Collect(sql.FileLocationsInScopeSeq(repos)), slices.Collect(s.FileLocationsInScopeSeq(repos)))
		require.Equal(t, sql.RepoNodesLight(repos), s.RepoNodesLight(repos))
	}
	wantNodes, wantEdges := sql.GetFileSubGraph("shared.go")
	gotNodes, gotEdges := s.GetFileSubGraph("shared.go")
	require.ElementsMatch(t, wantNodes, gotNodes)
	require.ElementsMatch(t, wantEdges, gotEdges)
	countNodes, count := s.GetFileSubGraphCounts("shared.go")
	require.Len(t, wantEdges, 2)
	require.Equal(t, len(wantEdges), count)
	require.Equal(t, gotNodes, countNodes)
	countNodes, count = s.GetFileSubGraphCounts("missing.go")
	require.Empty(t, countNodes)
	require.Zero(t, count)
	seen := 0
	for n := range s.NodesInScopeSeq([]string{"one", "two"}, nil) {
		require.NotNil(t, s.GetNode(n.ID))
		seen++
		break
	}
	require.Equal(t, 1, seen)
	require.NoError(t, s.ReadError())
}

func TestSnapshotScopeKindReadsOnlyIntersection(t *testing.T) {
	nodes := []*graph.Node{
		{ID: "a", Name: "a.go", Kind: graph.KindFile, RepoPrefix: "repo", FilePath: "nested/a.go"},
		{ID: "c", Name: "a.go", Kind: graph.KindFile, RepoPrefix: "repo/sub", FilePath: "nested/a.go"},
		{ID: "b", Name: "b.go", Kind: graph.KindFile, RepoPrefix: "other"},
	}
	for i := range 40 {
		nodes = append(nodes, &graph.Node{ID: fmt.Sprintf("noise-%02d", i), Name: "noise", Kind: graph.KindFunction, RepoPrefix: "repo", FilePath: "nested/a.go"})
	}
	sql, _, s := pairedSnapshot(t, nodes, nil)
	reads := trackSnapshotReads(s)
	repos := []string{"repo/sub", "repo", "repo"}
	kinds := []graph.NodeKind{graph.KindFile, "", graph.KindFile}
	wantKeys := []string{string(keyStrings([]byte{familyNode}, "a")), string(keyStrings([]byte{familyNode}, "c"))}
	for _, files := range [][]string{nil, {"nested/a.go", "nested/a.go"}} {
		reads.points = nil
		require.Equal(t, slices.Collect(sql.NodesInScopeSeq(repos, files, kinds...)), slices.Collect(s.NodesInScopeSeq(repos, files, kinds...)))
		require.Equal(t, wantKeys, reads.points)
		reads.points = nil
		require.Equal(t, slices.Collect(sql.NodesLightByKindsInScopeSeq(repos, files, kinds...)), slices.Collect(s.NodesLightByKindsInScopeSeq(repos, files, kinds...)))
		require.Equal(t, wantKeys, reads.points)
	}
	reads.points = nil
	require.Equal(t, slices.Collect(sql.FileLocationsInScopeSeq(repos)), slices.Collect(s.FileLocationsInScopeSeq(repos)))
	require.Equal(t, wantKeys, reads.points)
	reads.points = nil
	require.ElementsMatch(t, slices.Collect(sql.NodesByKindInRepos(graph.KindFile, repos)), slices.Collect(s.NodesByKindInRepos(graph.KindFile, repos)))
	require.ElementsMatch(t, wantKeys, reads.points)
	reads.points = nil
	for n := range s.NodesInScopeSeq(repos, nil, kinds...) {
		require.Equal(t, "a", n.ID)
		break
	}
	require.Equal(t, wantKeys[:1], reads.points)
	reads.points = nil
	require.Empty(t, slices.Collect(s.NodesInScopeSeq([]string{"missing"}, nil, kinds...)))
	require.Empty(t, slices.Collect(s.NodesInScopeSeq(repos, nil, graph.NodeKind(""))))
	require.Empty(t, reads.points)
	require.NoError(t, s.ReadError())
}

func TestSnapshotScopeKindRefusesOffGroup(t *testing.T) {
	b := newBuild(t, DefaultProfile)
	b.AddNode(&graph.Node{ID: "local", Kind: graph.KindLocal, RepoPrefix: "repo"})
	require.NoError(t, b.Publish(t.Context()))
	s := snapshot(t, b)
	reads := trackSnapshotReads(s)
	require.Empty(t, slices.Collect(s.NodesInScopeSeq([]string{"repo"}, nil, graph.KindLocal)))
	require.ErrorIs(t, s.ReadError(), ErrGroupOff)
	require.Empty(t, reads.points)
	require.Empty(t, reads.scans)
}
