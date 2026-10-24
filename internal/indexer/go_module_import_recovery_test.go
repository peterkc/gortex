package indexer

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	"github.com/zzet/gortex/internal/config"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/graph/store_sqlite"
	"github.com/zzet/gortex/internal/search"
)

func TestGoModuleImportReplaceRecoveryColdParity(t *testing.T) {
	for _, backend := range []string{"memory", "sqlite"} {
		for _, reverse := range []bool{false, true} {
			for _, scen := range []string{"inreplace", "samedir"} {
				t.Run(fmt.Sprintf("%s/%s/reverse=%v", backend, scen, reverse), func(t *testing.T) {
					mi, g, repos := goModuleLifecycleFixture(t, backend)
					a, b := repos[0].Path, repos[1].Path
					gomod := "module example.org/caller\nrequire example.org/library v1.0.0\n"
					if scen == "inreplace" {
						gomod += "replace example.org/library => ./third_party/library\n"
						require.NoError(t, os.MkdirAll(filepath.Join(a, "third_party", "library"), 0o755))
						writeFile(t, filepath.Join(a, "third_party", "library", "lib.go"), "package library\nfunc Hello() {}\n")
					} else {
						require.NoError(t, os.MkdirAll(filepath.Join(a, "pkg", "library"), 0o755))
						writeFile(t, filepath.Join(a, "pkg", "library", "l.go"), "package library\nfunc Hello() {}\n")
					}
					writeFile(t, filepath.Join(a, "go.mod"), gomod)
					writeFile(t, filepath.Join(a, "main.go"), "package caller\nimport library \"example.org/library\"\nfunc Caller() { library.Hello() }\n")
					entries := []config.RepoEntry{repos[0], repos[1]}
					if reverse {
						entries[0], entries[1] = entries[1], entries[0]
					}
					for _, e := range entries {
						_, err := mi.TrackRepoCtx(t.Context(), e)
						require.NoError(t, err)
					}
					compareCold := func() {
						t.Helper()
						var cg graph.Store = graph.New()
						if backend == "sqlite" {
							db, err := store_sqlite.Open(filepath.Join(t.TempDir(), "cold.sqlite"))
							require.NoError(t, err)
							defer func() { require.NoError(t, db.Close()) }()
							cg = db
						}
						cold := NewMultiIndexer(cg, newTestRegistry(), search.NewNull(), newTestConfigManager(t), zap.NewNop())
						defer func() { require.NoError(t, cold.Close(context.Background())) }()
						for _, e := range entries {
							_, err := cold.TrackRepoCtx(t.Context(), e)
							require.NoError(t, err)
						}
						cold.RunGlobalResolve()
						require.Len(t, goImportRecoveryRows(cg, "a/main.go"), 1)
						assert.Equal(t, goImportRecoveryRows(cg, "a/main.go"), goImportRecoveryRows(g, "a/main.go"))
					}
					compareCold()
					lib := filepath.Join(b, "lib.go")
					for i := 0; i < 3; i++ {
						writeFile(t, lib, "package library\nfunc Hello() { println("+string(rune('1'+i))+") }\n")
						_, err := mi.IncrementalReindexRepo("b", []string{lib})
						require.NoError(t, err)
						compareCold()
					}
					for i := 0; i < 3; i++ {
						mi.RunGlobalResolve()
						compareCold()
					}
					assert.Len(t, goImportRecoveryRows(g, "a/main.go"), 1, "one import statement must keep one IMPORT edge")
				})
			}
		}
	}
}

func goImportRecoveryRows(g graph.Store, file string) []string {
	var rows []string
	for _, e := range g.GetOutEdges(file) {
		if e.Kind != graph.EdgeImports {
			continue
		}
		rows = append(rows, fmt.Sprintf("L%d %s cross=%v conf=%v origin=%s gi=%v fb=%v", e.Line, e.To, e.CrossRepo, e.Confidence, e.Origin, e.Meta["go_import"], e.Meta["go_import_fallback"]))
	}
	sort.Strings(rows)
	return rows
}

