package resolver

import (
	"context"
	"iter"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/graph/store_sqlite"
)

func TestTrackedPythonImportRetry(t *testing.T) {
	for _, target := range []string{"unresolved::import::PIL.image", "external::PIL.image", "module::pypi:PIL", "local.py"} {
		t.Run(target, func(t *testing.T) {
			g := graph.New()
			for _, file := range []string{"consumer/app.py", "provider/src/PIL/__init__.py", "provider/src/PIL/image.py", "local.py"} {
				repo := "provider"
				if file == "consumer/app.py" {
					repo = "consumer"
				}
				g.AddNode(&graph.Node{ID: file, FilePath: file, Kind: graph.KindFile, Language: "python", RepoPrefix: repo, WorkspaceID: "shared"})
			}
			e := &graph.Edge{From: "consumer/app.py", FilePath: "consumer/app.py", To: target, Kind: graph.EdgeImports, Line: 1, Meta: map[string]any{"python_import": "PIL.image", "keep": "metadata"}}
			g.AddEdge(e)
			cr := NewCrossRepo(g)
			cr.SetPythonPackageLookup(func() *PythonPackageLookup {
				return &PythonPackageLookup{HasTop: func(top string) bool { return top == "PIL" }, Providers: func(caller, top string) map[string][]graph.FileNodeIdentity {
					require.Equal(t, "consumer", caller)
					require.Equal(t, "PIL", top)
					return map[string][]graph.FileNodeIdentity{"provider": {{ID: "provider/src/PIL/__init__.py", FilePath: "provider/src/PIL/__init__.py", RepoPrefix: "provider", WorkspaceID: "shared"}}}
				}}
			})
			cr.ResolveMutationFiles([]string{"consumer/app.py"}, nil)
			imports := outEdgesOfKind(g, "consumer/app.py", graph.EdgeImports)
			require.Len(t, imports, 1)
			if target == "local.py" {
				require.Equal(t, target, imports[0].To)
				return
			}
			require.Equal(t, "provider/src/PIL/image.py", imports[0].To)
			require.True(t, imports[0].CrossRepo)
			require.Equal(t, "metadata", imports[0].Meta["keep"])
		})
	}
}

func TestTrackedPythonImportSkipsStdlibBeforeLookup(t *testing.T) {
	for _, target := range []string{"module::python:stdlib::json", "external::json", "unresolved::import::json.tool"} {
		t.Run(target, func(t *testing.T) {
			g := graph.New()
			g.AddNode(&graph.Node{ID: "app/a.py", FilePath: "app/a.py", Kind: graph.KindFile, RepoPrefix: "app"})
			g.AddEdge(&graph.Edge{From: "app/a.py", FilePath: "app/a.py", To: target, Kind: graph.EdgeImports, Meta: map[string]any{"python_import": "json"}})
			lookups := 0
			cr := NewCrossRepo(g)
			cr.SetPythonPackageLookup(func() *PythonPackageLookup {
				lookups++
				return nil
			})
			cr.ResolveMutationFiles([]string{"app/a.py"}, nil)
			require.Zero(t, lookups)
		})
	}
}

type pythonImportPageStore struct {
	graph.Store
	fullScans, callerReads  int
	hydrated, callerBatches []int
}

func (s *pythonImportPageStore) EdgesByKind(kind graph.EdgeKind) iter.Seq[*graph.Edge] {
	if kind == graph.EdgeImports {
		s.fullScans++
	}
	return s.Store.EdgesByKind(kind)
}

func (s *pythonImportPageStore) EdgesLightSeq(kinds ...graph.EdgeKind) iter.Seq[*graph.Edge] {
	return graph.EdgesLightSeq(s.Store, kinds...)
}

func (s *pythonImportPageStore) GetNode(id string) *graph.Node {
	if id == "consumer/a.py" {
		s.callerReads++
	}
	return s.Store.GetNode(id)
}

func (s *pythonImportPageStore) GetNodesByIDs(ids []string) map[string]*graph.Node {
	s.callerBatches = append(s.callerBatches, len(ids))
	return s.Store.GetNodesByIDs(ids)
}

func (s *pythonImportPageStore) GetEdgeCandidates(endpoints []graph.EdgeEndpoint, sites []graph.EdgeSite) graph.EdgeCandidateSet {
	s.hydrated = append(s.hydrated, len(sites))
	return s.Store.GetEdgeCandidates(endpoints, sites)
}

func TestTrackedPythonImportPagedFullPass(t *testing.T) {
	for _, backend := range []string{"memory", "sqlite"} {
		t.Run(backend, func(t *testing.T) {
			var store graph.Store = graph.New()
			if backend == "sqlite" {
				db, err := store_sqlite.Open(filepath.Join(t.TempDir(), "graph.sqlite"))
				require.NoError(t, err)
				t.Cleanup(func() { require.NoError(t, db.Close()) })
				store = db
			}
			g := &pythonImportPageStore{Store: store}
			g.AddNode(&graph.Node{ID: "consumer/a.py", FilePath: "consumer/a.py", Kind: graph.KindFile, RepoPrefix: "consumer", WorkspaceID: "shared"})
			g.AddNode(&graph.Node{ID: "provider/PIL/__init__.py", FilePath: "provider/PIL/__init__.py", Kind: graph.KindFile, RepoPrefix: "provider", WorkspaceID: "shared"})
			for line := 1; line <= resolvePendingPageRows+1; line++ {
				g.AddEdge(&graph.Edge{From: "consumer/a.py", FilePath: "consumer/a.py", To: "module::pypi:PIL", Kind: graph.EdgeImports, Line: line, Meta: map[string]any{"python_import": "PIL", "keep": "metadata"}})
			}
			g.AddEdge(&graph.Edge{From: "consumer/a.py", FilePath: "consumer/a.py", To: "module::pypi:untracked", Kind: graph.EdgeImports, Line: resolvePendingPageRows + 2, Meta: map[string]any{"python_import": "untracked"}})
			cr := NewCrossRepo(g)
			cr.SetPythonPackageLookup(func() *PythonPackageLookup {
				return &PythonPackageLookup{HasTop: func(top string) bool { return top == "PIL" }, Providers: func(caller, top string) map[string][]graph.FileNodeIdentity {
					if top != "PIL" {
						return nil
					}
					return map[string][]graph.FileNodeIdentity{"provider": {{ID: "provider/PIL/__init__.py", FilePath: "provider/PIL/__init__.py", RepoPrefix: "provider", WorkspaceID: "shared"}}}
				}}
			})
			stats, err := cr.ResolveAllContext(context.Background())
			require.NoError(t, err)
			require.Equal(t, resolvePendingPageRows+1, stats.Resolved)
			require.Zero(t, g.fullScans, "full import scans must use metadata-free projections")
			require.Zero(t, g.callerReads, "caller nodes must be loaded in batches")
			require.Equal(t, []int{resolvePendingPageRows, 1}, g.hydrated)
			require.Equal(t, []int{1, 1}, g.callerBatches)
			imports := g.GetOutEdges("consumer/a.py")
			require.Len(t, imports, resolvePendingPageRows+2)
			for _, e := range imports {
				if e.Kind == graph.EdgeImports && e.Line <= resolvePendingPageRows+1 {
					require.Equal(t, "provider/PIL/__init__.py", e.To)
					require.Equal(t, "metadata", e.Meta["keep"])
				} else if e.Kind == graph.EdgeImports {
					require.Equal(t, "module::pypi:untracked", e.To)
					require.False(t, e.CrossRepo)
				}
			}
		})
	}
}
