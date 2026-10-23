package resolver

import (
	"context"
	"fmt"
	"iter"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/graph/store_sqlite"
)

func TestGoModuleImportRetry(t *testing.T) {
	for _, backend := range []string{"memory", "sqlite"} {
		for _, tc := range []struct {
			name, targetWorkspace, declared, want string
			modules                               map[string]string
		}{
			{"sub_package", "shared", "", "b/sub/a.go", map[string]string{"b": "example.com/lib"}},
			{"denied_workspace", "other", "", "dep::example.com/lib", map[string]string{"b": "example.com/lib"}},
			{"declared_workspace", "other", "example.com/lib", "b/sub/a.go", map[string]string{"b": "example.com/lib"}},
			{"wrong_declaration", "other", "example.com/lib2", "dep::example.com/lib", map[string]string{"b": "example.com/lib"}},
			{"wrong_module", "shared", "", "dep::example.com/lib", map[string]string{"b": "example.com/lib2"}},
			{"duplicate", "shared", "", "dep::example.com/lib", map[string]string{"b": "example.com/lib", "fork": "example.com/lib"}},
			{"longest", "shared", "", "child/a.go", map[string]string{"b": "example.com/lib", "child": "example.com/lib/sub"}},
		} {
			t.Run(backend+"/"+tc.name, func(t *testing.T) {
				var g graph.Store = graph.New()
				if backend == "sqlite" {
					s, err := store_sqlite.Open(filepath.Join(t.TempDir(), "graph.sqlite"))
					require.NoError(t, err)
					t.Cleanup(func() { require.NoError(t, s.Close()) })
					g = s
				}
				g.AddNode(&graph.Node{ID: "a/main.go", Kind: graph.KindFile, FilePath: "a/main.go", RepoPrefix: "a", WorkspaceID: "shared", Language: "go"})
				g.AddNode(&graph.Node{ID: "a/main.go::import::example.com/lib/sub", Kind: graph.KindImport, FilePath: "a/main.go", RepoPrefix: "a", Language: "go", StartLine: 2, Meta: map[string]any{"path": "example.com/lib/sub"}})
				addDepNode(t, g, "a", "example.com/lib")
				g.AddEdge(&graph.Edge{From: "a/go.mod", To: "dep::example.com/lib", Kind: graph.EdgeConsumes, FilePath: "a/go.mod"})
				for repo := range tc.modules {
					dir := repo + "/sub"
					if repo == "child" {
						dir = repo
					}
					for _, name := range []string{"z.go", "a.go", "0_test.go"} {
						file := dir + "/" + name
						g.AddNode(&graph.Node{ID: file, Kind: graph.KindFile, FilePath: file, RepoPrefix: repo, WorkspaceID: tc.targetWorkspace, Language: "go"})
					}
				}
				g.AddEdge(&graph.Edge{From: "a/main.go", To: "dep::example.com/lib", Kind: graph.EdgeImports, FilePath: "a/main.go", Line: 2})
				state := &GoModuleDepRetry{}
				resolve := func() {
					cr := NewCrossRepo(g)
					cr.SetGoModules(tc.modules)
					cr.SetGoModuleDepRetry(state, 1)
					cr.SetCrossWorkspaceDepLookup(func(string) []CrossWorkspaceDepRule {
						if tc.declared == "" {
							return nil
						}
						return []CrossWorkspaceDepRule{{Workspace: "other", Modules: []string{tc.declared}}}
					})
					require.NoError(t, cr.RetryGoModuleDeps(context.Background()))
				}
				resolve()
				resolve()
				edges := g.GetOutEdges("a/main.go")
				require.Len(t, edges, 1)
				assert.Equal(t, tc.want, edges[0].To)
				assert.Equal(t, tc.want != "dep::example.com/lib", edges[0].CrossRepo)
				if edges[0].CrossRepo {
					assert.Empty(t, edges[0].Origin)
					assert.Zero(t, edges[0].Confidence)
				}
				require.NotNil(t, g.GetNode("dep::example.com/lib"))
				contracts := g.GetOutEdges("a/go.mod")
				require.Len(t, contracts, 1)
				assert.Equal(t, "dep::example.com/lib", contracts[0].To)
				assert.Equal(t, graph.EdgeConsumes, contracts[0].Kind)
			})
		}
	}
}

