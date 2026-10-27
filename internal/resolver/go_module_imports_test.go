package resolver

import (
	"context"
	"fmt"
	"iter"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
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
	assert.Empty(t, goModuleImportPath(edge, nodes, nil), "a legacy module-only target is not enough evidence for either package")
}

func TestGoModuleImportPathContainment(t *testing.T) {
	const file = "a/main.go"
	const path = "example.com/lib/sub"
	const id = file + "::import::" + path
	edge := &graph.Edge{From: file, To: "dep::example.com/lib", Kind: graph.EdgeImports, FilePath: file, Line: 3}
	nodes := []*graph.Node{
		{ID: id, Kind: graph.KindImport, Language: "go", StartLine: 4, Meta: map[string]any{"path": path}},
	}
	for _, tc := range []struct {
		name string
		link *graph.Edge
		want string
	}{
		{"exact_line", &graph.Edge{From: file, To: id, Kind: graph.EdgeContains, Line: 3}, path},
		{"wrong_owner", &graph.Edge{From: "a/other.go", To: id, Kind: graph.EdgeContains, Line: 3}, ""},
		{"wrong_line", &graph.Edge{From: file, To: id, Kind: graph.EdgeContains, Line: 4}, ""},
		{"wrong_kind", &graph.Edge{From: file, To: id, Kind: graph.EdgeImports, Line: 3}, ""},
		{"wrong_target", &graph.Edge{From: file, To: "other", Kind: graph.EdgeContains, Line: 3}, ""},
		{"missing", nil, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var links []*graph.Edge
			if tc.link != nil {
				links = append(links, tc.link)
			}
			assert.Equal(t, tc.want, goModuleImportPath(edge, nodes, links))
		})
	}
	nodes = append(nodes, &graph.Node{ID: "other", Kind: graph.KindImport, Language: "go", StartLine: 4, Meta: map[string]any{"path": "example.com/lib/other"}})
	links := []*graph.Edge{
		{From: file, To: id, Kind: graph.EdgeContains, Line: 3},
		{From: file, To: "other", Kind: graph.EdgeContains, Line: 3},
	}
	assert.Empty(t, goModuleImportPath(edge, nodes, links), "containment must not bypass same-line package ambiguity")
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
	edgeLoads               int
	importScans, importRows int
	bindingRechecks         int
	afterBatch              func()
}