func TestGoModuleImportPopularProviderSave(t *testing.T) {
	for _, backend := range []string{"memory", "sqlite"} {
		for _, reverse := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/reverse=%v", backend, reverse), func(t *testing.T) {
				mi, g, repos := goModuleLifecycleFixture(t, backend)
				a, b := repos[0].Path, repos[1].Path
				writeFile(t, filepath.Join(a, "go.mod"), "module example.org/caller\nrequire example.org/library v1.0.0\n")
				writeFile(t, filepath.Join(a, "main.go"), "package caller\nimport library \"example.org/library\"\nfunc Caller() { library.New() }\n")
				writeFile(t, filepath.Join(a, "newpkg.go"), "package caller\nimport _ \"example.org/library/new\"\n")
				entries := []config.RepoEntry{repos[0], repos[1]}
				if reverse {
					entries[0], entries[1] = entries[1], entries[0]
				}
				track := func(idx *MultiIndexer) {
					t.Helper()
					for _, entry := range entries {
						_, err := idx.TrackRepoCtx(t.Context(), entry)
						require.NoError(t, err)
					}
				}
				track(mi)
				populate := func(store graph.Store) {
					var nodes []*graph.Node
					var edges []*graph.Edge
					for i := 0; i < graph.MaxIncomingSourceCandidateRows+1; i++ {
						f := fmt.Sprintf("a/filler%d.go", i)
						nodes = append(nodes, &graph.Node{ID: f, Kind: graph.KindFile, FilePath: f, RepoPrefix: "a", Language: "go"}, &graph.Node{ID: f + "::import::example.org/library", Kind: graph.KindImport, FilePath: f, RepoPrefix: "a", Language: "go", StartLine: 2, Meta: map[string]any{"path": "example.org/library"}})
						edges = append(edges, &graph.Edge{From: f, To: "b/lib.go", Kind: graph.EdgeImports, FilePath: f, Line: 2, CrossRepo: true, Meta: map[string]any{"go_import": "example.org/library", "go_import_fallback": "dep::example.org/library"}})
					}
					store.AddBatch(nodes, edges)
				}
				populate(g)
				// Reconcile the unchanged persisted graph into a new process, then seed on a full pass.
				restored := NewMultiIndexer(g, newTestRegistry(), search.NewNull(), mi.configMgr, zap.NewNop())
				defer func() { require.NoError(t, restored.Close(context.Background())) }()
				for _, entry := range entries {
					_, err := restored.ReconcileRepoCtx(t.Context(), entry, mi.FileMtimes(entry.Name))
					require.NoError(t, err)
				}
				restored.RunGlobalResolve()
				f := filepath.Join(b, "new.go")
				writeFile(t, f, "package library\nfunc New() {}\n")
				require.NoError(t, restored.GetIndexer("b").IndexFile(f))
				assert.Equal(t, "b/new.go::New", goModuleLifecycleCall(t, g).To, "the actual provider save must bind the unchanged caller")
				require.NoError(t, os.MkdirAll(filepath.Join(b, "new"), 0o755))
				pkg := filepath.Join(b, "new", "p.go")
				writeFile(t, pkg, "package newpkg\n")
				require.NoError(t, restored.GetIndexer("b").IndexFile(pkg))
				restored.RunGlobalResolve()
				var coldGraph graph.Store = graph.New()
				if backend == "sqlite" {
					db, err := store_sqlite.Open(filepath.Join(t.TempDir(), "cold.sqlite"))
					require.NoError(t, err)
					defer func() { require.NoError(t, db.Close()) }()
					coldGraph = db
				}
				cold := NewMultiIndexer(coldGraph, newTestRegistry(), search.NewNull(), newTestConfigManager(t), zap.NewNop())
				defer func() { require.NoError(t, cold.Close(context.Background())) }()
				track(cold)
				populate(coldGraph)
				cold.RunGlobalResolve()
				for _, file := range []string{"a/main.go", "a/newpkg.go"} {
					assert.Equal(t, goImportRecoveryRows(coldGraph, file), goImportRecoveryRows(g, file))
				}
				assert.Equal(t, goModuleLifecycleCall(t, coldGraph).To, goModuleLifecycleCall(t, g).To)
			})
		}
	}
}

func TestGoModuleImportReplaceAddedLater(t *testing.T) {
	for _, backend := range []string{"memory", "sqlite"} {
		t.Run(backend, func(t *testing.T) {
			mi, g, repos := goModuleLifecycleFixture(t, backend)
			a := repos[0].Path
			manifest := filepath.Join(a, "go.mod")
			writeFile(t, manifest, "module example.org/caller\nrequire example.org/library v1.0.0\n")
			entries := []config.RepoEntry{repos[0], repos[1]}
			for _, e := range entries {
				_, err := mi.TrackRepoCtx(t.Context(), e)
				require.NoError(t, err)
			}
			t.Logf("linked: %v call=%s", goImportRecoveryRows(g, "a/main.go"), goModuleLifecycleCall(t, g).To)
			require.NoError(t, os.MkdirAll(filepath.Join(a, "third_party", "library"), 0o755))
			vendored := filepath.Join(a, "third_party", "library", "lib.go")
			writeFile(t, vendored, "package library\nfunc Hello() {}\n")
			writeFile(t, manifest, "module example.org/caller\nrequire example.org/library v1.0.0\nreplace example.org/library => ./third_party/library\n")
			_, err := mi.IncrementalReindexRepo("a", []string{manifest, vendored})
			require.NoError(t, err)
			t.Logf("after replace save: %v call=%s", goImportRecoveryRows(g, "a/main.go"), goModuleLifecycleCall(t, g).To)
			mi.RunGlobalResolve()
			t.Logf("after global: %v call=%s", goImportRecoveryRows(g, "a/main.go"), goModuleLifecycleCall(t, g).To)
			var coldGraph graph.Store = graph.New()
			if backend == "sqlite" {
				db, err := store_sqlite.Open(filepath.Join(t.TempDir(), "cold.sqlite"))
				require.NoError(t, err)
				t.Cleanup(func() { require.NoError(t, db.Close()) })
				coldGraph = db
			}
			cold := NewMultiIndexer(coldGraph, newTestRegistry(), search.NewNull(), newTestConfigManager(t), zap.NewNop())
			t.Cleanup(func() { require.NoError(t, cold.Close(context.Background())) })
			for _, e := range entries {
				_, err := cold.TrackRepoCtx(t.Context(), e)
				require.NoError(t, err)
			}
			cold.RunGlobalResolve()
			require.Len(t, goImportRecoveryRows(coldGraph, "a/main.go"), 1)
			t.Logf("cold: %v call=%s", goImportRecoveryRows(coldGraph, "a/main.go"), goModuleLifecycleCall(t, coldGraph).To)
			assert.Equal(t, goImportRecoveryRows(coldGraph, "a/main.go"), goImportRecoveryRows(g, "a/main.go"))
		})
	}
}
