package resolver

import (
	"context"
	"fmt"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/graph/store_sqlite"
)

// Given more unrelated import identities than a save's recovery ceiling, the
// first save after restart must still discover and recover a small provider.
func TestGoModuleImportLargeRestartSeed(t *testing.T) {
	for _, backend := range []string{"memory", "sqlite"} {
		for _, mode := range []string{"new_package", "representative"} {
			t.Run(backend+"/"+mode, func(t *testing.T) {
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
				var nodes []*graph.Node
				for i := 0; i < graph.MaxIncomingSourceCandidateRows+1; i++ {
					file := fmt.Sprintf("noise/f%d.go", i)
					nodes = append(nodes, &graph.Node{ID: file + "::import::untracked.org/library", Kind: graph.KindImport, FilePath: file, RepoPrefix: "noise", Language: "go"})
				}
				path, target := "example.org/lib/sub", "dep::example.org/lib"
				first, second := "b/sub/z.go", "b/sub/a.go"
				if mode == "representative" {
					path, target = "example.org/lib", "b/z.go"
					first, second = "b/a.go", "b/0.go"
				}
				nodes = append(nodes,
					&graph.Node{ID: "a/main.go", Kind: graph.KindFile, FilePath: "a/main.go", RepoPrefix: "a", Language: "go"},
					&graph.Node{ID: "a/main.go::import::" + path, Kind: graph.KindImport, FilePath: "a/main.go", RepoPrefix: "a", Language: "go", StartLine: 2, Meta: map[string]any{"path": path}},
					&graph.Node{ID: "b/z.go", Kind: graph.KindFile, FilePath: "b/z.go", RepoPrefix: "b", Language: "go"})
				edge := &graph.Edge{From: "a/main.go", To: target, Kind: graph.EdgeImports, FilePath: "a/main.go", Line: 2}
				if mode == "representative" {
					edge.CrossRepo = true
					edge.Meta = map[string]any{"go_import": path, "go_import_fallback": "dep::example.org/lib"}
				}
				g := &goImportCountingStore{Store: newStore()}
				g.AddBatch(nodes, []*graph.Edge{edge})
				state := &GoModuleDepRetry{} // unchanged warm restart has no process-local registry
				resolver := func(store graph.Store, retry *GoModuleDepRetry) *CrossRepoResolver {
					cr := NewCrossRepo(store)
					cr.SetGoModules(map[string]string{"a": "example.org/app", "b": "example.org/lib"})
					cr.SetGoModuleDepRetry(retry, 1)
					return cr
				}
				for _, file := range []string{first, second} {
					g.AddNode(&graph.Node{ID: file, Kind: graph.KindFile, FilePath: file, RepoPrefix: "b", Language: "go"})
					cr := resolver(g, state)
					_, admission := cr.ResolveMutationFrontiersBounded(nil, nil, []string{file})
					assert.False(t, admission.Refused)
					rows := g.GetOutEdges("a/main.go")
					require.Len(t, rows, 1)
					assert.Equal(t, file, rows[0].To, "save must recover the new package or representative without a full pass")
					assert.True(t, rows[0].CrossRepo)
					assert.True(t, state.importsSeeded, "first save must complete the registry even above the recovery ceiling")
				}
				assert.Equal(t, 1, g.importScans, "later saves must not repeat the global identity scan")
				assert.Equal(t, len(nodes)-2, g.importRows, "visit all import identities once, not only the first budget-sized prefix")
				cold := newStore()
				cold.AddBatch(nodes, []*graph.Edge{edge})
				for _, file := range []string{first, second} {
					cold.AddNode(&graph.Node{ID: file, Kind: graph.KindFile, FilePath: file, RepoPrefix: "b", Language: "go"})
				}
				_, err := resolver(cold, &GoModuleDepRetry{}).ResolveAllContext(context.Background())
				require.NoError(t, err)
				warmRows, coldRows := g.GetOutEdges("a/main.go"), cold.GetOutEdges("a/main.go")
				require.Len(t, coldRows, 1)
				require.Len(t, warmRows, 1)
				assert.Equal(t, coldRows[0].To, warmRows[0].To)
				assert.Equal(t, coldRows[0].CrossRepo, warmRows[0].CrossRepo)
			})
		}
	}
}

