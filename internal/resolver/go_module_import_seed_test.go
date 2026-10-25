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
				require.False(t, admission.Refused)
			} else {
				require.NoError(t, cr.recheckGoModuleBindings(t.Context(), nil, &CrossRepoStats{ByRepo: map[string]int{}}))
			}
			imports := outEdgesOfKind(g, "consumer/main.go", graph.EdgeImports)
			require.Len(t, imports, 1, "the small provider must still restore its import")
			require.Equal(t, "b-small/lib.go", imports[0].To)
		})
	}
}
