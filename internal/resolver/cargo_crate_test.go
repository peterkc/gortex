package resolver

import (
	"context"
	"fmt"
	"iter"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/graph/store_sqlite"
)

func TestCargoImportUntrackedSkipsNodeReads(t *testing.T) {
	g := &resolverBatchCountingStore{Store: graph.New()}
	cr := NewCrossRepo(g)
	cr.SetCargoCrateLookup(func(_, _ string) []CargoCrate { return nil })
	e := &graph.Edge{From: "app/src/lib.rs", To: "external::serde/Item", Kind: graph.EdgeImports, FilePath: "app/src/lib.rs"}
	node, module := cr.cargoImportTarget(e, "serde/Item")
	assert.Nil(t, node)
	assert.Empty(t, module)
	assert.Zero(t, g.getNodeCalls, "unknown crates must be rejected before caller node reads")
}

type cargoPagingStore struct {
	graph.Store
	fullScans, lightScans, pages, maxSites int
	cursorOpen                             bool
}

func (s *cargoPagingStore) EdgesByKind(kind graph.EdgeKind) iter.Seq[*graph.Edge] {
	s.fullScans++
	return s.Store.EdgesByKind(kind)
}

func (s *cargoPagingStore) EdgesLightSeq(kinds ...graph.EdgeKind) iter.Seq[*graph.Edge] {
	s.lightScans++
	return func(yield func(*graph.Edge) bool) {
		s.cursorOpen = true
		defer func() { s.cursorOpen = false }()
		for e := range graph.EdgesLightSeq(s.Store, kinds...) {
			if !yield(e) {
				return
			}
		}
	}
}

func (s *cargoPagingStore) GetEdgeCandidates(endpoints []graph.EdgeEndpoint, sites []graph.EdgeSite) graph.EdgeCandidateSet {
	if s.cursorOpen {
		panic("hydration while light cursor is open")
	}
	s.pages++
	s.maxSites = max(s.maxSites, len(sites))
	return s.Store.GetEdgeCandidates(endpoints, sites)
}

func TestCargoImportLightPaging(t *testing.T) {
	for _, backend := range []string{"memory", "sqlite"} {
		t.Run(backend, func(t *testing.T) {
			var store graph.Store = graph.New()
			if backend == "sqlite" {
				db, err := store_sqlite.Open(filepath.Join(t.TempDir(), "graph.sqlite"))
				require.NoError(t, err)
				t.Cleanup(func() { require.NoError(t, db.Close()) })
				store = db
			}
			g := &cargoPagingStore{Store: store}
			const count = resolvePendingPageRows + 1
			edges := make([]*graph.Edge, 0, count+1)
			for line := 1; line <= count; line++ {
				edges = append(edges, &graph.Edge{From: "app/src/lib.rs", To: "external::other_crate/Item", Kind: graph.EdgeImports, FilePath: "app/src/lib.rs", Line: line, Meta: map[string]any{"marker": "preserved"}})
			}
			edges = append(edges, &graph.Edge{From: "app/src/lib.rs", To: "external::serde/Item", Kind: graph.EdgeImports, FilePath: "app/src/lib.rs", Line: count + 1})
			g.AddBatch([]*graph.Node{
				{ID: "app/src/lib.rs", Kind: graph.KindFile, FilePath: "app/src/lib.rs", RepoPrefix: "app", Language: "rust"},
				{ID: "lib/src/lib.rs", Kind: graph.KindFile, FilePath: "lib/src/lib.rs", RepoPrefix: "lib", Language: "rust"},
			}, edges)
			cr := NewCrossRepo(g)
			cr.SetCargoCrateLookup(func(_, name string) []CargoCrate {
				if name != "other_crate" {
					return nil
				}
				return []CargoCrate{{Root: "lib/src/lib.rs", Name: "other_crate"}}
			})
			stats := &CrossRepoStats{ByRepo: map[string]int{}}
			_, err := cr.resolveCargoImports(context.Background(), graph.EdgesLightSeq(g, graph.EdgeImports), stats)
			require.NoError(t, err)
			assert.Zero(t, g.fullScans)
			assert.Equal(t, 1, g.lightScans)
			assert.Equal(t, 2, g.pages)
			assert.Equal(t, resolvePendingPageRows, g.maxSites)
			assert.Equal(t, count, stats.Resolved)
			imports := g.GetOutEdges("app/src/lib.rs")
			require.Len(t, imports, count+1)
			for _, edge := range imports {
				if edge.Line == count+1 {
					assert.Equal(t, "external::serde/Item", edge.To)
					assert.False(t, edge.CrossRepo)
					continue
				}
				assert.Equal(t, "lib/src/lib.rs", edge.To)
				assert.True(t, edge.CrossRepo)
				assert.Equal(t, "preserved", edge.Meta["marker"])
			}
		})
	}
}