func TestGoModuleImportOversizedProviderDoesNotStarveNext(t *testing.T) {
	for _, route := range []string{"frontier", "identity"} {
		t.Run(route, func(t *testing.T) {
			g := graph.New()
			for _, file := range []string{"a-big/lib.go", "b-small/lib.go", "consumer/main.go"} {
				g.AddNode(&graph.Node{ID: file, Name: "lib.go", FilePath: file, Kind: graph.KindFile, RepoPrefix: graph.RepoPrefixOfID(file), Language: "go"})
			}
			const id = "consumer/main.go::import::example.org/small"
			g.AddNode(&graph.Node{ID: id, Name: "small", FilePath: "consumer/main.go", StartLine: 2, Kind: graph.KindImport, RepoPrefix: "consumer", Language: "go", Meta: map[string]any{"path": "example.org/small"}})
			cr := NewCrossRepo(g)
			modules := map[string]string{"a-big": "example.org/big", "b-small": "example.org/small", "consumer": "example.org/app"}
			cr.SetGoModules(modules)
			state := cr.goModuleImportState()
			state.importsSeeded, state.complete = true, true
			state.modules = modules
			state.imports["a-big"] = make(map[string]struct{})
			for i := 0; i <= graph.MaxIncomingSourceCandidateRows; i++ {
				state.imports["a-big"][fmt.Sprintf("missing/%d", i)] = struct{}{}
			}
			state.imports["b-small"] = map[string]struct{}{id: {}}
			if route == "frontier" {
				_, admission := cr.ResolveMutationFrontiersBounded(nil, nil, []string{"a-big/lib.go", "b-small/lib.go"})
				require.True(t, admission.Refused)
				require.Error(t, admission.Refusal)
			} else {
				require.NoError(t, cr.recheckGoModuleBindings(t.Context(), nil, &CrossRepoStats{ByRepo: map[string]int{}}, false))
			}
			imports := outEdgesOfKind(g, "consumer/main.go", graph.EdgeImports)
			require.Len(t, imports, 1, "the small provider must still restore its import")
			require.Equal(t, "b-small/lib.go", imports[0].To)
		})
	}
}

func TestGoModuleNestedProviderFullAndWarmParity(t *testing.T) {
	for _, backend := range []string{"memory", "sqlite"} {
		for _, route := range []string{"full", "frontier", "seed"} {
			t.Run(backend+"/"+route, func(t *testing.T) {
				g := goImportReviewStore(t, backend)
				const file = "a/main.go"
				const spec = "example.org/root/child/sub"
				const id = file + "::import::" + spec
				g.AddBatch([]*graph.Node{
					{ID: file, Name: "main.go", FilePath: file, Kind: graph.KindFile, RepoPrefix: "a", Language: "go"},
					{ID: id, Name: "sub", FilePath: file, StartLine: 2, Kind: graph.KindImport, RepoPrefix: "a", Language: "go", Meta: map[string]any{"path": spec}},
					{ID: "b/sub/lib.go", Name: "lib.go", FilePath: "b/sub/lib.go", Kind: graph.KindFile, RepoPrefix: "b", Language: "go"},
				}, []*graph.Edge{{From: file, To: "dep::example.org/root/child", FilePath: file, Line: 2, Kind: graph.EdgeImports}})
				cr := NewCrossRepo(g)
				cr.SetGoModules(map[string]string{"a": "example.org/root", "b": "example.org/root/child"})
				if route == "seed" {
					require.NoError(t, cr.seedGoModuleImportRegistry(t.Context()))
					require.Contains(t, cr.goModuleRetry.imports["b"], id)
					return
				}
				if route == "full" {
					_, err := cr.ResolveAllContext(t.Context())
					require.NoError(t, err)
				} else {
					cr.ResolveMutationFiles([]string{file}, nil)
				}
				imports := outEdgesOfKind(g, file, graph.EdgeImports)
				require.Len(t, imports, 1)
				require.Equal(t, "b/sub/lib.go", imports[0].To)
				require.True(t, imports[0].CrossRepo)
			})
		}
	}
}

