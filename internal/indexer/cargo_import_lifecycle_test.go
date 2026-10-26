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

func TestTrackedCargoImportLifecycle(t *testing.T) {
	for _, backend := range []string{"memory", "sqlite"} {
		for _, providerFirst := range []bool{false, true} {
			for _, operation := range []string{"consumer_save", "provider_save", "provider_save_util", "provider_save_lib_contract", "provider_delete_util_batched", "forced_evict_util", "delete_restore_util", "consumer_save_then_restore", "provider_gains_new_module", "provider_gains_empty_module", "no_new_file_noop", "consumer_full", "provider_full", "warm_full_retrack", "untrack_retrack", "no_change", "track", "deferred_track"} {
				if operation == "deferred_track" && providerFirst {
					continue // This case starts with an already tracked consumer.
				}
				order := "consumer_first"
				if providerFirst {
					order = "provider_first"
				}
				t.Run(backend+"/"+order+"/"+operation, func(t *testing.T) {
					provider, consumer := t.TempDir(), t.TempDir()
					writeWorkspaceFile(t, provider, "Cargo.toml", "[package]\nname = 'other-crate'\nversion = '0.1.0'\n")
					providerSource := cargoLifecycleProvider
					if operation == "provider_gains_new_module" || operation == "provider_gains_empty_module" {
						providerSource += "pub mod newmod;\n"
					}
					writeWorkspaceFile(t, provider, "src/lib.rs", providerSource)
					writeWorkspaceFile(t, provider, "src/util.rs", "pub fn helper() {}\n")
					if operation == "no_new_file_noop" {
						writeWorkspaceFile(t, provider, "src/spare.rs", "pub fn unused() {}\n")
					}
					writeWorkspaceFile(t, consumer, "Cargo.toml", "[package]\nname = 'app'\nversion = '0.1.0'\n[dependencies]\nother-crate = '1'\n")
					consumerSource := cargoLifecycleConsumer
					switch operation {
					case "provider_gains_new_module":
						consumerSource += "use other_crate::newmod::later;\npub fn after() { later(); }\n"
					case "provider_gains_empty_module":
						consumerSource += "use other_crate::newmod::*;\n"
					}
					writeWorkspaceFile(t, consumer, "src/lib.rs", consumerSource)
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
					reg.Register(languages.NewRustExtractor())
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
						for _, edge := range g.GetOutEdges("consumer/src/lib.rs") {
							if edge.Kind == graph.EdgeImports {
								require.False(t, edge.CrossRepo, "deferred track must not resolve shared imports early")
							}
						}
						require.NoError(t, mi.RunPreEnrichResolveFiles(context.Background(), []string{"consumer/src/lib.rs", "provider/src/lib.rs", "provider/src/util.rs"}, nil))
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
						for _, edge := range g.GetOutEdges("consumer/src/lib.rs") {
							if edge.Kind == graph.EdgeImports && edge.Line <= 3 {
								imports = append(imports, edge)
							}
						}
						require.Len(t, imports, 3)
						for _, edge := range imports {
							require.True(t, edge.CrossRepo)
						}
						calls := make(map[string]string)
						for _, edge := range g.GetOutEdges("consumer/src/lib.rs::run") {
							if edge.Kind == graph.EdgeCalls {
								require.True(t, edge.CrossRepo)
								stub, _ := edge.Meta["cargo_call_stub"].(string)
								calls[edge.To] = stub
							}
						}
						require.Equal(t, map[string]string{
							"provider/src/lib.rs::Foo.new": "unresolved::new",
							"provider/src/lib.rs::Foo.go":  "unresolved::*.go",
							"provider/src/lib.rs::action":  "unresolved::action",
							"provider/src/util.rs::helper": "unresolved::helper",
						}, calls)
					}
					assertImport()
					logs.TakeAll()
					switch operation {
					case "consumer_save":
						writeWorkspaceFile(t, consumer, "src/lib.rs", cargoLifecycleConsumer+"pub fn edited() {}\n")
						_, err = mi.IncrementalReindexRepo("consumer", []string{filepath.Join(consumer, "src/lib.rs")})
					case "provider_save":
						writeWorkspaceFile(t, provider, "src/lib.rs", cargoLifecycleProvider+"pub fn added() {}\n")
						_, err = mi.IncrementalReindexRepo("provider", []string{filepath.Join(provider, "src/lib.rs")})
					case "provider_save_util":
						writeWorkspaceFile(t, provider, "src/util.rs", "pub fn helper() {}\npub fn added() {}\n")
						_, err = mi.IncrementalReindexRepo("provider", []string{filepath.Join(provider, "src/util.rs")})
					case "provider_save_lib_contract":
						writeWorkspaceFile(t, provider, "src/lib.rs", strings.Replace(cargoLifecycleProvider, "pub fn action() {}", "pub fn action(x: u8) {}", 1))
						_, err = mi.IncrementalReindexRepo("provider", []string{filepath.Join(provider, "src/lib.rs")})
					case "provider_delete_util_batched":
						require.NoError(t, os.Remove(filepath.Join(provider, "src/util.rs")))
						writeWorkspaceFile(t, provider, "src/lib.rs", strings.Replace(cargoLifecycleProvider, "pub mod util;\n", "", 1))
						_, err = mi.IncrementalReindexRepo("provider", []string{filepath.Join(provider, "src/util.rs"), filepath.Join(provider, "src/lib.rs")})
					case "no_new_file_noop":
						observed := &cargoImportReindexObserver{Store: mi.graph}
						mi.graph = observed
						writeWorkspaceFile(t, provider, "src/spare.rs", "pub fn unused() {}\npub fn added() {}\n")
						_, err = mi.IncrementalReindexRepo("provider", []string{filepath.Join(provider, "src/spare.rs")})
						require.NoError(t, err)
						require.Zero(t, observed.importReindexes, "an ordinary provider edit must not reindex working imports")
						require.Zero(t, observed.importReads, "bound imports must not be admitted for an ordinary edit")
					case "provider_gains_empty_module":
						writeWorkspaceFile(t, provider, "src/newmod.rs", "")
						_, err = mi.IncrementalReindexRepo("provider", []string{filepath.Join(provider, "src/newmod.rs")})
					case "provider_gains_new_module":
						writeWorkspaceFile(t, provider, "src/newmod.rs", "pub fn later() {}\n")
						_, err = mi.IncrementalReindexRepo("provider", []string{filepath.Join(provider, "src/newmod.rs")})
					case "delete_restore_util", "consumer_save_then_restore":
						if operation == "consumer_save_then_restore" {
							writeWorkspaceFile(t, consumer, "src/lib.rs", cargoLifecycleConsumer+"pub fn edited() {}\n")
							_, err = mi.IncrementalReindexRepo("consumer", []string{filepath.Join(consumer, "src/lib.rs")})
							require.NoError(t, err)
							assertImport()
						}
						require.NoError(t, os.Remove(filepath.Join(provider, "src/util.rs")))
						_, err = mi.IncrementalReindexRepo("provider", []string{filepath.Join(provider, "src/util.rs")})
						require.NoError(t, err)
						var parked []*graph.Edge
						for _, edge := range g.GetOutEdges("consumer/src/lib.rs::run") {
							if edge.Kind == graph.EdgeCalls && edge.Line == 8 {
								parked = append(parked, edge)
							}
						}
						require.Len(t, parked, 1)
						require.Equal(t, "unresolved::helper", parked[0].To)
						require.False(t, parked[0].CrossRepo)
						writeWorkspaceFile(t, provider, "src/util.rs", "pub fn helper() {}\n")
						_, err = mi.IncrementalReindexRepo("provider", []string{filepath.Join(provider, "src/util.rs")})
					case "forced_evict_util":
						require.NoError(t, os.Remove(filepath.Join(provider, "src/util.rs")))
						mi.GetIndexer("provider").EvictFile(filepath.Join(provider, "src/util.rs"))
					case "consumer_full":
						_, err = mi.IndexRepo("consumer")
					case "provider_full":
						var result *IndexResult
						result, err = mi.IndexRepo("provider")
						require.NoError(t, err)
						require.Contains(t, result.CrossRepoResolveFiles, "consumer/src/lib.rs")
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
								require.NotContains(t, result.DerivedInvalidation.Files, "consumer/src/lib.rs")
							}
						}
						if backend == "sqlite" {
							require.Contains(t, output.Files, "consumer/src/lib.rs")
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
					if operation == "provider_delete_util_batched" || operation == "forced_evict_util" {
						var parked []*graph.Edge
						for _, edge := range g.GetOutEdges("consumer/src/lib.rs::run") {
							if edge.Kind == graph.EdgeCalls && edge.Line == 8 {
								parked = append(parked, edge)
							}
						}
						require.Len(t, parked, 1)
						require.Equal(t, "unresolved::helper", parked[0].To)
						require.Equal(t, parked[0].To, parked[0].Meta["cargo_call_stub"])
						require.False(t, parked[0].CrossRepo)
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
					require.Equal(t, cargoLifecycleBindings(cold, operation), cargoLifecycleBindings(g, operation), "after %s", operation)
					require.Empty(t, logs.FilterMessage("cross-repo resolve: pass start").All())
					require.Empty(t, logs.FilterMessage("DEFERRED-TIMING master.ResolveAll").All())
				})
			}
		}
	}
}

