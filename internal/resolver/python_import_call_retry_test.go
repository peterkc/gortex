package resolver

import (
	"context"
	"path/filepath"
	"slices"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/graph/store_sqlite"
)

func TestTrackedPythonImportReopensOnlyNewlyLinkedCalls(t *testing.T) {
	for _, backend := range []string{"memory", "sqlite"} {
		t.Run(backend, func(t *testing.T) {
			var g graph.Store = graph.New()
			if backend == "sqlite" {
				db, err := store_sqlite.Open(filepath.Join(t.TempDir(), "graph.sqlite"))
				require.NoError(t, err)
				t.Cleanup(func() { require.NoError(t, db.Close()) })
				g = db
			}
			file := "consumer/app.py"
			for _, path := range []string{file, "consumer/other.py", "outsider/app.py"} {
				g.AddNode(&graph.Node{ID: path, Kind: graph.KindFile, FilePath: path})
			}
			eligible := "external-call::dep::PIL.image.f"
			for i, tc := range []struct {
				file, target string
				kind         graph.EdgeKind
			}{
				{file, eligible, graph.EdgeCalls},
				{file, "external-call::dep::other.f", graph.EdgeCalls},
				{file, "external-call::dep::PIL.image_extra.f", graph.EdgeCalls},
				{file, "provider/src/PIL/image.py::f", graph.EdgeCalls},
				{file, eligible, graph.EdgeReferences},
				{"consumer/other.py", eligible, graph.EdgeCalls},
				{"outsider/app.py", eligible, graph.EdgeCalls},
				{file, "dep::PIL.image::f", graph.EdgeCalls},
				{file, "dep::other::f", graph.EdgeCalls},
				{file, "dep::PIL.image_extra::f", graph.EdgeCalls},
				{file, "dep::PIL.image::f", graph.EdgeReferences},
			} {
				g.AddEdge(&graph.Edge{From: tc.file, FilePath: tc.file, To: tc.target, Kind: tc.kind, Line: i + 1, Meta: map[string]any{"external_call": true, PythonCallStubMeta: "unresolved::extern::PIL.image::f"}})
			}
			g.AddEdge(&graph.Edge{From: file, FilePath: file, To: eligible, Kind: graph.EdgeCalls, Line: 12})
			g.AddEdge(&graph.Edge{From: file, FilePath: file, To: "dep::PIL.image::f", Kind: graph.EdgeCalls, Line: 13})
			linked := &graph.Edge{From: file, FilePath: file, To: "provider/src/PIL/image.py", Kind: graph.EdgeImports, CrossRepo: true,
				Meta: map[string]any{PythonImportStubMeta: "module::pypi:PIL", "python_import": "PIL.image"}}
			unmarked := &graph.Edge{From: "consumer/other.py", FilePath: "consumer/other.py", To: linked.To, Kind: graph.EdgeImports, CrossRepo: true,
				Meta: map[string]any{"python_import": "PIL.image"}}
			unlinked := &graph.Edge{From: "outsider/app.py", FilePath: "outsider/app.py", To: "module::pypi:PIL", Kind: graph.EdgeImports,
				Meta: linked.Meta}
			cr := NewCrossRepo(g)
			reopened := cr.restubCallsForLinkedPythonImports([]graph.EdgeReindex{{Edge: linked}, {Edge: unmarked}, {Edge: unlinked}})
			require.Len(t, reopened, 2)
			for _, call := range reopened {
				require.Equal(t, file, call.FilePath)
				require.Equal(t, "unresolved::extern::PIL.image::f", call.To)
				require.NotContains(t, call.Meta, "external_call")
			}
			for _, edges := range g.GetOutEdgesByNodeIDs([]string{file, "consumer/other.py", "outsider/app.py"}) {
				for _, edge := range edges {
					if edge.Line != 1 && edge.Line != 8 {
						require.NotEqual(t, "unresolved::extern::PIL.image::f", edge.To)
					}
				}
			}
		})
	}
}