func (s *goImportCountingStore) FileNodeIdentitiesSeq(repos []string) iter.Seq[graph.FileNodeIdentity] {
	s.fileScans++
	return graph.FileNodeIdentitiesSeq(s.Store, repos)
}
func (s *goImportCountingStore) RepoNodeIdentitiesSeq(repos []string, kinds ...graph.NodeKind) iter.Seq[graph.RepoNodeIdentity] {
	if IsGoModuleBindingRecheckForTest(kinds) {
		s.bindingRechecks++
	}
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
func (s *goImportCountingStore) GetOutEdgesByNodeIDs(ids []string) map[string][]*graph.Edge {
	s.edgeLoads += len(ids)
	return s.Store.GetOutEdgesByNodeIDs(ids)
}

func TestGoModuleImportLookupBudget(t *testing.T) {
	for _, mode := range []string{"cache", "filter", "dedup", "frontier_stats", "external_stats", "deleted_target"} {
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
			case "frontier_stats", "external_stats":
				g.RemoveEdge(e.From, e.To, e.Kind)
				if mode == "frontier_stats" {
					addDepNode(t, g, "a", "example.com/lib")
				}
				e.To = "unresolved::import::example.com/lib"
				g.AddEdge(e)
				stats := cr.ResolveMutationFrontiers([]string{"a/main.go"}, nil, nil)
				assert.Equal(t, 1, stats.Resolved)
				assert.Equal(t, 1, stats.CrossRepoEdges)
				assert.Zero(t, stats.Unresolved)
			case "deleted_target":
				require.NoError(t, cr.resolveGoModuleImports(t.Context(), []string{"a/main.go"}, nil, nil))
				g.EvictFile("b/lib.go")
				g.AddEdge(&graph.Edge{From: e.From, To: "dep::example.com/lib", Kind: e.Kind, FilePath: e.FilePath, Line: e.Line})
				require.NoError(t, cr.resolveGoModuleImports(t.Context(), []string{"a/main.go"}, nil, nil))
				edges := g.GetOutEdges("a/main.go")
				require.Len(t, edges, 1)
				assert.Equal(t, "dep::example.com/lib", edges[0].To, "a stale cached representative must not become a dangling target")
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

func TestGoModuleImportSameRepoSave(t *testing.T) {
	for _, otherRepo := range []bool{false, true} {
		t.Run(fmt.Sprint(otherRepo), func(t *testing.T) {
			g := &goImportCountingStore{Store: graph.New()}
			modules := map[string]string{"a": "example.com/self"}
			if otherRepo {
				modules["b"] = "example.com/other"
				g.AddNode(&graph.Node{ID: "b/lib.go", Kind: graph.KindFile, FilePath: "b/lib.go", RepoPrefix: "b", Language: "go"})
			}
			for i := range 4 {
				file := fmt.Sprintf("a/f%d.go", i)
				g.AddNode(&graph.Node{ID: file, Kind: graph.KindFile, FilePath: file, RepoPrefix: "a", Language: "go"})
				g.AddNode(&graph.Node{ID: file + "::import::example.com/self", Kind: graph.KindImport, FilePath: file, RepoPrefix: "a", Language: "go", StartLine: 2})
				g.AddEdge(&graph.Edge{From: file, To: "a/f0.go", Kind: graph.EdgeImports, FilePath: file, Line: 2})
			}
			cr := NewCrossRepo(g)
			cr.SetGoModules(modules)
			for range 2 { // cold seed, then an ordinary same-repo save
				g.hydrated, g.edgeLoads = 0, 0
				cr.ResolveMutationFrontiers(nil, nil, []string{"a/f0.go"})
				assert.Equal(t, 1, g.hydrated, "only the exact definition file may be hydrated")
				assert.Zero(t, g.edgeLoads, "same-repo imports must not trigger restore adjacency loads")
				assert.Empty(t, cr.goModuleRetry.imports["a"])
			}
		})
	}
}

func TestGoModuleImportFallbackProviderDeletion(t *testing.T) {
	for _, backend := range []string{"memory", "sqlite"} {
		t.Run(backend, func(t *testing.T) {
			var g graph.Store = graph.New()
			if backend == "sqlite" {
				s, err := store_sqlite.Open(filepath.Join(t.TempDir(), "graph.sqlite"))
				require.NoError(t, err)
				t.Cleanup(func() { require.NoError(t, s.Close()) })
				g = s
			}
			g.AddNode(&graph.Node{ID: "a/main.go", Kind: graph.KindFile, FilePath: "a/main.go", RepoPrefix: "a", Language: "go"})
			g.AddNode(&graph.Node{ID: "a/main.go::import::example.com/lib", Kind: graph.KindImport, FilePath: "a/main.go", RepoPrefix: "a", Language: "go", StartLine: 2})
			for _, file := range []string{"b/a.go", "b/z.go"} {
				g.AddNode(&graph.Node{ID: file, Kind: graph.KindFile, FilePath: file, RepoPrefix: "b", Language: "go"})
			}
			resetImport := func() {
				for _, e := range g.GetOutEdges("a/main.go") {
					g.RemoveEdge(e.From, e.To, e.Kind)
				}
				g.AddEdge(&graph.Edge{From: "a/main.go", To: "unresolved::import::example.com/lib", Kind: graph.EdgeImports, FilePath: "a/main.go", Line: 2})
			}
			cr := NewCrossRepo(g)
			cr.SetGoModules(map[string]string{"b": "example.com/lib"})
			resetImport()
			require.NoError(t, cr.RetryGoModuleDeps(t.Context()))
			require.Equal(t, "b/a.go", g.GetOutEdges("a/main.go")[0].To)
			g.EvictFile("b/a.go")
			_, err := cr.ResolveAllContext(t.Context()) // incomplete receipt and watcher fallback
			require.NoError(t, err)
			var targets []string
			for _, e := range g.GetOutEdges("a/main.go") {
				if e.Kind == graph.EdgeImports {
					targets = append(targets, e.To)
				}
			}
			assert.Equal(t, []string{"b/z.go"}, targets, "fallback must restore the evicted import onto a live representative")
			resetImport()
			cr.ResolveMutationFrontiers([]string{"a/main.go"}, nil, nil)
			edges := g.GetOutEdges("a/main.go")
			require.Len(t, edges, 1)
			assert.Equal(t, "b/z.go", edges[0].To, "consumer edit must not reuse the deleted package-cache entry")
			require.NotNil(t, g.GetNode(edges[0].To))
		})
	}
}

func TestGoModuleImportIDsDoesNotSeed(t *testing.T) {
	g := &goImportCountingStore{Store: graph.New()}
	g.AddNode(&graph.Node{ID: "a/main.go::import::example.com/lib", Kind: graph.KindImport, FilePath: "a/main.go", RepoPrefix: "a", Language: "go"})
	cr := NewCrossRepo(g)
	cr.SetGoModules(map[string]string{"b": "example.com/lib"})
	cr.mu.Lock()
	defer cr.mu.Unlock()
	cr.goModuleImportIDs("b")
	assert.Zero(t, g.importScans, "the ID getter must not run a lock-yielding seed")
	assert.Zero(t, g.hydrated)
}

type goImportCancelStore struct {
	graph.Store
	cancel  context.CancelFunc
	batches int
}

func (s *goImportCancelStore) GetNodesByIDs(ids []string) map[string]*graph.Node {
	if s.cancel != nil && len(ids) > 0 && strings.Contains(ids[0], "::import::") {
		s.batches++
		s.cancel()
	}
	return s.Store.GetNodesByIDs(ids)
}

func TestGoModuleImportRestoreCancellation(t *testing.T) {
	g := &goImportCancelStore{Store: graph.New()}
	g.AddNode(&graph.Node{ID: "b/lib.go", Kind: graph.KindFile, FilePath: "b/lib.go", RepoPrefix: "b", Language: "go"})
	for i := range 2*externalCallMutationChunk + 1 {
		file := fmt.Sprintf("a/f%d.go", i)
		g.AddNode(&graph.Node{ID: file, Kind: graph.KindFile, FilePath: file, RepoPrefix: "a", Language: "go"})
		g.AddNode(&graph.Node{ID: file + "::import::example.com/lib", Kind: graph.KindImport, FilePath: file, RepoPrefix: "a", Language: "go", StartLine: 2})
		g.AddEdge(&graph.Edge{From: file, To: "unresolved::import::example.com/lib", Kind: graph.EdgeImports, FilePath: file, Line: 2})
	}
	cr := NewCrossRepo(g)
	cr.SetGoModules(map[string]string{"b": "example.com/lib"})
	require.NoError(t, cr.RetryGoModuleDeps(t.Context()))
	g.EvictFile("b/lib.go")
	g.AddNode(&graph.Node{ID: "b/lib.go", Kind: graph.KindFile, FilePath: "b/lib.go", RepoPrefix: "b", Language: "go"})
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	g.cancel = cancel // cancel when restoration starts its first payload batch
	stats, err := cr.ResolveAllContext(ctx)
	assert.ErrorIs(t, err, context.Canceled)
	assert.Equal(t, 1, g.batches, "cancellation must stop before the next restoration batch")
	assert.Zero(t, stats.Resolved, "the cancelled context must reach the import rebind")
}

func TestGoModuleImportRestoreSameLine(t *testing.T) {
	for _, backend := range []string{"memory", "sqlite"} {
		for _, route := range []string{"mutation", "fallback", "post_publish"} {
			t.Run(backend+"/"+route, func(t *testing.T) {
				var g graph.Store = graph.New()
				if backend == "sqlite" {
					s, err := store_sqlite.Open(filepath.Join(t.TempDir(), "graph.sqlite"))
					require.NoError(t, err)
					t.Cleanup(func() { require.NoError(t, s.Close()) })
					g = s
				}
				g.AddNode(&graph.Node{ID: "a/main.go", Kind: graph.KindFile, FilePath: "a/main.go", RepoPrefix: "a", Language: "go"})
				// import ( b "example.com/lib/sub"; c "example.net/other/pkg" )
				for _, path := range []string{"example.com/lib/sub", "example.net/other/pkg"} {
					g.AddNode(&graph.Node{ID: "a/main.go::import::" + path, Kind: graph.KindImport, FilePath: "a/main.go", RepoPrefix: "a", Language: "go", StartLine: 2, Meta: map[string]any{"path": path}})
					g.AddEdge(&graph.Edge{From: "a/main.go", To: "unresolved::import::" + path, Kind: graph.EdgeImports, FilePath: "a/main.go", Line: 2})
				}
				for _, file := range []string{"b/sub/lib.go", "c/pkg/other.go"} {
					g.AddNode(&graph.Node{ID: file, Kind: graph.KindFile, FilePath: file, RepoPrefix: graph.RepoPrefixOfID(file), Language: "go"})
				}
				cr := NewCrossRepo(g)
				cr.SetGoModules(map[string]string{"b": "example.com/lib", "c": "example.net/other"})
				require.NoError(t, cr.RetryGoModuleDeps(t.Context()))
				require.Len(t, g.GetOutEdges("a/main.go"), 2)
				g.EvictFile("b/sub/lib.go")
				remaining := g.GetOutEdges("a/main.go")
				require.Len(t, remaining, 1)
				require.Equal(t, "c/pkg/other.go", remaining[0].To)
				g.AddNode(&graph.Node{ID: "b/sub/lib.go", Kind: graph.KindFile, FilePath: "b/sub/lib.go", RepoPrefix: "b", Language: "go"})
				resolve := func() *CrossRepoStats {
					switch route {
					case "mutation":
						return cr.ResolveMutationFrontiers(nil, nil, []string{"b/sub/lib.go"})
					case "fallback":
						stats, err := cr.ResolveAllContext(t.Context())
						require.NoError(t, err)
						return stats
					default:
						require.NoError(t, cr.ResolveGoModuleIncomingForRepo("b"))
						return nil
					}
				}
				for pass := range 2 { // recovery, then an unchanged provider pass
					if stats := resolve(); stats != nil {
						assert.Equal(t, 1-pass, stats.Resolved, "only the missing import may be rebound")
					}
					var targets []string
					for _, e := range g.GetOutEdges("a/main.go") {
						if e.Kind == graph.EdgeImports {
							targets = append(targets, e.To)
							assert.True(t, e.CrossRepo)
							assert.Equal(t, 2, e.Line)
						}
					}
					assert.ElementsMatch(t, []string{"b/sub/lib.go", "c/pkg/other.go"}, targets, "both same-line imports must survive provider recovery without duplicates")
				}
			})
		}
	}
}

func TestGoModuleImportRestoreNestedProvider(t *testing.T) {
	for _, backend := range []string{"memory", "sqlite"} {
		for _, module := range []string{"example.com/lib/v2", "example.com/lib/nested"} {
			t.Run(backend+"/"+module, func(t *testing.T) {
				var g graph.Store = graph.New()
				if backend == "sqlite" {
					s, err := store_sqlite.Open(filepath.Join(t.TempDir(), "graph.sqlite"))
					require.NoError(t, err)
					t.Cleanup(func() { require.NoError(t, s.Close()) })
					g = s
				}
				path := module + "/pkg"
				g.AddNode(&graph.Node{ID: "a/main.go", Kind: graph.KindFile, FilePath: "a/main.go", RepoPrefix: "a", Language: "go"})
				g.AddNode(&graph.Node{ID: "a/main.go::import::" + path, Kind: graph.KindImport, FilePath: "a/main.go", RepoPrefix: "a", Language: "go", StartLine: 2, Meta: map[string]any{"path": path}})
				g.AddEdge(&graph.Edge{From: "a/main.go", To: "unresolved::import::" + path, Kind: graph.EdgeImports, FilePath: "a/main.go", Line: 2})
				bFile := "b" + strings.TrimPrefix(path, "example.com/lib") + "/lib.go"
				for _, file := range []string{bFile, "d/pkg/lib.go"} {
					g.AddNode(&graph.Node{ID: file, Kind: graph.KindFile, FilePath: file, RepoPrefix: graph.RepoPrefixOfID(file), Language: "go"})
				}
				cr := NewCrossRepo(g)
				cr.SetGoModules(map[string]string{"b": "example.com/lib", "d": module})
				require.NoError(t, cr.RetryGoModuleDeps(t.Context()))
				check := func(stats *CrossRepoStats) {
					t.Helper()
					edges := g.GetOutEdges("a/main.go")
					require.Len(t, edges, 1, "an import bound to the longer module must not be restored again")
					assert.Equal(t, "d/pkg/lib.go", edges[0].To)
					assert.Equal(t, graph.EdgeImports, edges[0].Kind)
					assert.Equal(t, 2, edges[0].Line)
					assert.True(t, edges[0].CrossRepo)
					if stats != nil {
						assert.Zero(t, stats.Resolved)
						assert.Zero(t, stats.CrossRepoEdges)
						assert.Empty(t, stats.ByRepo)
					}
				}
				check(nil)
				for range 2 {
					stats, err := cr.ResolveAllContext(t.Context())
					require.NoError(t, err)
					check(stats)
					g.EvictFile(bFile)
					g.AddNode(&graph.Node{ID: bFile, Kind: graph.KindFile, FilePath: bFile, RepoPrefix: "b", Language: "go"})
					check(cr.ResolveMutationFrontiers(nil, nil, []string{bFile}))
				}
			})
		}
	}
}

func TestGoModuleImportRestoreLookupBudget(t *testing.T) {
	for _, tc := range []struct {
		name, target string
		line, want   int
	}{
		{"bound", "b/lib.go", 2, 1},
		{"missing", "", 2, 1},
		{"unresolved", "unresolved::import::example.com/lib", 2, 1},
		{"external", "external::example.com/lib", 2, 1},
		{"dep", "dep::example.com/lib", 2, 2},
		{"unresolved_dep", "unresolved::dep::example.com/lib", 2, 2},
		{"external_call_dep", "external-call::dep::example.com/lib", 2, 2},
		{"other_line_dep", "dep::example.com/lib", 3, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			g := &goImportCountingStore{Store: graph.New()}
			g.AddNode(&graph.Node{ID: "a/main.go", Kind: graph.KindFile, FilePath: "a/main.go", RepoPrefix: "a", Language: "go"})
			g.AddNode(&graph.Node{ID: "a/main.go::import::example.com/lib", Kind: graph.KindImport, FilePath: "a/main.go", RepoPrefix: "a", Language: "go", StartLine: 2, Meta: map[string]any{"path": "example.com/lib"}})
			g.AddNode(&graph.Node{ID: "b/lib.go", Kind: graph.KindFile, FilePath: "b/lib.go", RepoPrefix: "b", Language: "go"})
			g.AddEdge(&graph.Edge{From: "a/main.go", To: "unresolved::import::example.com/lib", Kind: graph.EdgeImports, FilePath: "a/main.go", Line: 2})
			cr := NewCrossRepo(g)
			cr.SetGoModules(map[string]string{"b": "example.com/lib"})
			require.NoError(t, cr.RetryGoModuleDeps(t.Context()))
			g.RemoveEdge("a/main.go", "b/lib.go", graph.EdgeImports)
			if tc.target != "" {
				g.AddEdge(&graph.Edge{From: "a/main.go", To: tc.target, Kind: graph.EdgeImports, FilePath: "a/main.go", Line: tc.line})
			}
			g.hydrated = 0
			_, err := cr.ResolveAllContext(t.Context())
			require.NoError(t, err)
			assert.LessOrEqual(t, g.hydrated, tc.want, "only a same-line dep terminal may need restore payloads in addition to the rebind's single load")
		})
	}
}

// Given import rows above the shared admission ceiling, the incoming leg
// refuses before either parked-edge binding or legacy recovery changes rows.
func TestGoModuleImportIncomingAdmissionRefused(t *testing.T) {
	for _, recovery := range []bool{false, true} {
		t.Run(fmt.Sprint(recovery), func(t *testing.T) {
			source := &graph.Node{ID: "a/main.go", Name: "main.go", Kind: graph.KindFile, FilePath: "a/main.go", RepoPrefix: "a", Language: "go"}
			provider := &graph.Node{ID: "b/lib.go", Name: "lib.go", Kind: graph.KindFile, FilePath: "b/lib.go", RepoPrefix: "b", Language: "go"}
			edge := &graph.Edge{From: source.ID, To: "unresolved::import::example.org/lib", Kind: graph.EdgeImports, FilePath: source.FilePath, Meta: map[string]any{"go_import": "example.org/lib"}}
			g := &goModuleAdmissionStore{Store: graph.New(), edge: edge}
			g.AddBatch([]*graph.Node{source, provider}, []*graph.Edge{edge})
			modules := map[string]string{"a": "example.org/app", "b": "example.org/lib"}
			state := &GoModuleDepRetry{modules: modules, complete: true, revision: 1, packageModules: modules, packageRevision: 1, importsSeeded: true}
			if recovery {
				state.imports = map[string]map[string]struct{}{"b": {}}
				for i := 0; i <= graph.MaxIncomingSourceCandidateRows; i++ {
					state.imports["b"][fmt.Sprint(i)] = struct{}{}
				}
			}
			cr := NewCrossRepo(g)
			cr.SetGoModules(modules)
			cr.SetGoModuleDepRetry(state, 1)
			_, admission := cr.ResolveMutationFrontiersBounded(nil, nil, []string{provider.FilePath})
			require.True(t, admission.Refused)
			var limit *graph.BoundedLocalizationLimitError
			require.ErrorAs(t, admission.Refusal, &limit)
			assert.Equal(t, "unresolved::import::example.org/lib", g.GetOutEdges(source.ID)[0].To)
		})
	}
}

func TestGoModuleImportFallbackStatsAreUnresolved(t *testing.T) {
	for _, target := range []string{"dep::example.org/lib", "external::example.org/lib"} {
		for _, priorCount := range []string{"none", "unresolved", "resolved"} {
			t.Run(target+"/"+priorCount, func(t *testing.T) {
				g := graph.New()
				old := &graph.Edge{From: "a/main.go", To: "b/lib.go", Kind: graph.EdgeImports, FilePath: "a/main.go", Line: 2}
				g.AddEdge(old)
				fresh := *old
				fresh.To = target
				stats := &CrossRepoStats{ByRepo: map[string]int{}}
				counted := map[graph.EdgeIdentity]bool{}
				if priorCount != "none" {
					resolved := priorCount == "resolved"
					counted[graph.EdgeIdentityFor(old)] = resolved
					if resolved {
						stats.Resolved = 1
					} else {
						stats.Unresolved = 1
					}
				}
				NewCrossRepo(g).applyGoModuleImportReindexes([]graph.EdgeReindex{{Edge: &fresh, OldTo: old.To}}, stats, counted)
				require.Zero(t, stats.Resolved)
				require.Equal(t, 1, stats.Unresolved)
				require.Zero(t, stats.CrossRepoEdges)
			})
		}
	}
}

func TestInvalidatePackageRepoKeepsOtherProviders(t *testing.T) {
	// Given one shared retry object with independent provider projections.
	state := &GoModuleDepRetry{complete: true, packages: map[string]map[string]graph.FileNodeIdentity{
		"b": {"b": {ID: "b/lib.go", FilePath: "b/lib.go", RepoPrefix: "b"}},
		"c": {"c": {ID: "c/lib.go", FilePath: "c/lib.go", RepoPrefix: "c"}},
	}}
	// When only C's projection is invalidated.
	state.InvalidatePackageRepo("c")
	// Then B's cached identity and the shared retry state are preserved.
	require.NotContains(t, state.packages, "c")
	require.Equal(t, graph.FileNodeIdentity{ID: "b/lib.go", FilePath: "b/lib.go", RepoPrefix: "b"}, state.packages["b"]["b"])
	require.True(t, state.complete, "package invalidation must not replace the terminal-sweep policy")
}

func TestGoModuleImportRestorePreservesVendoredBinding(t *testing.T) {
	for _, backend := range []string{"memory", "sqlite"} {
		t.Run(backend, func(t *testing.T) {
			g := goImportReviewStore(t, backend)
			for _, file := range []string{"consumer/main.go", "consumer/vendor/example.org/lib/lib.go", "provider/lib.go"} {
				g.AddNode(&graph.Node{ID: file, FilePath: file, Kind: graph.KindFile, RepoPrefix: graph.RepoPrefixOfID(file), Language: "go"})
			}
			g.AddNode(&graph.Node{ID: "consumer/main.go::import::example.org/lib", FilePath: "consumer/main.go", StartLine: 2, Kind: graph.KindImport, RepoPrefix: "consumer", Language: "go", Meta: map[string]any{"path": "example.org/lib"}})
			g.AddEdge(&graph.Edge{From: "consumer/main.go", FilePath: "consumer/main.go", To: "consumer/vendor/example.org/lib/lib.go", Kind: graph.EdgeImports, Line: 2, Origin: graph.OriginASTResolved})
			cr := NewCrossRepo(g)
			cr.SetGoModules(map[string]string{"provider": "example.org/lib"})
			for pass := 0; pass < 2; pass++ {
				_, err := cr.ResolveAllContext(t.Context())
				require.NoError(t, err)
				rows := outEdgesOfKind(g, "consumer/main.go", graph.EdgeImports)
				require.Len(t, rows, 1)
				require.Equal(t, "consumer/vendor/example.org/lib/lib.go", rows[0].To)
				require.False(t, rows[0].CrossRepo)
			}
		})
	}
}

func TestGoModuleImportUsesBatchedFileAndStatsNodes(t *testing.T) {
	for _, backend := range []string{"memory", "sqlite"} {
		t.Run(backend, func(t *testing.T) {
			g := &resolverBatchCountingStore{Store: goImportReviewStore(t, backend)}
			g.AddBatch([]*graph.Node{{ID: "consumer/main.go", FilePath: "consumer/main.go", Kind: graph.KindFile, RepoPrefix: "consumer", Language: "go"}, {ID: "provider/lib.go", FilePath: "provider/lib.go", Kind: graph.KindFile, RepoPrefix: "provider", Language: "go"}}, nil)
			const count = 64
			for i := 0; i < count; i++ {
				g.AddEdge(&graph.Edge{From: "consumer/main.go", FilePath: "consumer/main.go", To: "external::example.org/lib", Kind: graph.EdgeImports, Line: i + 1})
			}
			cr := NewCrossRepo(g)
			cr.SetGoModules(map[string]string{"provider": "example.org/lib"})
			for pass := 0; pass < 2; pass++ {
				g.getNodeCalls, g.getFileNodesByPathsCalls = 0, 0
				stats := &CrossRepoStats{ByRepo: map[string]int{}}
				require.NoError(t, cr.resolveGoModuleImports(t.Context(), []string{"consumer/main.go"}, stats, nil))
				require.Zero(t, g.getNodeCalls, "reuse loaded caller files and batch statistics targets")
				require.Equal(t, 1, g.getFileNodesByPathsCalls)
				rows := outEdgesOfKind(g, "consumer/main.go", graph.EdgeImports)
				require.Len(t, rows, count)
				for _, e := range rows {
					require.Equal(t, "provider/lib.go", e.To)
				}
				if pass == 0 {
					require.Equal(t, count, stats.ByRepo["provider"])
				} else {
					require.Zero(t, stats.Resolved)
					require.Empty(t, stats.ByRepo)
				}
			}
		})
	}
}

func TestGoModuleImportCounterExactBindingKinds(t *testing.T) {
	g := &goImportCountingStore{Store: graph.New()}
	for range g.RepoNodeIdentitiesSeq(nil, graph.KindFunction, graph.KindType, graph.KindInterface, graph.KindFile) {
	}
	require.Equal(t, 1, g.bindingRechecks)
	g.bindingRechecks = 0
	for range g.RepoNodeIdentitiesSeq(nil, graph.KindFunction, graph.KindType, graph.KindInterface, graph.KindImport) {
	}
	require.Zero(t, g.bindingRechecks, "four unrelated kinds must not count as the binding recheck")
}
