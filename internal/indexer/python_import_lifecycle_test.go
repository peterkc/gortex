package indexer

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
	"go.uber.org/zap/zaptest/observer"

	"github.com/zzet/gortex/internal/config"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/graph/store_sqlite"
	"github.com/zzet/gortex/internal/parser/languages"
	"github.com/zzet/gortex/internal/search"
)

func TestTrackedPythonImportLifecycle(t *testing.T) {
	for _, backend := range []string{"memory", "sqlite"} {
		for _, providerFirst := range []bool{false, true} {
			for _, operation := range []string{"consumer_save", "provider_save", "provider_save_util", "provider_save_lib_contract", "provider_delete_util_batched", "forced_evict_util", "delete_restore_util", "consumer_save_then_restore", "provider_gains_new_module", "provider_gains_empty_module", "consumer_full", "provider_full", "warm_full_retrack", "untrack_retrack", "no_change", "track", "deferred_track"} {
				if operation == "deferred_track" && providerFirst {
					continue // This case starts with an already tracked consumer.
				}
				order := "consumer_first"
				if providerFirst {
					order = "provider_first"
				}
				t.Run(backend+"/"+order+"/"+operation, func(t *testing.T) {
					provider, consumer := t.TempDir(), t.TempDir()
					writeWorkspaceFile(t, provider, "pyproject.toml", "[project]\nname = 'pillow'\n")
					writeWorkspaceFile(t, provider, "src/PIL/__init__.py", "")
					writeWorkspaceFile(t, provider, "src/PIL/image.py", pythonLifecycleProvider)
					writeWorkspaceFile(t, provider, "src/PIL/util.py", "def helper(): pass\n")
					writeWorkspaceFile(t, consumer, "pyproject.toml", "[project]\ndependencies = ['pillow']\n")
					consumerSource := pythonLifecycleConsumer
					switch operation {
					case "provider_gains_new_module":
						consumerSource += "\nfrom PIL.extra import later\ndef after(): return later()\n"
					case "provider_gains_empty_module":
						consumerSource += "\nimport PIL.extra\n"
					}
					writeWorkspaceFile(t, consumer, "app.py", consumerSource)
					entries := []config.RepoEntry{{Path: consumer, Name: "consumer", Workspace: "shared"}, {Path: provider, Name: "provider", Workspace: "shared"}}
					cfgPath := filepath.Join(t.TempDir(), "config.yaml")
					gc := &config.GlobalConfig{Repos: entries}
					if providerFirst {
						gc.Repos = []config.RepoEntry{entries[1], entries[0]}
					}
					gc.SetConfigPath(cfgPath)
					require.NoError(t, gc.Save())
					cm, err := config.NewConfigManager(cfgPath)
					require.NoError(t, err)
					var g graph.Store = graph.New()
					if backend == "sqlite" {
						disk, err := store_sqlite.Open(filepath.Join(t.TempDir(), "graph.sqlite"))
						require.NoError(t, err)
						t.Cleanup(func() { require.NoError(t, disk.Close()) })
						g = disk
					}
					reg := newTestRegistry()
					reg.Register(languages.NewPythonExtractor())
					core, logs := observer.New(zap.InfoLevel)
					logger := zap.New(core)
					mi := NewMultiIndexer(g, reg, search.NewNull(), cm, logger)
					t.Cleanup(func() { require.NoError(t, mi.Close(context.Background())) })
					switch operation {
					case "deferred_track":
						_, err = mi.TrackRepoCtx(context.Background(), entries[0])
						require.NoError(t, err)
						mi.BeginParallelBatch()
						_, err = mi.TrackRepoCtx(context.Background(), entries[1])
						require.NoError(t, err)
						for _, edge := range g.GetOutEdges("consumer/app.py") {
							if edge.Kind == graph.EdgeImports {
								require.False(t, edge.CrossRepo, "deferred track must not resolve shared imports early")
							}
						}
						require.NoError(t, mi.RunPreEnrichResolveFiles(context.Background(), []string{"consumer/app.py", "provider/src/PIL/image.py", "provider/src/PIL/util.py"}, nil))
						mi.EndBatch()
					case "track":
						for _, entry := range gc.Repos {
							_, err = mi.TrackRepoCtx(context.Background(), entry)
							require.NoError(t, err)
						}
					default:
						_, err = mi.IndexAll()
						require.NoError(t, err)
					}
					assertImport := func() {
						t.Helper()
						var imports []*graph.Edge
						for _, edge := range g.GetOutEdges("consumer/app.py") {
							if edge.Kind == graph.EdgeImports && edge.Line <= 4 {
								imports = append(imports, edge)
							}
						}
						require.Len(t, imports, 4)
						for _, edge := range imports {
							require.True(t, edge.CrossRepo, "%#v", edge)
						}
						calls := make(map[string]int)
						for _, edge := range g.GetOutEdges("consumer/app.py::run") {
							if edge.Kind == graph.EdgeCalls {
								require.True(t, edge.CrossRepo, "%#v", edge)
								require.NotEmpty(t, edge.Meta["python_call_stub"], "%#v", edge)
								require.NotContains(t, edge.Meta, "external_call")
								calls[edge.To]++
							}
						}
						require.Equal(t, map[string]int{
							"provider/src/PIL/image.py::Image":      1,
							"provider/src/PIL/image.py::Image.show": 1,
							"provider/src/PIL/image.py::f":          2,
							"provider/src/PIL/util.py::helper":      1,
						}, calls)
					}
					assertImport()
					logs.TakeAll()
					var scans *pythonLookupScanStore
					if operation == "provider_save" {
						scans = &pythonLookupScanStore{Store: g}
						mi.graph = scans
					}
					switch operation {
					case "consumer_save":
						writeWorkspaceFile(t, consumer, "app.py", pythonLifecycleConsumer+"def edited(): pass\n")
						_, err = mi.IncrementalReindexRepo("consumer", []string{filepath.Join(consumer, "app.py")})
					case "provider_save":
						writeWorkspaceFile(t, provider, "src/PIL/image.py", pythonLifecycleProvider+"def added(): pass\n")
						_, err = mi.IncrementalReindexRepo("provider", []string{filepath.Join(provider, "src/PIL/image.py")})
					case "provider_save_util":
						writeWorkspaceFile(t, provider, "src/PIL/util.py", "def helper(): pass\ndef added(): pass\n")
						_, err = mi.IncrementalReindexRepo("provider", []string{filepath.Join(provider, "src/PIL/util.py")})
					case "provider_save_lib_contract":
						writeWorkspaceFile(t, provider, "src/PIL/image.py", strings.Replace(pythonLifecycleProvider, "def f(): pass", "def f(x): pass", 1))
						_, err = mi.IncrementalReindexRepo("provider", []string{filepath.Join(provider, "src/PIL/image.py")})
					case "provider_delete_util_batched":
						require.NoError(t, os.Remove(filepath.Join(provider, "src/PIL/util.py")))
						writeWorkspaceFile(t, provider, "src/PIL/image.py", pythonLifecycleProvider+"def added(): pass\n")
						_, err = mi.IncrementalReindexRepo("provider", []string{filepath.Join(provider, "src/PIL/util.py"), filepath.Join(provider, "src/PIL/image.py")})
					case "provider_gains_empty_module":
						writeWorkspaceFile(t, provider, "src/PIL/extra.py", "")
						_, err = mi.IncrementalReindexRepo("provider", []string{filepath.Join(provider, "src/PIL/extra.py")})
					case "provider_gains_new_module":
						writeWorkspaceFile(t, provider, "src/PIL/extra.py", "def later(): pass\n")
						_, err = mi.IncrementalReindexRepo("provider", []string{filepath.Join(provider, "src/PIL/extra.py")})
					case "delete_restore_util", "consumer_save_then_restore":
						if operation == "consumer_save_then_restore" {
							writeWorkspaceFile(t, consumer, "app.py", pythonLifecycleConsumer+"def edited(): pass\n")
							_, err = mi.IncrementalReindexRepo("consumer", []string{filepath.Join(consumer, "app.py")})
							require.NoError(t, err)
							assertImport()
						}
						require.NoError(t, os.Remove(filepath.Join(provider, "src/PIL/util.py")))
						_, err = mi.IncrementalReindexRepo("provider", []string{filepath.Join(provider, "src/PIL/util.py")})
						require.NoError(t, err)
						var parked []*graph.Edge
						for _, edge := range g.GetOutEdges("consumer/app.py::run") {
							if edge.Kind == graph.EdgeCalls && edge.Line == 10 {
								parked = append(parked, edge)
							}
						}
						require.Len(t, parked, 1)
						require.Equal(t, "unresolved::extern::PIL.util.helper::helper", parked[0].To)
						require.False(t, parked[0].CrossRepo)
						writeWorkspaceFile(t, provider, "src/PIL/util.py", "def helper(): pass\n")
						_, err = mi.IncrementalReindexRepo("provider", []string{filepath.Join(provider, "src/PIL/util.py")})
					case "forced_evict_util":
						require.NoError(t, os.Remove(filepath.Join(provider, "src/PIL/util.py")))
						mi.GetIndexer("provider").EvictFile(filepath.Join(provider, "src/PIL/util.py"))
					case "consumer_full":
						_, err = mi.IndexRepo("consumer")
					case "provider_full":
						var result *IndexResult
						result, err = mi.IndexRepo("provider")
						require.NoError(t, err)
						data, marshalErr := json.Marshal(result)
						require.NoError(t, marshalErr)
						var output struct {
							Files []string `json:"cross_repo_resolve_files"`
						}
						require.NoError(t, json.Unmarshal(data, &output))
						require.Contains(t, output.Files, "consumer/app.py")
					case "warm_full_retrack":
						prior := map[string]map[string]int64{"consumer": mi.FileMtimes("consumer"), "provider": mi.FileMtimes("provider")}
						restarted := NewMultiIndexer(g, reg, search.NewNull(), cm, logger)
						t.Cleanup(func() { require.NoError(t, restarted.Close(context.Background())) })
						restarted.BeginBatch()
						var output struct {
							Files []string `json:"cross_repo_resolve_files"`
						}
						for _, entry := range gc.Repos {
							force := "0"
							if entry.Name == "provider" {
								force = "1"
							}
							t.Setenv("GORTEX_WARMUP_FULL_RETRACK", force)
							result, restoreErr := restarted.ReconcileRepoCtx(context.Background(), entry, prior[entry.Name])
							require.NoError(t, restoreErr)
							if entry.Name == "provider" {
								require.True(t, result.FullRetrack)
							}
							data, marshalErr := json.Marshal(result)
							require.NoError(t, marshalErr)
							var current struct {
								Files []string `json:"cross_repo_resolve_files"`
							}
							require.NoError(t, json.Unmarshal(data, &current))
							output.Files = appendUniqueSorted(output.Files, current.Files...)
							if entry.Name == "provider" {
								require.NotContains(t, result.DerivedInvalidation.Files, "consumer/app.py")
							}
						}
						if backend == "sqlite" {
							require.Contains(t, output.Files, "consumer/app.py")
						} else {
							require.Empty(t, output.Files, "memory replacement preserved the existing import")
						}
						err = restarted.RunPreEnrichResolveFiles(context.Background(), output.Files, nil)
						restarted.EndBatch()
					case "no_change":
						result, noChangeErr := mi.IncrementalReindexRepo("provider", nil)
						require.NoError(t, noChangeErr)
						require.Zero(t, result.StaleFileCount)
					case "untrack_retrack":
						_, _, err = mi.UntrackRepoChecked(context.Background(), "provider")
						require.NoError(t, err)
						_, err = mi.TrackRepoCtx(context.Background(), entries[1])
					}
					require.NoError(t, err)
					if scans != nil {
						// The existing cross-repo pass scans imports once for reachability.
						require.Equal(t, 1, scans.importScans, "ordinary saves must not add a Python consumer-frontier scan")
					}
					missingUtil := operation == "provider_delete_util_batched" || operation == "forced_evict_util"
					if missingUtil {
						var missingImport, missingCall []*graph.Edge
						for _, edge := range g.GetOutEdges("consumer/app.py") {
							if edge.Kind == graph.EdgeImports && edge.Line == 4 {
								missingImport = append(missingImport, edge)
							}
						}
						for _, edge := range g.GetOutEdges("consumer/app.py::run") {
							if edge.Kind == graph.EdgeCalls && edge.Line == 10 {
								missingCall = append(missingCall, edge)
							}
						}
						require.Len(t, missingImport, 1)
						require.Equal(t, "module::pypi:PIL", missingImport[0].To)
						require.Equal(t, "PIL.util", missingImport[0].Meta["python_import"])
						require.False(t, missingImport[0].CrossRepo)
						require.Len(t, missingCall, 1)
						require.Equal(t, "unresolved::extern::PIL.util.helper::helper", missingCall[0].To)
						require.Equal(t, missingCall[0].To, missingCall[0].Meta["python_call_stub"])
						require.False(t, missingCall[0].CrossRepo)
					}
					var cold graph.Store = graph.New()
					if backend == "sqlite" {
						db, openErr := store_sqlite.Open(filepath.Join(t.TempDir(), "cold.sqlite"))
						require.NoError(t, openErr)
						t.Cleanup(func() { require.NoError(t, db.Close()) })
						cold = db
					}
					coldIndexer := NewMultiIndexer(cold, reg, search.NewNull(), cm, zap.NewNop())
					t.Cleanup(func() { require.NoError(t, coldIndexer.Close(context.Background())) })
					_, err = coldIndexer.IndexAll()
					require.NoError(t, err)
					require.Equal(t, pythonLifecycleBindings(cold, missingUtil), pythonLifecycleBindings(g, missingUtil), "after %s", operation)
					require.Empty(t, logs.FilterMessage("cross-repo resolve: pass start").All())
					require.Empty(t, logs.FilterMessage("DEFERRED-TIMING master.ResolveAll").All())
				})
			}
		}
	}
}

