package indexer

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	"github.com/zzet/gortex/internal/config"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/graph/store_sqlite"
	"github.com/zzet/gortex/internal/search"
)

func TestGoModuleImportTieBrokenByRename(t *testing.T) {
	for _, backend := range []string{"memory", "sqlite"} {
		t.Run(backend, func(t *testing.T) {
			mi, g, repos := goModuleLifecycleFixture(t, backend)
			a := repos[0].Path
			writeFile(t, filepath.Join(a, "go.mod"), "module example.org/caller\nrequire example.org/library v1.0.0\n")
			entries := []config.RepoEntry{repos[0], repos[1], repos[2]}
			for _, e := range entries {
				_, err := mi.TrackRepoCtx(t.Context(), e)
				require.NoError(t, err)
			}
			mi.RunGlobalResolve()
			assert.False(t, goModuleLifecycleCall(t, g).CrossRepo, "ambiguous modules must not bind")
			manifest := filepath.Join(repos[2].Path, "go.mod")
			writeFile(t, manifest, "module example.org/forked\n")
			require.NoError(t, mi.GetIndexer("fork").IndexFile(manifest))
			assert.Equal(t, "b/lib.go::Hello", goModuleLifecycleCall(t, g).To)
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
			assert.Equal(t, goModuleLifecycleCall(t, coldGraph).To, goModuleLifecycleCall(t, g).To)
			assert.Equal(t, goImportRecoveryRows(coldGraph, "a/main.go"), goImportRecoveryRows(g, "a/main.go"))
		})
	}
}
