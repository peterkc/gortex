package indexer

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	"github.com/zzet/gortex/internal/config"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/graph/store_sqlite"
	"github.com/zzet/gortex/internal/parser/languages"
	"github.com/zzet/gortex/internal/search"
)

func newPythonBridgeIdentityIndexer(t *testing.T, backend string, entries []config.RepoEntry) (*MultiIndexer, graph.Store) {
	t.Helper()
	var g graph.Store = graph.New()
	if backend == "sqlite" {
		db, err := store_sqlite.Open(filepath.Join(t.TempDir(), "graph.sqlite"))
		require.NoError(t, err)
		t.Cleanup(func() { require.NoError(t, db.Close()) })
		g = db
	}
	cm := newTestConfigManager(t)
	cm.Global().Repos = entries
	reg := newTestRegistry()
	reg.Register(languages.NewPythonExtractor())
	mi := NewMultiIndexer(g, reg, search.NewNull(), cm, zap.NewNop())
	t.Cleanup(func() { require.NoError(t, mi.Close(context.Background())) })
	return mi, g
}

func TestTrackedPythonSameLineImportIdentities(t *testing.T) {
	for _, backend := range []string{"memory", "sqlite"} {
		t.Run(backend, func(t *testing.T) {
			provider, consumer := t.TempDir(), t.TempDir()
			writeWorkspaceFile(t, provider, "pyproject.toml", "[project]\nname = 'dist'\n")
			writeWorkspaceFile(t, provider, "pkg/__init__.py", "")
			writeWorkspaceFile(t, provider, "pkg/a.py", "def f(): pass\n")
			writeWorkspaceFile(t, provider, "pkg/b.py", "def g(): pass\n")
			writeWorkspaceFile(t, consumer, "pyproject.toml", "[project]\ndependencies = ['dist']\n")
			source := "import pkg.a, pkg.b\ndef run():\n    pkg.b.g()\n"
			writeWorkspaceFile(t, consumer, "app.py", source)
			entries := []config.RepoEntry{{Path: consumer, Name: "consumer", Workspace: "shared"}, {Path: provider, Name: "provider", Workspace: "shared"}}
			mi, g := newPythonBridgeIdentityIndexer(t, backend, entries)
			_, err := mi.IndexAll()
			require.NoError(t, err)
			check := func() {
				require.ElementsMatch(t, []string{"provider/pkg/a.py", "provider/pkg/b.py"}, revisionCallTargets(g, "consumer/app.py", graph.EdgeImports))
				require.ElementsMatch(t, []string{"provider/pkg/b.py::g"}, revisionCallTargets(g, "consumer/app.py::run", graph.EdgeCalls))
			}
			// Given two dotted imports on one line, cold indexing keeps both.
			check()
			// No-change and edited-file saves must link those same two files.
			require.NoError(t, mi.GetIndexer("consumer").IndexFile(filepath.Join(consumer, "app.py")))
			check()
			writeWorkspaceFile(t, consumer, "app.py", source+"# comment\n")
			require.NoError(t, mi.GetIndexer("consumer").IndexFile(filepath.Join(consumer, "app.py")))
			check()
			cold, coldGraph := newPythonBridgeIdentityIndexer(t, backend, entries)
			_, err = cold.IndexAll()
			require.NoError(t, err)
			require.ElementsMatch(t, revisionCallTargets(coldGraph, "consumer/app.py", graph.EdgeImports), revisionCallTargets(g, "consumer/app.py", graph.EdgeImports))
		})
	}
}