const cargoLifecycleProvider = "pub mod util;\npub struct Foo;\nimpl Foo {\n    pub fn new() -> Foo { Foo }\n    pub fn go(&self) {}\n}\npub fn action() {}\n"
const cargoLifecycleConsumer = "use other_crate::Foo;\nuse other_crate::action;\nuse other_crate::util::helper;\npub fn run() {\n    let f = Foo::new();\n    f.go();\n    action();\n    helper();\n}\n"

func cargoLifecycleBindings(g graph.Store, operation string) []string {
	var bindings []string
	for _, node := range g.GetFileNodes("consumer/src/lib.rs") {
		for _, edge := range g.GetOutEdges(node.ID) {
			if edge.Kind == graph.EdgeImports || edge.Kind == graph.EdgeCalls {
				var stub any
				if edge.Kind == graph.EdgeCalls {
					stub = edge.Meta["cargo_call_stub"]
					if edge.Line == 8 && (operation == "provider_delete_util_batched" || operation == "forced_evict_util") {
						stub = nil // Cold has no prior bind; the exact parked marker is asserted separately.
					}
				}
				bindings = append(bindings, fmt.Sprintf("%s -%s-> %s cross=%v line=%d stub=%v", edge.From, edge.Kind, edge.To, edge.CrossRepo, edge.Line, stub))
			}
		}
	}
	slices.Sort(bindings)
	return bindings
}

