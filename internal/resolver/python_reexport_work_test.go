package resolver

import (
	"fmt"
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
			g.reads = map[string]int{}
			cr.buildReachableReposIndex(nil)
			require.Len(t, cr.pythonReexportsByFile[pkg], 1, "publish reexports before workers start")
			require.Equal(t, 1, g.reads[pkg])
			g.reads = map[string]int{}
			for range 4 {
				edge := &graph.Edge{From: file, FilePath: file, Kind: graph.EdgeCalls, Meta: map[string]any{}}
				require.True(t, cr.resolvePythonPackageReExport(edge, "pkg", "Public"))
				require.Equal(t, impl+"::Original", edge.To)
			}
			require.Zero(t, g.reads[file], "caller bridge metadata is already loaded")
			require.Zero(t, g.reads[pkg], "workers only read the prefilled package index")
			cr.clearReachableReposIndex()
			row := g.GetOutEdges(pkg)[0]
			row.Meta["python_reexports"] = map[string]any{"Public": "Alternate"}
			g.ReindexEdges([]graph.EdgeReindex{{Edge: row, OldTo: row.To}})
			g.reads = map[string]int{}
			cr.buildReachableReposIndex(nil)
			require.Len(t, cr.pythonReexportsByFile[pkg], 1, "publish reexports before workers start")
			require.Equal(t, 1, g.reads[pkg])
			g.reads = map[string]int{}
			edge := &graph.Edge{From: file, FilePath: file, Kind: graph.EdgeCalls, Meta: map[string]any{}}
			require.True(t, cr.resolvePythonPackageReExport(edge, "pkg", "Public"))
			require.Equal(t, impl+"::Alternate", edge.To)
			require.Zero(t, g.reads[file])
			require.Zero(t, g.reads[pkg])
		})
	}
}

// The full pass must publish package exports before its resolveEdge fan-out.
// Run this result assertion with -race in fork CI as well.
func TestPythonPackageReExportFullPassWorkers(t *testing.T) {
	for _, backend := range []string{"memory", "sqlite"} {
		t.Run(backend, func(t *testing.T) {
			g := goImportReviewStore(t, backend)
			for i := range 4 {
				pkg := fmt.Sprintf("provider/pkg%d/__init__.py", i)
				impl := fmt.Sprintf("provider/pkg%d/impl.py", i)
				g.AddNode(&graph.Node{ID: pkg, FilePath: pkg, Kind: graph.KindFile, RepoPrefix: "provider", WorkspaceID: "shared"})
				g.AddNode(&graph.Node{ID: impl + "::Original", Name: "Original", FilePath: impl, Kind: graph.KindType, RepoPrefix: "provider", WorkspaceID: "shared"})
				g.AddEdge(&graph.Edge{From: pkg, To: impl, FilePath: pkg, Kind: graph.EdgeImports, Meta: map[string]any{"python_reexports": map[string]any{"Public": "Original"}, "python_reexport_stem": fmt.Sprintf("pkg%d/impl", i)}})
			}
			for i := range 64 {
				file := fmt.Sprintf("consumer/app%d.py", i)
				spec := fmt.Sprintf("pkg%d", i%4)
				pkg := "provider/" + spec + "/__init__.py"
				stub := "unresolved::extern::" + spec + "::Public"
				g.AddNode(&graph.Node{ID: file, FilePath: file, Kind: graph.KindFile, RepoPrefix: "consumer", WorkspaceID: "shared"})
				g.AddEdge(&graph.Edge{From: file, To: pkg, FilePath: file, Kind: graph.EdgeImports, CrossRepo: true, Meta: map[string]any{PythonImportStubMeta: "external::" + spec, "python_import": spec}})
				g.AddEdge(&graph.Edge{From: file, To: stub, FilePath: file, Kind: graph.EdgeCalls, Line: 2, Meta: map[string]any{PythonCallStubMeta: stub}})
			}
			cr := NewCrossRepo(g)
			cr.SetPythonPackageLookup(func() *PythonPackageLookup { return nil })
			cr.SetCrossWorkspaceDepLookup(func(string) []CrossWorkspaceDepRule { return nil })
			cr.ResolveAll()
			for i := range 64 {
				calls := outEdgesOfKind(g, fmt.Sprintf("consumer/app%d.py", i), graph.EdgeCalls)
				require.Len(t, calls, 1)
				require.Equal(t, fmt.Sprintf("provider/pkg%d/impl.py::Original", i%4), calls[0].To)
				require.True(t, calls[0].CrossRepo)
			}
		})
	}
}