func TestTrackedPythonImportFullPassStreamsReopenedCalls(t *testing.T) {
	g := graph.New()
	for _, file := range []string{"consumer/app.py", "provider/PIL/__init__.py", "provider/PIL/image.py"} {
		g.AddNode(&graph.Node{ID: file, FilePath: file, Kind: graph.KindFile, RepoPrefix: graph.RepoPrefixOfID(file), WorkspaceID: "shared"})
	}
	importEdge := &graph.Edge{From: "consumer/app.py", FilePath: "consumer/app.py", Kind: graph.EdgeImports, To: "external::PIL.image", Line: 1}
	g.AddEdge(importEdge)
	g.AddEdge(&graph.Edge{From: "consumer/app.py", FilePath: "consumer/app.py", Kind: graph.EdgeCalls, To: "external-call::dep::PIL.image.f", Line: 2, Meta: map[string]any{PythonCallStubMeta: "unresolved::extern::PIL.image::f"}})
	cr := NewCrossRepo(g)
	cr.SetPythonPackageLookup(func() *PythonPackageLookup {
		return &PythonPackageLookup{HasTop: func(top string) bool { return top == "PIL" }, Providers: func(caller, top string) map[string][]graph.FileNodeIdentity {
			return map[string][]graph.FileNodeIdentity{"provider": {{ID: "provider/PIL/__init__.py", FilePath: "provider/PIL/__init__.py", RepoPrefix: "provider", WorkspaceID: "shared"}}}
		}}
	})
	calls, err := cr.resolveTrackedPythonImports(context.Background(), slices.Values([]*graph.Edge{importEdge}), &CrossRepoStats{ByRepo: map[string]int{}}, false)
	require.NoError(t, err)
	require.Empty(t, calls, "the full pass must not retain call metadata across import pages")
	out := outEdgesOfKind(g, "consumer/app.py", graph.EdgeCalls)
	require.Len(t, out, 1)
	require.Equal(t, "unresolved::extern::PIL.image::f", out[0].To, "the full pass's pending stream will admit this reopened call")
}