func TestGoModuleImportPathAmbiguousLine(t *testing.T) {
	edge := &graph.Edge{To: "dep::example.com/lib", Kind: graph.EdgeImports, Line: 2}
	nodes := []*graph.Node{
		{Kind: graph.KindImport, Language: "go", StartLine: 2, Meta: map[string]any{"path": "example.com/lib/a"}},
		{Kind: graph.KindImport, Language: "go", StartLine: 2, Meta: map[string]any{"path": "example.com/lib/b"}},
	}
	assert.Empty(t, goModuleImportPath(edge, nodes), "a legacy module-only target is not enough evidence for either package")
}

func TestGoModuleImportDroppedBatch(t *testing.T) {
	g := &goModuleDroppingStore{Store: graph.New(), drop: true}
	g.AddNode(&graph.Node{ID: "a/main.go", Kind: graph.KindFile, FilePath: "a/main.go", RepoPrefix: "a", Language: "go"})
	g.AddNode(&graph.Node{ID: "a/main.go::import::example.com/lib", Kind: graph.KindImport, FilePath: "a/main.go", RepoPrefix: "a", Language: "go", StartLine: 2, Meta: map[string]any{"path": "example.com/lib"}})
	g.AddNode(&graph.Node{ID: "b/lib.go", Kind: graph.KindFile, FilePath: "b/lib.go", RepoPrefix: "b", Language: "go"})
	g.AddEdge(&graph.Edge{From: "a/main.go", To: "dep::example.com/lib", Kind: graph.EdgeImports, FilePath: "a/main.go", Line: 2})
	state := &GoModuleDepRetry{}
	for pass := range 2 {
		cr := NewCrossRepo(g)
		cr.SetGoModules(map[string]string{"b": "example.com/lib"})
		cr.SetGoModuleDepRetry(state, 1)
		require.NoError(t, cr.RetryGoModuleDeps(context.Background()))
		edges := g.GetOutEdges("a/main.go")
		require.Len(t, edges, 1)
		if pass == 0 {
			assert.Equal(t, "dep::example.com/lib", edges[0].To)
			assert.Len(t, state.pending, 1)
		} else {
			assert.Equal(t, "b/lib.go", edges[0].To)
			assert.Empty(t, state.pending)
		}
	}
}

// Count the expensive projections, not elapsed time.
type goImportCountingStore struct {
	graph.Store
	fileScans, hydrated     int
	importScans, importRows int
	afterBatch              func()
}

