package resolver

import (
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/zzet/gortex/internal/graph"
)

func TestTrackedPythonPackageReExportEvidence(t *testing.T) {
	for _, scenario := range []string{"module_call", "from_alias", "no_import", "unmarked_import", "wrong_package", "package_workspace", "symbol_workspace", "missing_symbol", "ambiguous_layout"} {
		t.Run(scenario, func(t *testing.T) {
			g := graph.New()
			file, pkg, impl := "consumer/app.py", "provider/src/pkg/__init__.py", "provider/src/pkg/impl.py"
			for _, node := range []*graph.Node{
				{ID: file, FilePath: file, Kind: graph.KindFile, RepoPrefix: "consumer", WorkspaceID: "shared"},
				{ID: pkg, FilePath: pkg, Kind: graph.KindFile, RepoPrefix: "provider", WorkspaceID: "shared"},
				{ID: impl + "::Original", FilePath: impl, Name: "Original", Kind: graph.KindType, RepoPrefix: "provider", WorkspaceID: "shared"},
			} {
				if scenario == "package_workspace" && node.ID == pkg || scenario == "symbol_workspace" && node.ID == impl+"::Original" {
					node.WorkspaceID = "foreign"
				}
				if scenario != "missing_symbol" || node.ID != impl+"::Original" {
					g.AddNode(node)
				}
			}
			imp := &graph.Edge{From: file, FilePath: file, To: pkg, Kind: graph.EdgeImports, CrossRepo: true,
				Meta: map[string]any{PythonImportStubMeta: "external::pkg", "python_import": "pkg"}}
			if scenario == "unmarked_import" {
				delete(imp.Meta, PythonImportStubMeta)
			}
			if scenario == "wrong_package" {
				imp.Meta["python_import"] = "other"
			}
			if scenario != "no_import" {
				g.AddEdge(imp)
			}
			g.AddEdge(&graph.Edge{From: pkg, FilePath: pkg, To: impl, Kind: graph.EdgeImports,
				Meta: map[string]any{"python_reexports": map[string]any{"Public": "Original"}, "python_reexport_stem": "src/pkg/impl"}})
			if scenario == "ambiguous_layout" {
				path := "provider/src/pkg/impl/__init__.py"
				g.AddNode(&graph.Node{ID: path + "::Original", FilePath: path, Name: "Original", Kind: graph.KindType, RepoPrefix: "provider", WorkspaceID: "shared"})
			}
			stub := "unresolved::extern::pkg::Public"
			if scenario == "from_alias" {
				stub = "unresolved::extern::pkg.Public::Alias"
			}
			g.AddEdge(&graph.Edge{From: file, FilePath: file, To: stub, Kind: graph.EdgeCalls, Line: 2, Meta: map[string]any{PythonCallStubMeta: stub}})
			cr := NewCrossRepo(g)
			cr.SetPythonPackageLookup(func() *PythonPackageLookup { return nil })
			cr.SetCrossWorkspaceDepLookup(func(string) []CrossWorkspaceDepRule { return nil })
			cr.ResolveForFile("consumer", "app.py")
			calls := outEdgesOfKind(g, file, graph.EdgeCalls)
			require.Len(t, calls, 1)
			if scenario == "module_call" || scenario == "from_alias" {
				require.Equal(t, impl+"::Original", calls[0].To)
				require.True(t, calls[0].CrossRepo)
			} else {
				require.Equal(t, stub, calls[0].To)
				require.False(t, calls[0].CrossRepo)
			}
		})
	}
}
