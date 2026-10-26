package indexer

import (
	"github.com/stretchr/testify/require"
	"github.com/zzet/gortex/internal/config"
	"github.com/zzet/gortex/internal/graph"
	"testing"
)

func TestPythonUnregisteredConfigDoesNotKeepWarming(t *testing.T) {
	for _, backend := range []string{"memory", "sqlite"} {
		t.Run(backend, func(t *testing.T) {
			g := &pythonLookupScanStore{Store: cargoWarmStore(t, backend)}
			g.AddNode(&graph.Node{ID: "provider/pkg/__init__.py", FilePath: "provider/pkg/__init__.py", RepoPrefix: "provider", Kind: graph.KindFile})
			cm := newTestConfigManager(t)
			cm.Global().Repos = []config.RepoEntry{{Name: "provider", Path: t.TempDir()}, {Name: "missing", Path: t.TempDir() + "/absent"}, {Name: "failed", Path: t.TempDir()}}
			mi := &MultiIndexer{graph: g, configMgr: cm, repos: map[string]*RepoMetadata{"provider": {RootPath: cm.Global().Repos[0].Path}}}
			for range 2 {
				require.Empty(t, mi.pythonAddedFileConsumers([]string{"provider/pkg/extra.py"}))
			}
			require.Zero(t, g.importScans, "unregistered config is not a live warmup")
			for _, parallel := range []bool{false, true} {
				if parallel {
					mi.BeginParallelBatch()
				} else {
					mi.BeginBatch()
				}
				require.Empty(t, mi.pythonAddedFileConsumers([]string{"provider/pkg/extra.py"}))
				require.Zero(t, g.importScans, "a one-repo graph has no pending foreign consumers")
				mi.ResetBatch()
			}
		})
	}
}

func TestPythonAddedFileWarmGateRequiresPendingGraphRepo(t *testing.T) {
	for _, backend := range []string{"memory", "sqlite"} {
		for _, parallel := range []bool{false, true} {
			t.Run(backend+map[bool]string{false: "/serial", true: "/parallel"}[parallel], func(t *testing.T) {
				g := &pythonLookupScanStore{Store: cargoWarmStore(t, backend)}
				for _, file := range []string{"provider/pkg/__init__.py", "consumer/app.py"} {
					g.AddNode(&graph.Node{ID: file, FilePath: file, RepoPrefix: graph.RepoPrefixOfID(file), Kind: graph.KindFile})
				}
				g.AddEdge(&graph.Edge{From: "consumer/app.py", FilePath: "consumer/app.py", To: "external::pkg.extra", Kind: graph.EdgeImports})
				mi := &MultiIndexer{graph: g, repos: map[string]*RepoMetadata{"provider": {RootPath: t.TempDir()}}}
				if parallel {
					mi.BeginParallelBatch()
				} else {
					mi.BeginBatch()
				}
				require.Equal(t, []string{"consumer/app.py"}, mi.pythonAddedFileConsumers([]string{"provider/pkg/extra.py"}))
				require.Equal(t, 1, g.importScans)
				mi.ResetBatch()
				g.importScans = 0
				require.Empty(t, mi.pythonAddedFileConsumers([]string{"provider/pkg/next.py"}))
				require.Zero(t, g.importScans, "a completed warmup must not leave admission open")
			})
		}
	}
}