func TestTrackedPythonModuleCallKeepsImportAndWorkspaceEvidence(t *testing.T) {
	for _, backend := range []string{"memory", "sqlite"} {
		for _, scenario := range []string{"module", "from_alias", "unreachable", "foreign_workspace", "wrong_module", "method", "other_provider"} {
			t.Run(backend+"/"+scenario, func(t *testing.T) {
				var g graph.Store = graph.New()
				if backend == "sqlite" {
					db, err := store_sqlite.Open(filepath.Join(t.TempDir(), "graph.sqlite"))
					require.NoError(t, err)
					t.Cleanup(func() { require.NoError(t, db.Close()) })
					g = db
				}
				file, providerFile := "consumer/app.py", "provider/PIL/image.py"
				workspace := "shared"
				if scenario == "foreign_workspace" {
					workspace = "foreign"
				}
				for _, node := range []*graph.Node{
					{ID: file, FilePath: file, Kind: graph.KindFile, RepoPrefix: "consumer", WorkspaceID: "shared", Language: "python"},
					{ID: file + "::run", FilePath: file, Name: "run", Kind: graph.KindFunction, RepoPrefix: "consumer", WorkspaceID: "shared", Language: "python"},
					{ID: providerFile, FilePath: providerFile, Kind: graph.KindFile, RepoPrefix: "provider", WorkspaceID: workspace, Language: "python"},
				} {
					g.AddNode(node)
				}
				if scenario != "unreachable" {
					g.AddEdge(&graph.Edge{From: file, FilePath: file, Kind: graph.EdgeImports, To: providerFile, CrossRepo: true, Line: 1,
						Meta: map[string]any{PythonImportStubMeta: "external::PIL.image", "python_import": "PIL.image"}})
				}
				candidate := &graph.Node{ID: providerFile + "::f", FilePath: providerFile, Name: "f", Kind: graph.KindFunction, RepoPrefix: "provider", WorkspaceID: workspace, Language: "python"}
				switch scenario {
				case "wrong_module":
					candidate.FilePath = "provider/PIL/unrelated.py"
					candidate.ID = candidate.FilePath + "::f"
				case "method":
					candidate.Kind = graph.KindMethod
				case "other_provider":
					candidate.FilePath, candidate.RepoPrefix = "other/PIL/image.py", "other"
					candidate.ID = candidate.FilePath + "::f"
					g.AddNode(&graph.Node{ID: "other/unrelated.py", FilePath: "other/unrelated.py", Kind: graph.KindFile, RepoPrefix: "other", WorkspaceID: "shared"})
					g.AddEdge(&graph.Edge{From: file, FilePath: file, Kind: graph.EdgeImports, To: "other/unrelated.py", Line: 2, CrossRepo: true})
				}
				g.AddNode(candidate)
				original := "unresolved::extern::PIL.image::f"
				if scenario == "from_alias" {
					original = "unresolved::extern::PIL.image.f::g"
				}
				g.AddEdge(&graph.Edge{From: file + "::run", FilePath: file, Kind: graph.EdgeCalls, To: original, Line: 3,
					Meta: map[string]any{PythonCallStubMeta: original}})
				cr := NewCrossRepo(g)
				cr.SetPythonPackageLookup(func() *PythonPackageLookup { return nil })
				cr.SetCrossWorkspaceDepLookup(func(string) []CrossWorkspaceDepRule { return nil })
				cr.ResolveForFile("consumer", "app.py")
				calls := outEdgesOfKind(g, file+"::run", graph.EdgeCalls)
				require.Len(t, calls, 1)
				if scenario == "module" || scenario == "from_alias" {
					require.Equal(t, candidate.ID, calls[0].To)
					require.True(t, calls[0].CrossRepo)
				} else {
					require.Equal(t, original, calls[0].To)
					require.False(t, calls[0].CrossRepo)
				}
			})
		}
	}
}

func TestTrackedPythonScopedRetryVisitsEachCallOnce(t *testing.T) {
	g := graph.New()
	for _, file := range []string{"consumer/app.py", "provider/PIL/__init__.py", "provider/PIL/image.py"} {
		g.AddNode(&graph.Node{ID: file, FilePath: file, Kind: graph.KindFile, RepoPrefix: graph.RepoPrefixOfID(file), WorkspaceID: "shared"})
	}
	g.AddNode(&graph.Node{ID: "provider/PIL/image.py::f", FilePath: "provider/PIL/image.py", Name: "f", Kind: graph.KindFunction, RepoPrefix: "provider", WorkspaceID: "shared"})
	g.AddEdge(&graph.Edge{From: "consumer/app.py", FilePath: "consumer/app.py", Kind: graph.EdgeImports, To: "external::PIL.image", Line: 1})
	g.AddEdge(&graph.Edge{From: "consumer/app.py", FilePath: "consumer/app.py", Kind: graph.EdgeCalls, To: "external-call::dep::PIL.image.f", Line: 2,
		Meta: map[string]any{PythonCallStubMeta: "unresolved::extern::PIL.image::f"}})
	cr := NewCrossRepo(g)
	cr.SetPythonPackageLookup(func() *PythonPackageLookup {
		return &PythonPackageLookup{HasTop: func(top string) bool { return top == "PIL" }, Providers: func(caller, top string) map[string][]graph.FileNodeIdentity {
			return map[string][]graph.FileNodeIdentity{"provider": {{ID: "provider/PIL/__init__.py", FilePath: "provider/PIL/__init__.py", RepoPrefix: "provider", WorkspaceID: "shared"}}}
		}}
	})
	stats := cr.ResolveForFile("consumer", "app.py")
	require.Equal(t, 2, stats.Resolved)
	require.Zero(t, stats.Unresolved)
}