func TestCargoGroupedAliasedUses(t *testing.T) {
	for _, backend := range []string{"memory", "sqlite"} {
		for _, use := range []string{"other_crate/util/{action, Other}", "other_crate/util/action as act"} {
			t.Run(backend+"/"+use, func(t *testing.T) {
				g := goImportReviewStore(t, backend)
				file, provider := "consumer/src/lib.rs", "provider/src/util.rs"
				for _, path := range []string{file, "provider/src/lib.rs", provider} {
					g.AddNode(&graph.Node{ID: path, FilePath: path, Kind: graph.KindFile, RepoPrefix: graph.RepoPrefixOfID(path), Language: "rust"})
				}
				g.AddNode(&graph.Node{ID: provider + "::action", Name: "action", FilePath: provider, Kind: graph.KindFunction, RepoPrefix: "provider", Language: "rust"})
				local := "action"
				if strings.Contains(use, " as ") {
					local = "act"
				}
				g.AddEdge(&graph.Edge{From: file, FilePath: file, To: "external::" + use, Kind: graph.EdgeImports, Line: 1})
				g.AddEdge(&graph.Edge{From: file, FilePath: file, To: "unresolved::" + local, Kind: graph.EdgeCalls, Line: 2})
				cr := NewCrossRepo(g)
				cr.SetCargoCrateLookup(func(string, string) []CargoCrate {
					return []CargoCrate{{Root: "provider/src/lib.rs", Name: "other_crate"}}
				})
				_, err := cr.ResolveAllContext(t.Context())
				require.NoError(t, err)
				require.Equal(t, provider, outEdgesOfKind(g, file, graph.EdgeImports)[0].To)
				require.Equal(t, provider+"::action", outEdgesOfKind(g, file, graph.EdgeCalls)[0].To)
			})
		}
	}
}

func TestScopedBridgeImportHydration(t *testing.T) {
	g := &cargoPagingStore{Store: graph.New()}
	for i := 0; i < 12; i++ {
		file := fmt.Sprintf("app/%d.rs", i)
		g.AddNode(&graph.Node{ID: file, FilePath: file, Kind: graph.KindFile, RepoPrefix: "app", Language: "rust"})
		g.AddEdge(&graph.Edge{From: file, FilePath: file, To: "lib/src/lib.rs", Kind: graph.EdgeImports, Line: 1, CrossRepo: true, Meta: map[string]any{CargoImportStubMeta: "external::other/action"}})
	}
	g.AddNode(&graph.Node{ID: "lib/src/lib.rs", FilePath: "lib/src/lib.rs", Kind: graph.KindFile, RepoPrefix: "lib", Language: "rust"})
	g.AddEdge(&graph.Edge{From: "app/0.rs", FilePath: "app/0.rs", To: "unresolved::action", Kind: graph.EdgeCalls, Line: 2})
	cr := NewCrossRepo(g)
	cr.ResolveForFile("app", "0.rs")
	require.Equal(t, 1, g.maxSites, "only the pending caller's import metadata may be hydrated")
	g.maxSites = 0
	_, err := cr.ResolveAllContext(t.Context())
	require.NoError(t, err)
	require.Equal(t, 12, g.maxSites, "full passes retain corpus-wide import evidence")
}