const pythonLifecycleProvider = "class Image:\n    def show(self): pass\ndef f(): pass\n"
const pythonLifecycleConsumer = "from PIL.image import Image\nfrom PIL.image import f\nimport PIL.image as image\nfrom PIL.util import helper\ndef run():\n    x = Image()\n    x.show()\n    f()\n    image.f()\n    helper()\n"

func pythonLifecycleBindings(g graph.Store, missingUtil bool) []string {
	var bindings []string
	for _, node := range g.GetFileNodes("consumer/app.py") {
		for _, edge := range g.GetOutEdges(node.ID) {
			if edge.Kind == graph.EdgeImports || edge.Kind == graph.EdgeCalls {
				if missingUtil && (edge.Kind == graph.EdgeImports && edge.Line == 4 || edge.Kind == graph.EdgeCalls && edge.Line == 10) {
					continue // These exact parked sites are asserted in the deletion case.
				}
				var stub any
				if edge.Kind == graph.EdgeCalls {
					stub = edge.Meta["python_call_stub"]
				}
				bindings = append(bindings, fmt.Sprintf("%s -%s-> %s cross=%v line=%d stub=%v", edge.From, edge.Kind, edge.To, edge.CrossRepo, edge.Line, stub))
			}
		}
	}
	slices.Sort(bindings)
	return bindings
}

func TestPythonAddedFileWithoutConsumersDoesNotRunGlobalResolve(t *testing.T) {
	core, logs := observer.New(zap.InfoLevel)
	g := graph.New()
	g.AddNode(&graph.Node{ID: "consumer/app.py::run", Kind: graph.KindFunction, Name: "run", FilePath: "consumer/app.py", RepoPrefix: "consumer", Language: "python"})
	g.AddNode(&graph.Node{ID: "provider/new.py::f", Kind: graph.KindFunction, Name: "f", FilePath: "provider/new.py", RepoPrefix: "provider", Language: "python"})
	g.AddEdge(&graph.Edge{From: "consumer/app.py::run", To: "unresolved::f", Kind: graph.EdgeCalls, FilePath: "consumer/app.py"})
	mi := &MultiIndexer{graph: g, logger: zap.New(core)}
	mi.resolveIncrementalRepoMutation("provider", &IndexResult{}, &graph.MutationReceipt{Complete: true},
		&reparsePendingEnrichmentBatch{pythonAddedFiles: []string{"provider/empty.py"}})
	require.Empty(t, logs.FilterMessage("cross-repo resolve: pass start").All())
}
