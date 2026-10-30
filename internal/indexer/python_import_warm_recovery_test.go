package indexer

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	"github.com/zzet/gortex/internal/config"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/parser/languages"
	"github.com/zzet/gortex/internal/search"
)

func TestTrackedPythonWarmFileRecovery(t *testing.T) {
	for _, backend := range []string{"memory", "sqlite"} {
		for _, providerFirst := range []bool{false, true} {
			for _, deferred := range []bool{false, true} {
				for _, restore := range []bool{false, true} {
					t.Run(fmt.Sprintf("%s/provider_first=%v/deferred=%v/restore=%v", backend, providerFirst, deferred, restore), func(t *testing.T) {
						provider, consumer := t.TempDir(), t.TempDir()
						writeWorkspaceFile(t, provider, "pyproject.toml", "[project]\nname = 'pillow'\n")
						writeWorkspaceFile(t, provider, "src/PIL/__init__.py", "")
						writeWorkspaceFile(t, provider, "src/PIL/image.py", pythonLifecycleProvider)
						writeWorkspaceFile(t, provider, "src/PIL/util.py", "def helper(): pass\n")
						writeWorkspaceFile(t, consumer, "pyproject.toml", "[project]\ndependencies = ['pillow']\n")
						consumerSource := pythonLifecycleConsumer
						if !restore {
							consumerSource += "\nimport PIL.extra\n"
						}
						writeWorkspaceFile(t, consumer, "app.py", consumerSource)
						for i := 0; i < 6; i++ {
							writeWorkspaceFile(t, provider, fmt.Sprintf("pad%d.py", i), "def pad(): pass\n")
							writeWorkspaceFile(t, consumer, fmt.Sprintf("pad%d.py", i), "def pad(): pass\n")
						}
						entries := []config.RepoEntry{{Path: consumer, Name: "consumer", Workspace: "shared"}, {Path: provider, Name: "provider", Workspace: "shared"}}
						if providerFirst {
							entries[0], entries[1] = entries[1], entries[0]
						}
						cfgPath := filepath.Join(t.TempDir(), "config.yaml")
						gc := &config.GlobalConfig{Repos: entries}
						gc.SetConfigPath(cfgPath)
						require.NoError(t, gc.Save())
						cm, err := config.NewConfigManager(cfgPath)
						require.NoError(t, err)
						reg := newTestRegistry()
						reg.Register(languages.NewPythonExtractor())
						newMI := func(g graph.Store) *MultiIndexer {
							mi := NewMultiIndexer(g, reg, search.NewNull(), cm, zap.NewNop())
							t.Cleanup(func() { require.NoError(t, mi.Close(context.Background())) })
							return mi
						}
						g := pythonLocalStore(t, backend)
						mi := newMI(g)
						_, err = mi.IndexAll()
						require.NoError(t, err)
						if restore {
							file := filepath.Join(provider, "src/PIL/util.py")
							require.NoError(t, os.Remove(file))
							_, err = mi.IncrementalReindexRepo("provider", []string{file})
							require.NoError(t, err)
						}
						prior := map[string]map[string]int64{"consumer": mi.FileMtimes("consumer"), "provider": mi.FileMtimes("provider")}
						require.NoError(t, mi.Close(context.Background()))
						if restore {
							writeWorkspaceFile(t, provider, "src/PIL/util.py", "def helper(): pass\n")
						} else {
							writeWorkspaceFile(t, provider, "src/PIL/extra.py", "")
						}
						restarted := newMI(g)
						if deferred {
							restarted.BeginParallelBatch()
						} else {
							restarted.BeginBatch()
						}
						var files []string
						for _, entry := range entries {
							result, err := restarted.ReconcileRepoCtx(context.Background(), entry, prior[entry.Name])
							require.NoError(t, err)
							require.False(t, result.FullRetrack, "must exercise scoped warm reconcile")
							if entry.Name == "provider" {
								require.Contains(t, result.CrossRepoResolveFiles, "consumer/app.py")
							}
							files = appendUniqueSorted(files, result.DerivedInvalidation.Files...)
							files = appendUniqueSorted(files, result.CrossRepoResolveFiles...)
						}
						require.NoError(t, restarted.RunPreEnrichResolveFiles(context.Background(), files, nil))
						restarted.EndBatch()
						cold := pythonLocalStore(t, backend)
						_, err = newMI(cold).IndexAll()
						require.NoError(t, err)
						require.Equal(t, pythonLifecycleBindings(cold, ""), pythonLifecycleBindings(g, ""))
					})
				}
			}
		}
	}
}