func TestCargoGroupedUsesRejectNestedPathDecoys(t *testing.T) {
	for _, backend := range []string{"memory", "sqlite"} {
		for _, spec := range []string{"other/util/{action, sub/helper}", "other/{util/action, net/fetch}", "other/{self, action}"} {
			t.Run(backend+"/"+spec, func(t *testing.T) {
				// Given a real nested item and a same-named decoy in the group's linked file.
				g := goImportReviewStore(t, backend)
				file, root, module := "consumer/src/lib.rs", "provider/src/lib.rs", "provider/src/util.rs"
				paths := []string{file, root, module, "provider/src/util/sub.rs", "provider/src/net.rs"}
				for _, path := range paths {
					g.AddNode(&graph.Node{ID: path, FilePath: path, Kind: graph.KindFile, RepoPrefix: graph.RepoPrefixOfID(path), Language: "rust"})
				}
				decoy, name := module, "helper"
				if strings.HasPrefix(spec, "other/{") {
					decoy, name = root, "fetch"
				}
				if strings.Contains(spec, "{self,") {
					name = "action"
				}
				for _, path := range []string{decoy, "provider/src/util/sub.rs", "provider/src/net.rs"} {
					g.AddNode(&graph.Node{ID: path + "::" + name, FilePath: path, Name: name, Kind: graph.KindFunction, RepoPrefix: "provider", Language: "rust"})
				}
				g.AddNode(&graph.Node{ID: module + "::action", FilePath: module, Name: "action", Kind: graph.KindFunction, RepoPrefix: "provider", Language: "rust"})
				g.AddEdge(&graph.Edge{From: file, FilePath: file, To: "external::" + spec, Kind: graph.EdgeImports, Line: 1})
				g.AddEdge(&graph.Edge{From: file, FilePath: file, To: "unresolved::" + name, Kind: graph.EdgeCalls, Line: 2})
				g.AddEdge(&graph.Edge{From: file, FilePath: file, To: "unresolved::action", Kind: graph.EdgeCalls, Line: 3})
				cr := NewCrossRepo(g)
				cr.SetCargoCrateLookup(func(string, string) []CargoCrate { return []CargoCrate{{Root: root, Name: "other"}} })
				// When the tracked bridge resolves the grouped import and its calls.
				_, err := cr.ResolveAllContext(t.Context())
				require.NoError(t, err)
				// Then the nested leaf never binds to a decoy in that file.
				calls := outEdgesOfKind(g, file, graph.EdgeCalls)
				require.Len(t, calls, 2)
				for _, call := range calls {
					if call.Line == 2 {
						if strings.Contains(spec, "{self,") {
							require.Equal(t, root+"::action", call.To)
						} else {
							require.Equal(t, "unresolved::"+name, call.To)
							require.False(t, call.CrossRepo)
						}
					}
					if call.Line == 3 && decoy == module {
						require.Equal(t, module+"::action", call.To)
					}
				}
			})
		}
	}
}

func TestScopedCallerFileUsesWarmedNodeCache(t *testing.T) {
	// Given many pending edges with one caller node.
	g := &resolverBatchCountingStore{Store: graph.New()}
	file, caller := "app/file.go", "app/file.go::run"
	g.AddNode(&graph.Node{ID: file, FilePath: file, Kind: graph.KindFile, RepoPrefix: "app", Language: "go"})
	g.AddNode(&graph.Node{ID: caller, FilePath: file, Kind: graph.KindFunction, RepoPrefix: "app", Language: "go"})
	var edges []*graph.Edge
	for i := 0; i < 30; i++ {
		edges = append(edges, &graph.Edge{From: caller, FilePath: file, To: "unresolved::missing", Kind: graph.EdgeCalls, Line: i + 1})
	}
	g.AddBatch(nil, edges)
	// When the scoped resolver builds its pending caller-file set.
	cr := NewCrossRepo(g)
	cr.resolveScopedLocked(edges)
	// Then the batched cache, not one GetNode per edge, supplies caller placement.
	require.Zero(t, g.getNodeCalls)
}

