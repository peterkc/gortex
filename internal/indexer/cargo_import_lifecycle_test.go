package indexer

import (
	"context"
	"encoding/json"
	"path/filepath"
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
			for _, operation := range []string{"consumer_save", "provider_save", "consumer_full", "provider_full", "warm_full_retrack", "untrack_retrack", "no_change", "track"} {
				order := "consumer_first"
				if providerFirst {
					order = "provider_first"
				}
				t.Run(backend+"/"+order+"/"+operation, func(t *testing.T) {
					provider, consumer := t.TempDir(), t.TempDir()
					writeWorkspaceFile(t, provider, "Cargo.toml", "[package]\nname = 'other-crate'\nversion = '0.1.0'\n")
					writeWorkspaceFile(t, provider, "src/lib.rs", "pub fn action() {}\n")
					writeWorkspaceFile(t, consumer, "Cargo.toml", "[package]\nname = 'app'\nversion = '0.1.0'\n[dependencies]\nother-crate = '1'\n")
					writeWorkspaceFile(t, consumer, "src/lib.rs", "use other_crate::action;\npub fn run() { action(); }\n")
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
					if operation == "track" {
						for _, entry := range gc.Repos {
							_, err = mi.TrackRepoCtx(context.Background(), entry)
							require.NoError(t, err)
						}
					} else {
						_, err = mi.IndexAll()
						require.NoError(t, err)
					}
					assertImport := func() {
						t.Helper()
						var imports []*graph.Edge
						for _, edge := range g.GetOutEdges("consumer/src/lib.rs") {
							if edge.Kind == graph.EdgeImports {
								imports = append(imports, edge)
							}
						}
						require.Len(t, imports, 1)
						require.Equal(t, "provider/src/lib.rs", imports[0].To)
						require.True(t, imports[0].CrossRepo)
						var calls []*graph.Edge
						for _, edge := range g.GetOutEdges("consumer/src/lib.rs::run") {
							if edge.Kind == graph.EdgeCalls {
								calls = append(calls, edge)
							}
						}
						require.Len(t, calls, 1)
						require.Equal(t, "provider/src/lib.rs::action", calls[0].To)
						require.True(t, calls[0].CrossRepo)
					}
					assertImport()
					logs.TakeAll()
					switch operation {
					case "consumer_save":
						writeWorkspaceFile(t, consumer, "src/lib.rs", "use other_crate::action;\npub fn run() { action(); }\npub fn edited() {}\n")
						_, err = mi.IncrementalReindexRepo("consumer", []string{filepath.Join(consumer, "src/lib.rs")})
					case "provider_save":
						writeWorkspaceFile(t, provider, "src/lib.rs", "pub fn action() {}\npub fn added() {}\n")
						_, err = mi.IncrementalReindexRepo("provider", []string{filepath.Join(provider, "src/lib.rs")})
					case "consumer_full":
						_, err = mi.IndexRepo("consumer")
					case "provider_full":
						_, err = mi.IndexRepo("provider")
					case "warm_full_retrack":
						prior := map[string]map[string]int64{"consumer": mi.FileMtimes("consumer"), "provider": mi.FileMtimes("provider")}
						restarted := NewMultiIndexer(g, reg, search.NewNull(), cm, logger)
						t.Cleanup(func() { require.NoError(t, restarted.Close(context.Background())) })
						restarted.BeginBatch()
						_, err = restarted.ReconcileRepoCtx(context.Background(), entries[0], prior["consumer"])
						require.NoError(t, err)
						t.Setenv("GORTEX_WARMUP_FULL_RETRACK", "1")
						result, restoreErr := restarted.ReconcileRepoCtx(context.Background(), entries[1], prior["provider"])
						require.NoError(t, restoreErr)
						require.True(t, result.FullRetrack)
						data, marshalErr := json.Marshal(result)
						require.NoError(t, marshalErr)
						var output struct {
							Files []string `json:"cross_repo_resolve_files"`
						}
						require.NoError(t, json.Unmarshal(data, &output))
						if backend == "sqlite" {
							require.Contains(t, output.Files, "consumer/src/lib.rs")
						} else {
							require.Empty(t, output.Files, "memory replacement preserved the existing import")
						}
						require.NotContains(t, result.DerivedInvalidation.Files, "consumer/src/lib.rs")
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
					assertImport()
					require.Empty(t, logs.FilterMessage("cross-repo resolve: pass start").All())
					require.Empty(t, logs.FilterMessage("DEFERRED-TIMING master.ResolveAll").All())
				})
			}
		}
	}
}
