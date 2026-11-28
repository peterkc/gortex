package store_pebble

import (
	"errors"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/zzet/gortex/internal/graph"
)

func TestRebuildContractViewReaders(t *testing.T) {
	ctx := t.Context()
	b, err := OpenBuild(t.TempDir(), FullProfile)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, b.Close()) })
	b.AddNode(&graph.Node{ID: "a.go", Kind: graph.KindFile, Language: "go", RepoPrefix: "repo", WorkspaceID: "work"})
	b.AddNode(&graph.Node{ID: "b", FilePath: "b.py", Kind: graph.KindFile, Language: "python"})
	b.AddNode(&graph.Node{ID: "call", Kind: graph.KindFunction, FilePath: "b.py"})
	view := b.RebuildView()
	var files [][4]string
	require.NoError(t, view.VisitFileIdentities(ctx, func(path, language, repo, workspace string) error {
		files = append(files, [4]string{path, language, repo, workspace})
		return nil
	}))
	require.Equal(t, [][4]string{{"a.go", "go", "repo", "work"}, {"b.py", "python", "", ""}}, files)

	provide := &graph.Edge{From: "call", To: "di::Bean", Kind: graph.EdgeProvides, FilePath: "b.py", Line: 1}
	consume := &graph.Edge{From: "call", To: "di::Token", Kind: graph.EdgeConsumes, FilePath: "b.py", Line: 2}
	b.AddEdge(provide)
	b.AddEdge(consume)
	b.AddEdge(&graph.Edge{From: "call", To: "di::Token", Kind: graph.EdgeCalls, FilePath: "b.py", Line: 3})
	b.AddEdge(&graph.Edge{From: "a.go", To: "di::Foreign", Kind: graph.EdgeProvides, FilePath: "a.go", Line: 4})
	b.AddEdge(&graph.Edge{From: "missing", To: "di::Dangling", Kind: graph.EdgeConsumes, FilePath: "b.py", Line: 5})
	var di []*graph.Edge
	require.NoError(t, view.VisitDIContractEdges(ctx, "", func(e *graph.Edge) error {
		di = append(di, e)
		return nil
	}))
	require.ElementsMatch(t, []*graph.Edge{provide, consume}, di)

	stop := errors.New("stop visiting contract facts")
	err = view.VisitDIContractEdges(ctx, "", func(*graph.Edge) error { return stop })
	require.ErrorIs(t, err, ErrRebuildView)
	require.ErrorIs(t, err, stop)
	err = view.VisitFileIdentities(ctx, func(string, string, string, string) error { return stop })
	require.ErrorIs(t, err, ErrRebuildView)
	require.ErrorIs(t, err, stop)
}
