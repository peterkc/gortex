package indexer

import (
	"context"
	"github.com/stretchr/testify/require"
	"github.com/zzet/gortex/internal/config"
	"github.com/zzet/gortex/internal/graph"
	"go.uber.org/zap"
	"go.uber.org/zap/zaptest/observer"
	"path/filepath"
	"testing"
)

func TestPythonSaveRunsOneIncomingResolve(t *testing.T) {
	for _, backend := range []string{"memory", "sqlite"} {
		for _, route := range []string{"receipt", "point"} {
			t.Run(backend+"/"+route, func(t *testing.T) {
				provider, consumer := t.TempDir(), t.TempDir()
				writeWorkspaceFile(t, provider, "pyproject.toml", "[project]\nname = 'dist'\n")
				writeWorkspaceFile(t, provider, "pkg/__init__.py", "")
				writeWorkspaceFile(t, provider, "pkg/image.py", "def f(): pass\n")
				writeWorkspaceFile(t, consumer, "pyproject.toml", "[project]\ndependencies = ['dist']\n")
				source := "from pkg.image import f\ndef run(): return f()\n"
				writeWorkspaceFile(t, consumer, "app.py", source)
				entries := []config.RepoEntry{{Path: consumer, Name: "consumer", Workspace: "shared"}, {Path: provider, Name: "provider", Workspace: "shared"}}
				mi, g := newPythonBridgeIdentityIndexer(t, backend, entries)
				for _, entry := range entries {
					_, err := mi.TrackRepoCtx(context.Background(), entry)
					require.NoError(t, err)
				}
				core, logs := observer.New(zap.InfoLevel)
				mi.logger = zap.New(core)
				save := func() {
					t.Helper()
					if route == "point" {
						require.NoError(t, mi.GetIndexer("consumer").IndexFile(filepath.Join(consumer, "app.py")))
					} else {
						_, err := mi.IncrementalReindexRepo("consumer", []string{filepath.Join(consumer, "app.py")})
						require.NoError(t, err)
					}
					require.Equal(t, []string{"provider/pkg/image.py::f"}, revisionCallTargets(g, "consumer/app.py::run", graph.EdgeCalls))
				}
				writeWorkspaceFile(t, consumer, "app.py", source+"def added(): pass\n")
				save()
				require.Equal(t, 1, logs.FilterMessage("DEFERRED-TIMING master.ResolveFilesAndIncoming").Len())
				logs.TakeAll()
				save()
				require.LessOrEqual(t, logs.FilterMessage("DEFERRED-TIMING master.ResolveFilesAndIncoming").Len(), 1)
			})
		}
	}
}
