package resolver

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/graph/store_sqlite"
)

func TestCrossRepoTrackedNpmPackage(t *testing.T) {
	for _, backend := range []string{"memory", "sqlite"} {
		for _, target := range []string{"external::p", "unresolved::import::p"} {
			for _, mode := range []string{"all", "repo", "file", "mutation", "import only"} {
				t.Run(backend+"/"+target+"/"+mode, func(t *testing.T) {
					var g graph.Store = graph.New()
					if backend == "sqlite" {
						store, err := store_sqlite.Open(filepath.Join(t.TempDir(), "graph.sqlite"))
						require.NoError(t, err)
						t.Cleanup(func() { require.NoError(t, store.Close()) })
						g = store
					}
					g.AddBatch([]*graph.Node{
						{ID: "app/src/app.ts", Kind: graph.KindFile, FilePath: "app/src/app.ts", RepoPrefix: "app", Language: "typescript"},
						{ID: "app/src/app.ts::run", Kind: graph.KindFunction, Name: "run", FilePath: "app/src/app.ts", RepoPrefix: "app", Language: "typescript"},
						{ID: "lib/src/index.ts", Kind: graph.KindFile, FilePath: "lib/src/index.ts", RepoPrefix: "lib", Language: "typescript"},
						{ID: "lib/src/index.ts::f", Kind: graph.KindFunction, Name: "f", FilePath: "lib/src/index.ts", RepoPrefix: "lib", Language: "typescript"},
					}, nil)
					imp := &graph.Edge{From: "app/src/app.ts", To: target, Kind: graph.EdgeImports, FilePath: "app/src/app.ts", Line: 1, Meta: map[string]any{"test_marker": "preserved"}}
					g.AddEdge(imp)
					if mode != "import only" {
						g.AddEdge(&graph.Edge{From: "app/src/app.ts::run", To: "unresolved::f", Kind: graph.EdgeCalls, FilePath: "app/src/app.ts", Line: 2})
					}
					cr := NewCrossRepo(g)
					cr.SetNpmDependencyLookup(func(_, spec string) bool { return spec == "p" })
					cr.SetNpmPackageLookup(func(spec string) map[string][]string {
						if spec != "p" {
							return nil
						}
						return map[string][]string{"lib": {"lib/src/index.ts", "lib"}}
					})
					var stats *CrossRepoStats
					switch mode {
					case "all", "import only":
						stats = cr.ResolveAll()
					case "repo":
						stats = cr.ResolveForRepo("app")
					case "file":
						stats = cr.ResolveForFile("app", "src/app.ts")
					case "mutation":
						stats = cr.ResolveFilesAndIncoming([]string{"app/src/app.ts"})
					}
					wantResolved := 2
					if mode == "import only" {
						wantResolved = 1
					}
					assert.Equal(t, wantResolved, stats.Resolved)
					assert.Equal(t, wantResolved, stats.CrossRepoEdges)
					imports := g.GetOutEdges("app/src/app.ts")
					require.Len(t, imports, 1)
					var found bool
					for _, e := range imports {
						if e.Kind != graph.EdgeImports {
							continue
						}
						found = true
						assert.Equal(t, "lib/src/index.ts", e.To)
						assert.True(t, e.CrossRepo)
						assert.Equal(t, "preserved", e.Meta["test_marker"])
					}
					assert.True(t, found)
					if mode != "import only" {
						var calls []*graph.Edge
						for _, e := range g.GetOutEdges("app/src/app.ts::run") {
							if e.Kind == graph.EdgeCalls {
								calls = append(calls, e)
							}
						}
						require.Len(t, calls, 1)
						assert.Equal(t, "lib/src/index.ts::f", calls[0].To)
						assert.True(t, calls[0].CrossRepo)
					}
					// A second pass is idempotent and leaves no old target bucket.
					assert.Zero(t, cr.ResolveAll().Resolved)
					assert.Empty(t, g.GetInEdges(target))
				})
			}
		}
	}
}

func TestCrossRepoTrackedNpmPackageAmbiguousOrUndeclared(t *testing.T) {
	for _, declared := range []bool{false, true} {
		t.Run(map[bool]string{false: "undeclared", true: "ambiguous"}[declared], func(t *testing.T) {
			g := graph.New()
			g.AddNode(&graph.Node{ID: "app/a.ts", Kind: graph.KindFile, FilePath: "app/a.ts", RepoPrefix: "app", Language: "typescript"})
			g.AddNode(&graph.Node{ID: "one/index.ts", Kind: graph.KindFile, FilePath: "one/index.ts", RepoPrefix: "one", Language: "typescript"})
			g.AddNode(&graph.Node{ID: "two/index.ts", Kind: graph.KindFile, FilePath: "two/index.ts", RepoPrefix: "two", Language: "typescript"})
			imp := &graph.Edge{From: "app/a.ts", To: "external::p", Kind: graph.EdgeImports, FilePath: "app/a.ts", Line: 1}
			g.AddEdge(imp)
			cr := NewCrossRepo(g)
			cr.SetNpmDependencyLookup(func(_, _ string) bool { return declared })
			cr.SetNpmPackageLookup(func(_ string) map[string][]string {
				targets := map[string][]string{"one": {"one/index.ts"}}
				if declared {
					targets["two"] = []string{"two/index.ts"}
				}
				return targets
			})
			assert.Zero(t, cr.ResolveAll().Resolved)
			assert.Equal(t, "external::p", imp.To)
			assert.False(t, imp.CrossRepo)
		})
	}
}

