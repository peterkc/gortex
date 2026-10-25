package indexer

import (
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/resolver"
)

func TestPythonRestubLeavesUnmarkedImportsUnchanged(t *testing.T) {
	for _, file := range []string{"provider/module.py", "provider/main.go"} {
		t.Run(file, func(t *testing.T) {
			g := graph.New()
			g.AddNode(&graph.Node{ID: file, Kind: graph.KindFile, FilePath: file, RepoPrefix: "provider"})
			edge := &graph.Edge{From: "consumer/source", FilePath: "consumer/source", To: file, Kind: graph.EdgeImports, CrossRepo: true}
			if file == "provider/main.go" {
				edge.Meta = map[string]any{resolver.PythonImportStubMeta: "external::PIL.image"}
			}
			g.AddEdge(edge)
			require.Empty(t, restubTrackedPythonRepoIncoming(g, "provider"))
			require.Equal(t, file, edge.To)
			require.True(t, edge.CrossRepo)
		})
	}
}

func TestPythonRestubKeepsExactImportAndCallStubs(t *testing.T) {
	g := graph.New()
	file, caller, target := "consumer/app.py", "consumer/app.py::run", "provider/src/PIL/image.py"
	g.AddNode(&graph.Node{ID: file, Kind: graph.KindFile, FilePath: file, RepoPrefix: "consumer"})
	g.AddNode(&graph.Node{ID: caller, Kind: graph.KindFunction, FilePath: file, RepoPrefix: "consumer"})
	g.AddNode(&graph.Node{ID: target, Kind: graph.KindFile, FilePath: target, RepoPrefix: "provider"})
	importEdge := &graph.Edge{From: file, FilePath: file, To: target, Kind: graph.EdgeImports, CrossRepo: true, Meta: map[string]any{
		resolver.PythonImportStubMeta: "module::pypi:PIL", "python_import": "PIL.image",
	}}
	g.AddNode(&graph.Node{ID: target + "::f", Kind: graph.KindFunction, FilePath: target, RepoPrefix: "provider"})
	call := &graph.Edge{From: caller, FilePath: file, To: target + "::f", Kind: graph.EdgeCalls, CrossRepo: true,
		Meta: map[string]any{resolver.PythonCallStubMeta: "unresolved::extern::PIL.image::f"}}
	g.AddEdge(importEdge)
	g.AddEdge(call)
	require.Equal(t, []string{file}, restubTrackedPythonRepoIncoming(g, "provider"))
	require.Equal(t, "module::pypi:PIL", importEdge.To)
	require.Equal(t, "PIL.image", importEdge.Meta["python_import"])
	require.False(t, importEdge.CrossRepo)
	require.Equal(t, "unresolved::extern::PIL.image::f", call.To)
	require.False(t, call.CrossRepo)
}

func TestPythonRestubIgnoresMetadataOnlyTargets(t *testing.T) {
	g := graph.New()
	target := "provider/src/PIL/image.py"
	edge := &graph.Edge{From: "consumer/app.py", FilePath: "consumer/app.py", To: target, Kind: graph.EdgeImports, CrossRepo: true,
		Meta: map[string]any{resolver.PythonImportStubMeta: "module::pypi:PIL"}}
	g.AddEdge(edge)
	files := restubTrackedPythonImportsFromView(g, map[string]struct{}{"provider/src/PIL/changed.py": {}}, map[string][]*graph.Edge{target: {edge}}, false)
	require.Empty(t, files)
	require.Equal(t, target, edge.To)
	require.True(t, edge.CrossRepo)
}