func TestCargoCallRequiresExactParentModule(t *testing.T) {
	for _, backend := range []string{"memory", "sqlite"} {
		for _, spec := range []string{"other/sys/{open}", "other/sys/open", "other/util/sys/{open}", "other/util/sys/open", "other/util/{open}", "other/util/open"} {
			t.Run(backend+"/"+spec, func(t *testing.T) {
				// Given a missing sys module and decoys in its ancestor files.
				g := goImportReviewStore(t, backend)
				file, root, module := "consumer/src/lib.rs", "provider/src/lib.rs", "provider/src/util.rs"
				for _, path := range []string{file, root, module} {
					g.AddNode(&graph.Node{ID: path, FilePath: path, Kind: graph.KindFile, RepoPrefix: graph.RepoPrefixOfID(path), Language: "rust"})
				}
				for _, path := range []string{root, module} {
					g.AddNode(&graph.Node{ID: path + "::open", FilePath: path, Name: "open", Kind: graph.KindFunction, RepoPrefix: "provider", Language: "rust"})
				}
				g.AddEdge(&graph.Edge{From: file, FilePath: file, To: "external::" + spec, Kind: graph.EdgeImports, Line: 1})
				g.AddEdge(&graph.Edge{From: file, FilePath: file, To: "unresolved::open", Kind: graph.EdgeCalls, Line: 2})
				cr := NewCrossRepo(g)
				cr.SetCargoCrateLookup(func(string, string) []CargoCrate { return []CargoCrate{{Root: root, Name: "other"}} })
				// When the import falls back to the nearest indexed module file.
				_, err := cr.ResolveAllContext(t.Context())
				require.NoError(t, err)
				// Then only an exact parent module authorizes its member call.
				imp := outEdgesOfKind(g, file, graph.EdgeImports)[0]
				matched := "other::util"
				if strings.HasPrefix(spec, "other/sys/") {
					matched = "other"
				}
				require.Equal(t, matched, imp.Meta[CargoImportModuleMeta])
				call := outEdgesOfKind(g, file, graph.EdgeCalls)[0]
				if strings.Contains(spec, "/sys/") {
					require.Equal(t, "unresolved::open", call.To)
					require.False(t, call.CrossRepo)
				} else {
					require.Equal(t, module+"::open", call.To)
					require.True(t, call.CrossRepo)
				}
			})
		}
	}
}

func TestCargoAliasedCallsWorkspacePolicy(t *testing.T) {
	for _, backend := range []string{"memory", "sqlite"} {
		for _, allowed := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/allowed=%v", backend, allowed), func(t *testing.T) {
				g := goImportReviewStore(t, backend)
				for _, file := range []string{"consumer/src/lib.rs", "provider/src/lib.rs", "provider/src/util.rs"} {
					ws := "private"
					if strings.HasPrefix(file, "consumer/") {
						ws = "shared"
					}
					g.AddNode(&graph.Node{ID: file, FilePath: file, Kind: graph.KindFile, RepoPrefix: graph.RepoPrefixOfID(file), WorkspaceID: ws, Language: "rust"})
				}
				g.AddNode(&graph.Node{ID: "provider/src/util.rs::helper", Name: "helper", FilePath: "provider/src/util.rs", Kind: graph.KindFunction, RepoPrefix: "provider", WorkspaceID: "private", Language: "rust"})
				g.AddEdge(&graph.Edge{From: "consumer/src/lib.rs", FilePath: "consumer/src/lib.rs", To: "external::oc/util/helper", Kind: graph.EdgeImports, Line: 1})
				g.AddEdge(&graph.Edge{From: "consumer/src/lib.rs", FilePath: "consumer/src/lib.rs", To: "unresolved::helper", Kind: graph.EdgeCalls, Line: 2})
				cr := NewCrossRepo(g)
				cr.SetCargoCrateLookup(func(_, alias string) []CargoCrate {
					if alias != "oc" {
						return nil
					}
					return []CargoCrate{{Root: "provider/src/lib.rs", Name: "other_crate"}}
				})
				module := "oc"
				if allowed {
					module = "other_crate"
				}
				cr.SetCrossWorkspaceDepLookup(func(string) []CrossWorkspaceDepRule {
					return []CrossWorkspaceDepRule{{Workspace: "private", Modules: []string{module}}}
				})
				for pass := 0; pass < 2; pass++ {
					_, err := cr.ResolveAllContext(t.Context())
					require.NoError(t, err)
					imports := outEdgesOfKind(g, "consumer/src/lib.rs", graph.EdgeImports)
					require.Len(t, imports, 1)
					require.Equal(t, allowed, imports[0].CrossRepo)
					calls := outEdgesOfKind(g, "consumer/src/lib.rs", graph.EdgeCalls)
					require.Len(t, calls, 1)
					require.Equal(t, allowed, calls[0].CrossRepo)
					if allowed {
						require.Equal(t, "provider/src/util.rs::helper", calls[0].To)
					}
				}
			})
		}
	}
}