func TestCrossRepoTrackedNpmPackageBoundaries(t *testing.T) {
	for _, tc := range []struct {
		name, visibility string
		dirs             map[string][]string
		want             string
	}{
		{"own package missing index", "private", map[string][]string{"app/packages/p": {"app/packages/p/missing"}, "one": {"one/index.ts"}}, "external::p::f"},
		{"own root package", "private", map[string][]string{"app": {"app/missing"}, "one": {"one/index.ts"}}, "external::p::f"},
		{"same workspace ambiguous", "private", map[string][]string{"one": {"one/index.ts"}, "two": {"two/index.ts"}}, "external::p::f"},
		{"private symbol", "private", map[string][]string{"one": {"one/index.ts"}}, "one/index.ts"},
		{"public symbol", "public", map[string][]string{"one": {"one/index.ts"}}, "one/index.ts::f"},
		{"unknown visibility", "", map[string][]string{"one": {"one/index.ts"}}, "one/index.ts"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			g := graph.New()
			g.AddNode(&graph.Node{ID: "app/a.ts", Kind: graph.KindFile, FilePath: "app/a.ts", RepoPrefix: "app", WorkspaceID: "shared"})
			for _, repo := range []string{"one", "two"} {
				file := repo + "/index.ts"
				g.AddNode(&graph.Node{ID: file, Kind: graph.KindFile, FilePath: file, RepoPrefix: repo, WorkspaceID: "shared"})
				g.AddNode(&graph.Node{ID: file + "::f", Kind: graph.KindFunction, Name: "f", FilePath: file, RepoPrefix: repo, WorkspaceID: "shared", Meta: map[string]any{"visibility": tc.visibility}})
			}
			imp := &graph.Edge{From: "app/a.ts", To: "external::p::f", Kind: graph.EdgeImports, FilePath: "app/a.ts", Line: 1}
			g.AddEdge(imp)
			cr := NewCrossRepo(g)
			cr.SetNpmDependencyLookup(func(_, _ string) bool { return true })
			cr.SetNpmPackageLookup(func(_ string) map[string][]string { return tc.dirs })
			cr.ResolveAll()
			assert.Equal(t, tc.want, imp.To)
			assert.Equal(t, tc.want != "external::p::f", imp.CrossRepo)
		})
	}
}

type npmProbeCountingStore struct {
	graph.Store
	providerReads int
}

func (g *npmProbeCountingStore) GetNode(id string) *graph.Node {
	if len(id) >= 4 && id[:4] == "lib/" {
		g.providerReads++
	}
	return g.Store.GetNode(id)
}

func TestCrossRepoTrackedNpmPackageProbeMemo(t *testing.T) {
	for _, declared := range []bool{false, true} {
		t.Run(map[bool]string{false: "undeclared", true: "missing target"}[declared], func(t *testing.T) {
			g := &npmProbeCountingStore{Store: graph.New()}
			g.AddNode(&graph.Node{ID: "app/a.ts", Kind: graph.KindFile, FilePath: "app/a.ts", RepoPrefix: "app"})
			for line := 1; line <= 100; line++ {
				g.AddEdge(&graph.Edge{From: "app/a.ts", To: "external::p", Kind: graph.EdgeImports, FilePath: "app/a.ts", Line: line})
			}
			cr := NewCrossRepo(g)
			cr.SetNpmDependencyLookup(func(_, _ string) bool { return declared })
			cr.SetNpmPackageLookup(func(_ string) map[string][]string { return map[string][]string{"lib": {"lib/missing"}} })
			for pass := 0; pass < 2; pass++ {
				g.providerReads = 0
				cr.ResolveAll()
				t.Logf("pass=%d declared=%v provider GetNode calls=%d", pass, declared, g.providerReads)
				if declared {
					assert.Equal(t, 2*len(jsTSImportExts), g.providerReads)
				} else {
					assert.Zero(t, g.providerReads)
				}
			}
		})
	}
}

type npmHydrationCountingStore struct {
	graph.Store
	sites, maxSites int
	reads           map[string]int
}

func (g *npmHydrationCountingStore) GetEdgeCandidates(endpoints []graph.EdgeEndpoint, sites []graph.EdgeSite) graph.EdgeCandidateSet {
	g.sites += len(sites)
	g.maxSites = max(g.maxSites, len(sites))
	return g.Store.GetEdgeCandidates(endpoints, sites)
}

func (g *npmHydrationCountingStore) GetNode(id string) *graph.Node {
	g.reads[id]++
	return g.Store.GetNode(id)
}

