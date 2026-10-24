package indexer

import (
	"context"
	"path/filepath"
	"sort"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	"github.com/zzet/gortex/internal/config"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/graph/store_sqlite"
	"github.com/zzet/gortex/internal/resolver"
	"github.com/zzet/gortex/internal/search"
)

// Given a tracked provider and an unchanged Go consumer, every provider
// transition must leave the same imports as an independent cold installation.
func TestGoModuleImportLifecycleColdParity(t *testing.T) {
	for _, backend := range []string{"memory", "sqlite"} {
		for _, reverse := range []bool{false, true} {
			order := "consumer_first"
			if reverse {
				order = "provider_first"
			}
			for _, mode := range []string{"point", "watcher", "full", "reconcile", "untrack", "rename", "denied"} {
				t.Run(backend+"/"+order+"/"+mode, func(t *testing.T) {
					mi, g, repos := goModuleLifecycleFixture(t, backend)
					writeFile(t, filepath.Join(repos[0].Path, "go.mod"), "module example.org/caller\nrequire example.org/library v1.0.0\n")
					writeFile(t, filepath.Join(repos[0].Path, "main.go"), "package caller\nimport library \"example.org/library\"\nimport _ \"example.net/untracked\"\nfunc Caller() { library.Hello() }\n")
					if mode == "denied" {
						repos[1].Workspace = "other"
					}
					entries := []config.RepoEntry{repos[0], repos[1]}
					if reverse {
						entries[0], entries[1] = entries[1], entries[0]
					}
					track := func(idx *MultiIndexer, entries []config.RepoEntry) {
						t.Helper()
						for _, entry := range entries {
							_, err := idx.TrackRepoCtx(t.Context(), entry)
							require.NoError(t, err)
						}
					}
					track(mi, entries)
					type importRow struct {
						To         string
						CrossRepo  bool
						Confidence float64
						Origin     string
					}
					imports := func(store graph.Store) []importRow {
						t.Helper()
						var rows []importRow
						for _, e := range store.GetOutEdges("a/main.go") {
							if e.Kind != graph.EdgeImports {
								continue
							}
							rows = append(rows, importRow{e.To, e.CrossRepo, e.Confidence, e.Origin})
							if e.CrossRepo {
								assert.Equal(t, "example.org/library", e.Meta["go_import"], "retain package identity for eviction and map changes")
							}
						}
						sort.Slice(rows, func(i, j int) bool { return rows[i].To < rows[j].To })
						require.Len(t, rows, 2, "both the tracked and unrelated imports survive")
						return rows
					}
					compareCold := func(active []config.RepoEntry) {
						t.Helper()
						var coldGraph graph.Store = graph.New()
						if backend == "sqlite" {
							db, err := store_sqlite.Open(filepath.Join(t.TempDir(), "cold.sqlite"))
							require.NoError(t, err)
							t.Cleanup(func() { require.NoError(t, db.Close()) })
							coldGraph = db
						}
						cold := NewMultiIndexer(coldGraph, newTestRegistry(), search.NewNull(), newTestConfigManager(t), zap.NewNop())
						t.Cleanup(func() { require.NoError(t, cold.Close(context.Background())) })
						track(cold, active)
						cold.RunGlobalResolve()
						assert.Equal(t, imports(coldGraph), imports(g), "warm imports must match a fresh cold graph")
					}
					initial := imports(g)
					assert.Contains(t, initial, importRow{To: "external::example.net/untracked"})
					if mode == "denied" {
						assert.Contains(t, initial, importRow{To: "dep::example.org/library"})
					} else {
						assert.Contains(t, initial, importRow{To: "b/lib.go", CrossRepo: true})
					}
					compareCold(entries)
					lib := filepath.Join(repos[1].Path, "lib.go")
					writeFile(t, lib, "package library\nfunc Hello() { println(1) }\n")
					var err error
					switch mode {
					case "point", "denied":
						err = mi.GetIndexer("b").IndexFile(lib)
					case "watcher":
						_, err = mi.incrementalPointRepoRaw("b", lib)
					case "full":
						_, err = mi.IndexRepo("b")
					case "reconcile":
						t.Setenv("GORTEX_WARMUP_FULL_RETRACK", "1")
						restored := NewMultiIndexer(g, newTestRegistry(), search.NewNull(), mi.configMgr, zap.NewNop())
						t.Cleanup(func() { require.NoError(t, restored.Close(context.Background())) })
						var result *IndexResult
						result, err = restored.ReconcileRepoCtx(t.Context(), repos[1], mi.FileMtimes("b"))
						require.NoError(t, err)
						require.True(t, result.FullRetrack)
					case "untrack":
						_, _, err = mi.UntrackRepoChecked(t.Context(), "b")
						require.NoError(t, err)
						mi.RunGlobalResolve()
						compareCold([]config.RepoEntry{repos[0]})
						_, err = mi.TrackRepoCtx(t.Context(), repos[1])
					case "rename":
						manifest := filepath.Join(repos[1].Path, "go.mod")
						writeFile(t, manifest, "module example.org/renamed\n")
						err = mi.GetIndexer("b").IndexFile(manifest)
					}
					require.NoError(t, err)
					compareCold(entries)
					mi.RunGlobalResolve()
					compareCold(entries)
				})
			}
		}
	}
}

