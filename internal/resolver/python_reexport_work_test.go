package resolver

import (
	"github.com/stretchr/testify/require"
	"github.com/zzet/gortex/internal/graph"
	"testing"
)

type pythonReexportCountStore struct {
	graph.Store
	reads map[string]int
}

func (s *pythonReexportCountStore) GetOutEdges(id string) []*graph.Edge {
	s.reads[id]++
	return s.Store.GetOutEdges(id)
}

func TestPythonPackageReExportReadsOncePerPass(t *testing.T) {
	for _, backend := range []string{"memory", "sqlite"} {
		t.Run(backend, func(t *testing.T) {
			g := &pythonReexportCountStore{Store: goImportReviewStore(t, backend), reads: map[string]int{}}
			file, pkg, impl := "consumer/app.py", "provider/pkg/__init__.py", "provider/pkg/impl.py"
			for _, node := range []*graph.Node{
				{ID: file, FilePath: file, Kind: graph.KindFile, RepoPrefix: "consumer", WorkspaceID: "shared"},
				{ID: pkg, FilePath: pkg, Kind: graph.KindFile, RepoPrefix: "provider", WorkspaceID: "shared"},
				{ID: impl + "::Original", Name: "Original", FilePath: impl, Kind: graph.KindType, RepoPrefix: "provider", WorkspaceID: "shared"},
				{ID: impl + "::Alternate", Name: "Alternate", FilePath: impl, Kind: graph.KindType, RepoPrefix: "provider", WorkspaceID: "shared"},
			} {
				g.AddNode(node)
			}
			g.AddEdge(&graph.Edge{From: file, To: pkg, FilePath: file, Kind: graph.EdgeImports, CrossRepo: true, Meta: map[string]any{PythonImportStubMeta: "external::pkg", "python_import": "pkg"}})
			g.AddEdge(&graph.Edge{From: pkg, To: impl, FilePath: pkg, Kind: graph.EdgeImports, Meta: map[string]any{"python_reexports": map[string]any{"Public": "Original"}, "python_reexport_stem": "pkg/impl"}})
			cr := NewCrossRepo(g)
			cr.buildReachableReposIndex(nil)
			g.reads = map[string]int{}
			for range 4 {
				edge := &graph.Edge{From: file, FilePath: file, Kind: graph.EdgeCalls, Meta: map[string]any{}}
				require.True(t, cr.resolvePythonPackageReExport(edge, "pkg", "Public"))
				require.Equal(t, impl+"::Original", edge.To)
			}
			require.Zero(t, g.reads[file], "caller bridge metadata is already loaded")
			require.Equal(t, 1, g.reads[pkg], "package reexports are shared by the pass")
			cr.clearReachableReposIndex()
			row := g.GetOutEdges(pkg)[0]
			row.Meta["python_reexports"] = map[string]any{"Public": "Alternate"}
			g.ReindexEdges([]graph.EdgeReindex{{Edge: row, OldTo: row.To}})
			cr.buildReachableReposIndex(nil)
			g.reads = map[string]int{}
			edge := &graph.Edge{From: file, FilePath: file, Kind: graph.EdgeCalls, Meta: map[string]any{}}
			require.True(t, cr.resolvePythonPackageReExport(edge, "pkg", "Public"))
			require.Equal(t, impl+"::Alternate", edge.To)
			require.Zero(t, g.reads[file])
			require.Equal(t, 1, g.reads[pkg])
		})
	}
}