func TestCrossRepoTrackedNpmPackageHydration(t *testing.T) {
	for _, backend := range []string{"memory", "sqlite"} {
		for _, mode := range []string{"undeclared", "alias", "declared"} {
			t.Run(backend+"/"+mode, func(t *testing.T) {
				var store graph.Store = graph.New()
				if backend == "sqlite" {
					db, err := store_sqlite.Open(filepath.Join(t.TempDir(), "graph.sqlite"))
					require.NoError(t, err)
					t.Cleanup(func() { require.NoError(t, db.Close()) })
					store = db
				}
				g := &npmHydrationCountingStore{Store: store, reads: map[string]int{}}
				g.AddNode(&graph.Node{ID: "app/a.ts", Kind: graph.KindFile, RepoPrefix: "app"})
				g.AddNode(&graph.Node{ID: "lib/index.ts", Kind: graph.KindFile, RepoPrefix: "lib"})
				g.AddNode(&graph.Node{ID: "lib/index.ts::f", Kind: graph.KindFunction, RepoPrefix: "lib", Meta: map[string]any{"visibility": "public"}})
				var edges []*graph.Edge
				for line := 1; line <= resolvePendingPageRows+3; line++ {
					e := &graph.Edge{From: "app/a.ts", To: "external::p::f", Kind: graph.EdgeImports, FilePath: "app/a.ts", Line: line}
					edges = append(edges, e)
					g.AddEdge(e)
				}
				cr := NewCrossRepo(g)
				cr.SetNpmDependencyLookup(func(_, _ string) bool { return mode != "undeclared" })
				if mode == "alias" {
					cr.SetNpmAliasResolver(func(_, _ string) string { return "other" })
				}
				lookups := 0
				cr.SetNpmPackageLookup(func(_ string) map[string][]string { lookups++; return map[string][]string{"lib": {"lib/index.ts"}} })
				stats := &CrossRepoStats{ByRepo: map[string]int{}}
				require.NoError(t, cr.resolveTrackedNpmImports(context.Background(), graph.EdgesLightSeq(store, graph.EdgeImports), stats))
				if mode != "declared" {
					assert.Zero(t, g.sites)
					assert.Zero(t, lookups, "rejected imports must not trigger manifest lookup")
					assert.Zero(t, stats.Resolved)
				} else {
					assert.Equal(t, len(edges), g.sites)
					assert.LessOrEqual(t, g.maxSites, resolvePendingPageRows)
					assert.Equal(t, len(edges), stats.Resolved)
					assert.Equal(t, 1, g.reads["app/a.ts"])
					assert.Equal(t, 1, g.reads["lib/index.ts::f"])
				}
			})
		}
	}
}

// Cancellation must stop the npm prepass, not only the residual resolver.
type npmCancelHydrationStore struct {
	graph.Store
	cancel context.CancelFunc
	pages  int
}

func (g *npmCancelHydrationStore) GetEdgeCandidates(endpoints []graph.EdgeEndpoint, sites []graph.EdgeSite) graph.EdgeCandidateSet {
	g.pages++
	g.cancel()
	return g.Store.GetEdgeCandidates(endpoints, sites)
}

func TestCrossRepoTrackedNpmPackageCancellation(t *testing.T) {
	for _, phase := range []string{"before", "scan", "hydrate"} {
		t.Run(phase, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			g := &npmCancelHydrationStore{Store: graph.New(), cancel: cancel}
			g.AddNode(&graph.Node{ID: "app/a.ts", Kind: graph.KindFile, RepoPrefix: "app"})
			g.AddNode(&graph.Node{ID: "lib/index.ts", Kind: graph.KindFile, RepoPrefix: "lib"})
			for line := 1; line <= resolvePendingPageRows+3; line++ {
				g.AddEdge(&graph.Edge{From: "app/a.ts", To: "external::p", Kind: graph.EdgeImports, FilePath: "app/a.ts", Line: line})
			}
			cr := NewCrossRepo(g)
			cr.SetNpmDependencyLookup(func(_, _ string) bool { return true })
			lookups := 0
			cr.SetNpmPackageLookup(func(_ string) map[string][]string {
				lookups++
				if phase == "scan" {
					cancel()
				}
				return map[string][]string{"lib": {"lib/index.ts"}}
			})
			if phase == "before" {
				cancel()
			}
			stats, err := cr.ResolveAllContext(ctx)
			require.ErrorIs(t, err, context.Canceled)
			if phase == "hydrate" {
				assert.Equal(t, 1, g.pages, "must not hydrate another page after cancellation")
				assert.LessOrEqual(t, stats.Resolved, resolvePendingPageRows)
			} else {
				assert.Zero(t, g.pages)
				assert.Zero(t, stats.Resolved)
				if phase == "scan" {
					assert.Equal(t, 1, lookups, "must stop scanning after cancellation")
				} else {
					assert.Zero(t, lookups)
				}
			}
		})
	}
}
