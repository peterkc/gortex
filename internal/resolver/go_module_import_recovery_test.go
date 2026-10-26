package resolver

import (
	"context"
	"fmt"
	"path/filepath"
	"sort"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/graph/store_sqlite"
)

func TestGoModuleImportBudgetDoesNotStarveCalls(t *testing.T) {
	for _, backend := range []string{"memory", "sqlite"} {
		for _, providerFirst := range []bool{false, true} {
			for _, restart := range []bool{false, true} {
				t.Run(fmt.Sprintf("%s/providerFirst=%v/restart=%v", backend, providerFirst, restart), func(t *testing.T) {
					newStore := func() graph.Store {
						t.Helper()
						if backend == "memory" {
							return graph.New()
						}
						db, err := store_sqlite.Open(filepath.Join(t.TempDir(), "graph.sqlite"))
						require.NoError(t, err)
						t.Cleanup(func() { require.NoError(t, db.Close()) })
						return db
					}
					g := newStore()
					modules := map[string]string{"a": "example.org/app", "b": "example.org/lib"}
					state := &GoModuleDepRetry{}
					resolver := func(store graph.Store, retry *GoModuleDepRetry) *CrossRepoResolver {
						cr := NewCrossRepo(store)
						cr.SetGoModules(modules)
						cr.SetGoModuleDepRetry(retry, 1)
						return cr
					}
					seed := func(store graph.Store) {
						t.Helper()
						if bulk, ok := store.(graph.BulkLoader); ok {
							bulk.BeginBulkLoad()
							defer func() { require.NoError(t, bulk.FlushBulk()) }()
						}
						var consumers []*graph.Node
						var edges []*graph.Edge
						for i := 0; i < graph.MaxIncomingSourceCandidateRows+1; i++ {
							f := fmt.Sprintf("a/f%d.go", i)
							consumers = append(consumers, &graph.Node{ID: f, Kind: graph.KindFile, FilePath: f, RepoPrefix: "a", Language: "go"}, &graph.Node{ID: f + "::import::example.org/lib", Kind: graph.KindImport, FilePath: f, RepoPrefix: "a", Language: "go", StartLine: 2, Meta: map[string]any{"path": "example.org/lib"}})
							edges = append(edges, &graph.Edge{From: f, To: "b/lib.go", Kind: graph.EdgeImports, FilePath: f, Line: 2, CrossRepo: true, Meta: map[string]any{"go_import": "example.org/lib", "go_import_fallback": "dep::example.org/lib"}})
						}
						consumers = append(consumers, &graph.Node{ID: "a/main.go::Caller", Name: "Caller", Kind: graph.KindFunction, FilePath: "a/main.go", RepoPrefix: "a", Language: "go"})
						consumers = append(consumers,
							&graph.Node{ID: "a/new.go", Kind: graph.KindFile, FilePath: "a/new.go", RepoPrefix: "a", Language: "go"},
							&graph.Node{ID: "a/new.go::import::example.org/lib/new", Kind: graph.KindImport, FilePath: "a/new.go", RepoPrefix: "a", Language: "go", StartLine: 2, Meta: map[string]any{"path": "example.org/lib/new"}})
						edges = append(edges, &graph.Edge{From: "a/new.go", To: "dep::example.org/lib", Kind: graph.EdgeImports, FilePath: "a/new.go", Line: 2})
						provider := &graph.Node{ID: "b/lib.go", Kind: graph.KindFile, FilePath: "b/lib.go", RepoPrefix: "b", Language: "go"}
						if providerFirst {
							store.AddNode(provider)
						}
						store.AddBatch(consumers, edges)
						if !providerFirst {
							store.AddNode(provider)
						}
					}
					seed(g)
					cr := resolver(g, state)
					require.NoError(t, cr.RetryGoModuleDeps(t.Context()))
					// The provider now adds the missing package and function.
					addNew := func(store graph.Store) {
						store.AddBatch([]*graph.Node{
							{ID: "b/new/new.go", Kind: graph.KindFile, FilePath: "b/new/new.go", RepoPrefix: "b", Language: "go"},
							{ID: "b/new/new.go::New", Name: "New", Kind: graph.KindFunction, FilePath: "b/new/new.go", RepoPrefix: "b", Language: "go"},
						}, []*graph.Edge{
							{From: "a/main.go::Caller", To: "unresolved::extern::example.org/lib/new::New", Kind: graph.EdgeCalls, FilePath: "a/main.go", Line: 3},
						})
					}
					addNew(g)
					if restart {
						state = &GoModuleDepRetry{}
					}
					cr = resolver(g, state)
					// When the import recovery exceeds its own ceiling, the call leg must still bind.
					_, admission := cr.ResolveMutationFrontiersBounded(nil, nil, []string{"b/new/new.go"})
					assert.False(t, admission.Refused, "import refusal is not a call admission refusal")
					var calls []*graph.Edge
					for _, e := range g.GetOutEdges("a/main.go::Caller") {
						if e.Kind == graph.EdgeCalls {
							calls = append(calls, e)
						}
					}
					require.Len(t, calls, 1)
					assert.Equal(t, "b/new/new.go::New", calls[0].To)
					// Refused imports remain eligible for the next full pass, not lost or marked complete.
					imports := g.GetOutEdges("a/f0.go")
					require.Len(t, imports, 1)
					assert.Equal(t, "b/lib.go", imports[0].To)
					if !restart {
						rows := g.GetOutEdges("a/new.go")
						require.Len(t, rows, 1)
						assert.Equal(t, "dep::example.org/lib", rows[0].To)
					}
					_, err := cr.ResolveAllContext(context.Background())
					require.NoError(t, err)
					cold := newStore()
					seed(cold)
					addNew(cold)
					_, err = resolver(cold, &GoModuleDepRetry{}).ResolveAllContext(t.Context())
					require.NoError(t, err)
					files := []string{"a/new.go", "a/main.go::Caller"}
					for i := 0; i < graph.MaxIncomingSourceCandidateRows+1; i++ {
						files = append(files, fmt.Sprintf("a/f%d.go", i))
					}
					project := func(store graph.Store) map[string][]string {
						rows := make(map[string][]string)
						for file, edges := range store.GetOutEdgesByNodeIDs(files) {
							for _, e := range edges {
								rows[file] = append(rows[file], fmt.Sprintf("%s %s cross=%v", e.Kind, e.To, e.CrossRepo))
							}
							sort.Strings(rows[file])
						}
						return rows
					}
					assert.Equal(t, project(cold), project(g), "all imports and the new call must match cold, including multiplicity")
					freshImports := g.GetOutEdges("a/new.go")
					require.Len(t, freshImports, 1, "one source import must bind exactly once")
					for _, e := range freshImports {
						assert.Equal(t, "b/new/new.go", e.To)
					}
				})
			}
		}
	}
}

