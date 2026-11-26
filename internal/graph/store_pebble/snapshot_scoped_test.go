package store_pebble

import (
	"errors"
	"fmt"
	"slices"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/zzet/gortex/internal/graph"
)

func TestSnapshotScopedOutlineReadBounds(t *testing.T) {
	nodes := []*graph.Node{
		{ID: "a", Kind: graph.KindFunction, RepoPrefix: "repo", WorkspaceID: "w", ProjectID: "p", Language: "go", FilePath: "repo/a.go"},
		{ID: "b", Kind: graph.KindFile, RepoPrefix: "repo", WorkspaceID: "w", ProjectID: "p", Language: "go"},
		{ID: "ext", Kind: graph.KindFunction, FilePath: "external::go:fmt"},
		{ID: "unowned", Kind: graph.KindFunction, FilePath: "unowned.go", WorkspaceID: "other"},
		{ID: "foreign", Kind: graph.KindContract, RepoPrefix: "repo/sub", WorkspaceID: "w", ProjectID: "other"},
	}
	edges := []*graph.Edge{
		{From: "a", To: "b", Kind: graph.EdgeCalls},
		{From: "a", To: "b", Kind: graph.EdgeImports, Line: 1},
		{From: "a", To: "b", Kind: graph.EdgeImports, Line: 2, Meta: map[string]any{"via": "hidden"}},
		{From: "a", To: "foreign", Kind: graph.EdgeCalls},
		{From: "a", To: "missing", Kind: graph.EdgeCalls},
		{From: "foreign", To: "b", Kind: graph.EdgeImports, Line: 1},
		{From: "foreign", To: "b", Kind: graph.EdgeImports, Line: 2},
		{From: "missing", To: "ext", Kind: graph.EdgeImports},
		{From: "unowned", To: "b", Kind: graph.EdgeCalls},
		{From: "a", To: "ext", Kind: graph.EdgeReferences},
	}
	for i := range 40 {
		id := fmt.Sprintf("noise-%02d", i)
		nodes = append(nodes, &graph.Node{ID: id, Kind: graph.KindFunction, RepoPrefix: "repo/sub"})
		edges = append(edges, &graph.Edge{From: id, To: "foreign", Kind: graph.EdgeCalls})
	}
	_, _, s := pairedSnapshot(t, nodes, edges)
	ref := graph.New()
	ref.AddBatch(nodes, edges)
	reads := trackSnapshotReads(s)
	narrow := graph.NodeScope{WorkspaceID: "w", ProjectID: "p", RepoAllow: map[string]bool{"repo": true, "repo/sub": false}}
	for _, filter := range []graph.OutlineEdgeFilter{
		{},
		{ExcludeVia: "hidden", ExcludeNodeKinds: []graph.NodeKind{graph.KindContract}},
		{ExcludeKinds: []graph.EdgeKind{graph.EdgeImports}},
		{ExcludeNodeIDs: map[string]bool{"b": true, "a": false}},
	} {
		reads.points, reads.scans = nil, nil
		want, err := ref.ScopedOutlineCountsFiltered(narrow, filter)
		require.NoError(t, err)
		got, err := s.ScopedOutlineCountsFiltered(narrow, filter)
		require.NoError(t, err)
		require.Equal(t, want.TotalNodes, got.TotalNodes)
		require.Equal(t, want.TotalEdges, got.TotalEdges)
		require.Equal(t, want.ByLanguage, got.ByLanguage)
		require.ElementsMatch(t, want.FileImports, got.FileImports)
		var points, scans []string
		for _, id := range []string{"a", "b", "ext", "unowned"} {
			points = append(points, string(keyStrings([]byte{familyNode}, id)))
		}
		if len(filter.ExcludeNodeKinds) != 0 {
			// Only imports from outside the scope need the source kind.
			points = append(points, string(keyStrings([]byte{familyNode}, "foreign")), string(keyStrings([]byte{familyNode}, "missing")))
		}
		scans = append(scans, string(keyStrings([]byte{familyRepo}, "")), string(keyStrings([]byte{familyRepo}, "repo")))
		for _, id := range []string{"a", "b", "ext"} {
			scans = append(scans, string(keyStrings([]byte{familyEdge, edgeOut}, id)))
			if !slices.Contains(filter.ExcludeKinds, graph.EdgeImports) {
				scans = append(scans, string(keyStrings([]byte{familyEdge, edgeIn}, id, string(graph.EdgeImports))))
			}
		}
		require.ElementsMatch(t, points, reads.points)
		require.ElementsMatch(t, scans, reads.scans)
	}
	for _, scope := range []graph.NodeScope{
		{}, {ProjectID: "p"}, {WorkspaceID: "w"}, {RepoAllow: map[string]bool{"repo": false}}, {RepoAllow: map[string]bool{"missing": true}}, narrow,
	} {
		want, err := ref.ScopedOutlineCounts(scope)
		require.NoError(t, err)
		got, err := s.ScopedOutlineCounts(scope)
		require.NoError(t, err)
		require.Equal(t, want.TotalNodes, got.TotalNodes)
		require.Equal(t, want.TotalEdges, got.TotalEdges)
		require.Equal(t, want.ByLanguage, got.ByLanguage)
		require.ElementsMatch(t, want.FileImports, got.FileImports)
	}
	require.NoError(t, s.ReadError())
}

func TestSnapshotScopedOutlineReadError(t *testing.T) {
	_, _, s := pairedSnapshot(t, []*graph.Node{{ID: "a", RepoPrefix: "repo"}, {ID: "b", RepoPrefix: "repo"}}, nil)
	reads := trackSnapshotReads(s)
	reads.failPoint = string(keyStrings([]byte{familyNode}, "b"))
	reads.failure = errors.New("injected snapshot read failure")
	got, err := s.ScopedOutlineCounts(graph.NodeScope{RepoAllow: map[string]bool{"repo": true}})
	require.ErrorIs(t, err, reads.failure)
	require.Equal(t, graph.OutlineCounts{}, got)
}