func TestGoModuleImportRecoveryBudgetRefusal(t *testing.T) {
	old := newGoModuleImportRecoveryBudget
	newGoModuleImportRecoveryBudget = func() *graph.IncomingSourceBudget {
		b := &graph.IncomingSourceBudget{}
		require.NoError(t, b.Charge(graph.MaxIncomingSourceCandidateRows-1))
		return b
	}
	t.Cleanup(func() { newGoModuleImportRecoveryBudget = old })
	for _, backend := range []string{"memory", "sqlite"} {
		for _, route := range []string{"frontier", "identity"} {
			t.Run(backend+"/"+route, func(t *testing.T) {
				g := goImportReviewStore(t, backend)
				g.AddNode(&graph.Node{ID: "provider/lib.go", Name: "lib.go", FilePath: "provider/lib.go", Kind: graph.KindFile, RepoPrefix: "provider", Language: "go"})
				ids := map[string]struct{}{}
				for i := 0; i < 2; i++ {
					file := fmt.Sprintf("consumer/f%d.go", i)
					id := file + "::import::example.org/lib"
					ids[id] = struct{}{}
					g.AddBatch([]*graph.Node{{ID: file, FilePath: file, Kind: graph.KindFile, RepoPrefix: "consumer", Language: "go"}, {ID: id, FilePath: file, StartLine: 1, Kind: graph.KindImport, RepoPrefix: "consumer", Language: "go", Meta: map[string]any{"path": "example.org/lib"}}}, []*graph.Edge{{From: file, FilePath: file, To: "dep::example.org/lib", Kind: graph.EdgeImports, Line: 1}})
				}
				cr := NewCrossRepo(g)
				modules := map[string]string{"provider": "example.org/lib", "consumer": "example.org/app"}
				cr.SetGoModules(modules)
				state := cr.goModuleImportState()
				state.importsSeeded = true
				state.imports["provider"] = ids
				state.complete = route == "frontier"
				state.modules = modules
				changed := "provider/lib.go"
				if route == "identity" {
					changed = "consumer/f0.go"
				}
				_, admission := cr.ResolveMutationFrontiersBounded(nil, nil, []string{changed})
				require.True(t, admission.Refused)
				require.Error(t, admission.Refusal)
				require.GreaterOrEqual(t, admission.Dropped, 2)
				require.False(t, state.complete)
				require.False(t, admission.IncomingRefused)
				// The explicit full pass is unbounded and repairs the deferred imports.
				_, err := cr.ResolveAllContext(t.Context())
				require.NoError(t, err)
				require.True(t, state.complete, "a full pass must finish the deferred revision")
				for _, restart := range []bool{false, true} {
					next := cr
					if restart {
						next = NewCrossRepo(g)
						next.SetGoModules(modules)
						next.SetGoModuleDepRetry(state, cr.goModuleRevision)
					}
					_, nextAdmission := next.ResolveMutationFrontiersBounded(nil, nil, []string{"consumer/f0.go"})
					require.False(t, nextAdmission.Refused, "completed revision must not reopen its bounded recovery")
				}
				for i := 0; i < 2; i++ {
					rows := outEdgesOfKind(g, fmt.Sprintf("consumer/f%d.go", i), graph.EdgeImports)
					require.Len(t, rows, 1)
					require.Equal(t, "provider/lib.go", rows[0].To)
				}
				_, err = cr.ResolveAllContext(t.Context())
				require.NoError(t, err)
				require.True(t, state.complete)
			})
		}
	}
}