// Observe only the shared resolver; the per-repo indexer retains its native store.
type cargoImportReindexObserver struct {
	graph.Store
	importReindexes int
	importReads     int
}

func (s *cargoImportReindexObserver) ReindexEdges(batch []graph.EdgeReindex) {
	for _, change := range batch {
		if change.Edge != nil && change.Edge.Kind == graph.EdgeImports && change.Edge.FilePath == "consumer/src/lib.rs" {
			s.importReindexes++
		}
	}
	s.Store.ReindexEdges(batch)
}

func (s *cargoImportReindexObserver) GetEdgeCandidates(endpoints []graph.EdgeEndpoint, sites []graph.EdgeSite) graph.EdgeCandidateSet {
	for _, site := range sites {
		if site.Kind == graph.EdgeImports && site.From == "consumer/src/lib.rs" {
			s.importReads++
		}
	}
	return s.Store.GetEdgeCandidates(endpoints, sites)
}

func TestCargoAddedFileWithoutConsumersDoesNotRunGlobalResolve(t *testing.T) {
	core, logs := observer.New(zap.InfoLevel)
	g := graph.New()
	g.AddNode(&graph.Node{ID: "consumer/src/lib.rs::run", Kind: graph.KindFunction, Name: "run", FilePath: "consumer/src/lib.rs", RepoPrefix: "consumer", Language: "rust"})
	g.AddNode(&graph.Node{ID: "provider/src/new.rs::f", Kind: graph.KindFunction, Name: "f", FilePath: "provider/src/new.rs", RepoPrefix: "provider", Language: "rust"})
	g.AddEdge(&graph.Edge{From: "consumer/src/lib.rs::run", To: "unresolved::f", Kind: graph.EdgeCalls, FilePath: "consumer/src/lib.rs"})
	mi := &MultiIndexer{graph: g, logger: zap.New(core)}
	mi.resolveIncrementalRepoMutation("provider", &IndexResult{}, &graph.MutationReceipt{Complete: true},
		&reparsePendingEnrichmentBatch{cargoAddedFile: true})
	require.Empty(t, logs.FilterMessage("cross-repo resolve: pass start").All())
}