func (s *goImportCountingStore) FileNodeIdentitiesSeq(repos []string) iter.Seq[graph.FileNodeIdentity] {
	s.fileScans++
	return graph.FileNodeIdentitiesSeq(s.Store, repos)
}
func (s *goImportCountingStore) RepoNodeIdentitiesSeq(repos []string, kinds ...graph.NodeKind) iter.Seq[graph.RepoNodeIdentity] {
	return func(yield func(graph.RepoNodeIdentity) bool) {
		imports := len(kinds) == 1 && kinds[0] == graph.KindImport && repos == nil
		if imports {
			s.importScans++
		}
		visited := 0
		for row := range graph.RepoNodeIdentitiesSeq(s.Store, repos, kinds...) {
			if imports {
				s.importRows++
			}
			if !yield(row) {
				return
			}
			visited++
			if visited == externalCallMutationChunk && s.afterBatch != nil {
				s.afterBatch()
			}
		}
	}
}
func (s *goImportCountingStore) GetFileNodesByPaths(paths []string) map[string][]*graph.Node {
	s.hydrated += len(paths)
	return s.Store.GetFileNodesByPaths(paths)
}
func TestGoModuleImportLookupBudget(t *testing.T) {
	for _, mode := range []string{"cache", "filter", "dedup", "frontier_stats"} {
		t.Run(mode, func(t *testing.T) {
			g := &goImportCountingStore{Store: graph.New()}
			g.AddNode(&graph.Node{ID: "a/main.go", Kind: graph.KindFile, FilePath: "a/main.go", RepoPrefix: "a", Language: "go"})
			g.AddNode(&graph.Node{ID: "b/lib.go", Kind: graph.KindFile, FilePath: "b/lib.go", RepoPrefix: "b", Language: "go"})
			g.AddNode(&graph.Node{ID: "a/main.go::import::example.com/lib", Kind: graph.KindImport, FilePath: "a/main.go", RepoPrefix: "a", Language: "go", StartLine: 2, Meta: map[string]any{"path": "example.com/lib"}})
			e := &graph.Edge{From: "a/main.go", To: "dep::example.com/lib", Kind: graph.EdgeImports, FilePath: "a/main.go", Line: 2}
			g.AddEdge(e)
			cr := NewCrossRepo(g)
			cr.SetGoModules(map[string]string{"b": "example.com/lib"})
			switch mode {
			case "cache":
				require.NoError(t, cr.resolveGoModuleImports(t.Context(), []string{"a/main.go"}, nil, nil))
				g.RemoveEdge("a/main.go", "b/lib.go", graph.EdgeImports)
				g.AddEdge(&graph.Edge{From: "a/main.go", To: "dep::example.com/lib", Kind: graph.EdgeImports, FilePath: "a/main.go", Line: 2})
				require.NoError(t, cr.resolveGoModuleImports(t.Context(), []string{"a/main.go"}, nil, nil))
				assert.Equal(t, 1, g.fileScans)
			case "filter":
				g.RemoveEdge(e.From, e.To, e.Kind)
				g.EvictFile("a/main.go")
				g.AddNode(&graph.Node{ID: "a/unrelated.go::import::fmt", Kind: graph.KindImport, FilePath: "a/unrelated.go", RepoPrefix: "a", Language: "go"})
				require.NoError(t, cr.ResolveGoModuleIncomingForRepo("b"))
				require.NoError(t, cr.RetryGoModuleDeps(t.Context()))
				assert.Zero(t, g.hydrated)
			case "dedup":
				stats := &CrossRepoStats{ByRepo: map[string]int{}}
				cr.resolveGoModuleEdgesLocked([]*graph.Edge{e, e}, stats)
				assert.Equal(t, 1, stats.Resolved)
				assert.Equal(t, 1, stats.CrossRepoEdges)
			case "frontier_stats":
				g.RemoveEdge(e.From, e.To, e.Kind)
				addDepNode(t, g, "a", "example.com/lib")
				e.To = "unresolved::import::example.com/lib"
				g.AddEdge(e)
				stats := cr.ResolveMutationFrontiers([]string{"a/main.go"}, nil, nil)
				assert.Equal(t, 1, stats.Resolved)
				assert.Equal(t, 1, stats.CrossRepoEdges)
			}
		})
	}
}

func TestGoModuleImportWarmProviderEdit(t *testing.T) {
	for _, route := range []string{"mutation", "post_publish"} {
		t.Run(route, func(t *testing.T) {
			db := filepath.Join(t.TempDir(), "graph.sqlite")
			s, err := store_sqlite.Open(db)
			require.NoError(t, err)
			s.AddNode(&graph.Node{ID: "a/main.go", Kind: graph.KindFile, FilePath: "a/main.go", RepoPrefix: "a", Language: "go"})
			s.AddNode(&graph.Node{ID: "a/main.go::import::example.com/lib", Kind: graph.KindImport, FilePath: "a/main.go", RepoPrefix: "a", Language: "go", StartLine: 2, Meta: map[string]any{"path": "example.com/lib"}})
			s.AddNode(&graph.Node{ID: "b/lib.go", Kind: graph.KindFile, FilePath: "b/lib.go", RepoPrefix: "b", Language: "go"})
			// Persist an already-bound graph, with no retry state carried over.
			s.AddEdge(&graph.Edge{From: "a/main.go", To: "b/lib.go", Kind: graph.EdgeImports, FilePath: "a/main.go", Line: 2, CrossRepo: true})
			require.NoError(t, s.Close())
			s, err = store_sqlite.Open(db)
			require.NoError(t, err)
			t.Cleanup(func() { require.NoError(t, s.Close()) })
			g := &goImportCountingStore{Store: s}
			cr := NewCrossRepo(g)
			cr.SetGoModules(map[string]string{"b": "example.com/lib"})
			// The unchanged warm-restart fast path skips global resolution.
			// Include unrelated imports to expose the initial scan's exact cost.
			for _, path := range []string{"fmt", "strings", "example.com/untracked"} {
				s.AddNode(&graph.Node{ID: "a/other.go::import::" + path, Kind: graph.KindImport, FilePath: "a/other.go", RepoPrefix: "a", Language: "go"})
			}
			s.EvictFile("b/lib.go")
			s.AddNode(&graph.Node{ID: "b/lib.go", Kind: graph.KindFile, FilePath: "b/lib.go", RepoPrefix: "b", Language: "go"})
			if route == "mutation" {
				cr.ResolveMutationFrontiers(nil, nil, []string{"b/lib.go"})
			} else {
				require.NoError(t, cr.ResolveGoModuleIncomingForRepo("b"))
			}
			edges := s.GetOutEdges("a/main.go")
			require.Len(t, edges, 1)
			assert.Equal(t, "b/lib.go", edges[0].To)
			assert.True(t, edges[0].CrossRepo)
			assert.Equal(t, 1, g.importScans)
			assert.Equal(t, 4, g.importRows, "one tracked and three unrelated import identities")
			t.Logf("lazy seed: global scans=%d, import identities visited=%d", g.importScans, g.importRows)
			s.EvictFile("b/lib.go")
			s.AddNode(&graph.Node{ID: "b/lib.go", Kind: graph.KindFile, FilePath: "b/lib.go", RepoPrefix: "b", Language: "go"})
			if route == "mutation" {
				cr.ResolveMutationFrontiers(nil, nil, []string{"b/lib.go"})
			} else {
				require.NoError(t, cr.ResolveGoModuleIncomingForRepo("b"))
			}
			require.Len(t, s.GetOutEdges("a/main.go"), 1)
			assert.Equal(t, 1, g.importScans, "later edits must not repeat the global identity scan")
			assert.Equal(t, 4, g.importRows)
		})
	}
}