func TestGoModuleImportParkBeforeEviction(t *testing.T) {
	for _, backend := range []string{"memory", "sqlite"} {
		for _, route := range []string{"repo", "file", "deleted_batch"} {
			t.Run(backend+"/"+route, func(t *testing.T) {
				var g graph.Store = graph.New()
				if backend == "sqlite" {
					db, err := store_sqlite.Open(filepath.Join(t.TempDir(), "park.sqlite"))
					require.NoError(t, err)
					t.Cleanup(func() { require.NoError(t, db.Close()) })
					g = db
				}
				g.AddBatch([]*graph.Node{
					{ID: "a/main.go", Name: "main.go", Kind: graph.KindFile, FilePath: "a/main.go", RepoPrefix: "a", Language: "go"},
					{ID: "b/lib.go", Name: "lib.go", Kind: graph.KindFile, FilePath: "b/lib.go", RepoPrefix: "b", Language: "go"},
				}, []*graph.Edge{{From: "a/main.go", To: "b/lib.go", Kind: graph.EdgeImports, FilePath: "a/main.go", Line: 2, CrossRepo: true, Meta: map[string]any{"go_import": "example.org/lib"}}})
				if route == "repo" {
					restubGoModuleRepoIncoming(g, "b")
				} else if route == "deleted_batch" {
					provider := g.GetNode("b/lib.go")
					carried := restubIncomingRefsFromView(g,
						[]*incrementalBatchStage{{graphPath: "b/lib.go", priorNodes: []*graph.Node{provider}}},
						incrementalPriorView{inByNode: g.GetInEdgesByNodeIDs([]string{provider.ID})})
					assert.Empty(t, carried, "a deleted file cannot carry a resolved target")
				} else {
					idx := newTestIndexer(g)
					idx.SetRepoPrefix("b")
					t.Cleanup(idx.Close)
					idx.restubIncomingRefs("b/lib.go")
				}
				g.EvictFile("b/lib.go")
				rows := g.GetOutEdges("a/main.go")
				require.Len(t, rows, 1, "the consumer-owned row must survive provider eviction")
				assert.Equal(t, "unresolved::import::example.org/lib", rows[0].To)
				assert.Equal(t, "example.org/lib", rows[0].Meta["go_import"])
				g.AddNode(&graph.Node{ID: "b/lib.go", Name: "lib.go", Kind: graph.KindFile, FilePath: "b/lib.go", RepoPrefix: "b", Language: "go"})
				cr := resolver.NewCrossRepo(g)
				cr.SetGoModules(map[string]string{"b": "example.org/lib"})
				_, admission := cr.ResolveMutationFrontiersBounded(nil, nil, []string{"b/lib.go"})
				assert.False(t, admission.Refused)
				rows = g.GetOutEdges("a/main.go")
				require.Len(t, rows, 1)
				assert.Equal(t, "b/lib.go", rows[0].To, "the exact incoming frontier must relink without a retained import statement")
				assert.True(t, rows[0].CrossRepo)
			})
		}
	}
}