// A restart seeds all identities once; cancellation still stops filtered rows.
func TestGoModuleImportSeedCompletionAndCancellation(t *testing.T) {
	for _, mode := range []string{"complete", "cancel"} {
		t.Run(mode, func(t *testing.T) {
			g := &goImportCountingStore{Store: graph.New()}
			count := graph.MaxIncomingSourceCandidateRows + 10
			if mode == "cancel" {
				count = externalCallMutationChunk*2 + 1
			}
			var nodes []*graph.Node
			for i := 0; i < count; i++ {
				nodes = append(nodes, &graph.Node{ID: fmt.Sprintf("a/f%d.go::import::untracked.org/lib", i), Kind: graph.KindImport, FilePath: fmt.Sprintf("a/f%d.go", i), RepoPrefix: "a", Language: "go"})
			}
			nodes = append(nodes, &graph.Node{ID: "b/lib.go", Kind: graph.KindFile, FilePath: "b/lib.go", RepoPrefix: "b", Language: "go"})
			g.AddBatch(nodes, nil)
			cr := NewCrossRepo(g)
			cr.SetGoModules(map[string]string{"b": "example.org/lib"})
			if mode == "cancel" {
				ctx, cancel := context.WithCancel(t.Context())
				defer cancel()
				g.afterBatch = cancel
				require.ErrorIs(t, cr.RetryGoModuleDeps(ctx), context.Canceled)
				assert.LessOrEqual(t, g.importRows, externalCallMutationChunk+1)
			} else {
				_, admission := cr.ResolveMutationFrontiersBounded(nil, nil, []string{"b/lib.go"})
				assert.False(t, admission.Refused)
				assert.Equal(t, count, g.importRows)
				assert.True(t, cr.goModuleRetry.importsSeeded)
				cr.ResolveMutationFrontiersBounded(nil, nil, []string{"b/lib.go"})
				assert.Equal(t, 1, g.importScans)
			}
			require.NotNil(t, cr.goModuleRetry)
			if mode == "cancel" {
				assert.False(t, cr.goModuleRetry.importsSeeded, "a cancelled seed remains eligible for retry")
			}
		})
	}
}

func TestGoModuleImportRestoresEveryAliasStatement(t *testing.T) {
	for _, backend := range []string{"memory", "sqlite"} {
		t.Run(backend, func(t *testing.T) {
			g := goImportReviewStore(t, backend)
			const file = "a/main.go"
			const id = file + "::import::example.org/lib"
			g.AddBatch([]*graph.Node{
				{ID: file, FilePath: file, Kind: graph.KindFile, RepoPrefix: "a", Language: "go"},
				{ID: id, FilePath: file, StartLine: 3, Kind: graph.KindImport, RepoPrefix: "a", Language: "go", Meta: map[string]any{"path": "example.org/lib"}},
				{ID: "b/lib.go", FilePath: "b/lib.go", Kind: graph.KindFile, RepoPrefix: "b", Language: "go"},
			}, []*graph.Edge{
				{From: file, To: id, Kind: graph.EdgeContains, FilePath: file, Line: 2},
				{From: file, To: id, Kind: graph.EdgeContains, FilePath: file, Line: 3},
				{From: "other.go", To: id, Kind: graph.EdgeContains, FilePath: "other.go", Line: 4},
				{From: file, To: "b/lib.go", Kind: graph.EdgeImports, FilePath: file, Line: 3, CrossRepo: true, Meta: map[string]any{"go_import": "example.org/lib"}},
			})
			cr := NewCrossRepo(g)
			cr.SetGoModules(map[string]string{"a": "example.org/app", "b": "example.org/lib"})
			for range 2 {
				require.NoError(t, cr.restoreGoModuleImportBatch(t.Context(), "b", []string{id}, &CrossRepoStats{ByRepo: map[string]int{}}))
			}
			var lines []int
			for _, edge := range outEdgesOfKind(g, file, graph.EdgeImports) {
				require.Equal(t, "b/lib.go", edge.To)
				lines = append(lines, edge.Line)
			}
			sort.Ints(lines)
			require.Equal(t, []int{2, 3}, lines, "one import per retained statement, with no duplicates")
		})
	}
}