type goImportPanicStore struct{ graph.Store }

func (s *goImportPanicStore) GetFileNodesByPaths([]string) map[string][]*graph.Node {
	panic("import-store-panic")
}

func TestGoModuleImportPanicUnlock(t *testing.T) {
	if os.Getenv("GORTEX_TEST_IMPORT_PANIC") != "1" {
		cmd := exec.Command(os.Args[0], "-test.run=^TestGoModuleImportPanicUnlock$", "-test.v")
		cmd.Env = append(os.Environ(), "GORTEX_TEST_IMPORT_PANIC=1")
		output, err := cmd.CombinedOutput()
		require.NoError(t, err, "%s", output)
		return
	}
	g := &goImportPanicStore{Store: graph.New()}
	g.AddNode(&graph.Node{ID: "a/main.go::import::example.com/lib", Kind: graph.KindImport, FilePath: "a/main.go", RepoPrefix: "a", Language: "go"})
	cr := NewCrossRepo(g)
	cr.SetGoModules(map[string]string{"b": "example.com/lib"})
	assert.PanicsWithValue(t, "import-store-panic", func() { _ = cr.RetryGoModuleDeps(t.Context()) })
	require.True(t, cr.mu.TryLock(), "the store resolve mutex must be released after panic")
	cr.mu.Unlock()
	require.True(t, cr.goModuleRetry.mu.TryLock(), "the retry queue mutex must remain usable")
	cr.goModuleRetry.mu.Unlock()
}

func TestGoModuleImportBatchUnlock(t *testing.T) {
	for _, route := range []string{"retry", "post_publish"} {
		t.Run(route, func(t *testing.T) {
			g := &goImportCountingStore{Store: graph.New()}
			for i := 0; i <= externalCallMutationChunk; i++ {
				file := fmt.Sprintf("a/f%d.go", i)
				g.AddNode(&graph.Node{ID: file + "::import::example.com/lib", Kind: graph.KindImport, FilePath: file, RepoPrefix: "a", Language: "go"})
			}
			cr := NewCrossRepo(g)
			cr.SetGoModules(map[string]string{"a": "example.com/consumer", "b": "example.com/lib"})
			probes := 0
			g.afterBatch = func() {
				probes++
				available := cr.mu.TryLock()
				assert.True(t, available, "the next identity page must not hold the resolve mutex")
				if available {
					cr.mu.Unlock()
				}
			}
			if route == "retry" {
				require.NoError(t, cr.RetryGoModuleDeps(t.Context()))
			} else {
				require.NoError(t, cr.ResolveGoModuleIncomingForRepo("a"))
			}
			assert.Positive(t, probes)
		})
	}
}
