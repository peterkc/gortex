package indexer

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/zzet/gortex/internal/config"
)

func TestRepositoryPrefixCollision(t *testing.T) {
	for _, reconcile := range []bool{false, true} {
		name := "track"
		if reconcile {
			name = "reconcile"
		}
		t.Run(name, func(t *testing.T) {
			first := setupRepoDir(t, "src")
			second := setupRepoDir(t, "src")
			writeFile(t, filepath.Join(first, ".gortex.yaml"), "workspace: owner\nexclude:\n  - owner-only\n")
			writeFile(t, filepath.Join(second, ".gortex.yaml"), "workspace: rejected\nexclude:\n  - rejected-only\n")
			writeFile(t, filepath.Join(second, "other.go"), "package main\nfunc Rejected() {}\n")
			g, mi, cm := newWorktreeTestIndexer(t)
			t.Cleanup(func() { require.NoError(t, mi.Close(context.Background())) })
			result, err := mi.TrackRepoCtx(t.Context(), config.RepoEntry{Path: first})
			require.NoError(t, err)
			require.NotNil(t, result)
			nodes, edges := g.AllNodes(), g.AllEdges()
			ownerConfig := cm.GetRepoConfig("src")
			ownerMeta := *mi.GetMetadata("src")
			revision := cm.Revision()
			entry := config.RepoEntry{Path: second}
			if reconcile {
				result, err = mi.ReconcileRepoCtx(t.Context(), entry, map[string]int64{"main.go": 1})
			} else {
				result, err = mi.TrackRepoCtx(t.Context(), entry)
			}
			if reconcile {
				// Startup keeps the base outcome: not loaded, warning only.
				require.NoError(t, err)
			} else {
				var collision *PrefixCollisionError
				require.ErrorAs(t, err, &collision)
				assert.Equal(t, "src", collision.Prefix)
				assert.Equal(t, first, collision.ExistingPath)
				assert.Equal(t, second, collision.Path)
				assert.Contains(t, err.Error(), first)
				assert.Contains(t, err.Error(), "gortex track --name <other>")
			}
			assert.Nil(t, result)
			assert.Len(t, mi.AllMetadata(), 1)
			assert.Equal(t, ownerMeta, *mi.GetMetadata("src"))
			assert.ElementsMatch(t, nodes, g.AllNodes())
			assert.ElementsMatch(t, edges, g.AllEdges())
			assert.Equal(t, ownerConfig, cm.GetRepoConfig("src"))
			assert.Equal(t, revision, cm.Revision(), "rejection must not publish workspace config")
			assert.Len(t, cm.Global().Repos, 1)

			if reconcile {
				result, err = mi.ReconcileRepoCtx(t.Context(), config.RepoEntry{Path: first}, nil)
			} else {
				result, err = mi.TrackRepoCtx(t.Context(), config.RepoEntry{Path: first})
			}
			require.NoError(t, err)
			require.Nil(t, result, "same path remains a no-op")
			result, err = mi.TrackRepoCtx(t.Context(), config.RepoEntry{Path: second, Name: "src2"})
			require.NoError(t, err)
			require.NotNil(t, result)
			assert.Equal(t, "src2", result.RepoPrefix)
			assert.Len(t, mi.AllMetadata(), 2)
			assert.NotEmpty(t, g.GetFileNodes("src2/other.go"))
			assert.Empty(t, g.GetFileNodes("src/other.go"))
			assert.Equal(t, "owner", cm.GetRepoConfig("src").Workspace)

			// A plain re-track of the renamed repository stays a no-op.
			if reconcile {
				result, err = mi.ReconcileRepoCtx(t.Context(), config.RepoEntry{Path: second}, map[string]int64{"other.go": 1})
			} else {
				result, err = mi.TrackRepoCtx(t.Context(), config.RepoEntry{Path: second})
			}
			require.NoError(t, err)
			require.Nil(t, result)
			assert.Len(t, mi.AllMetadata(), 2)
			assert.Equal(t, "owner", cm.GetRepoConfig("src").Workspace, "a no-op re-track must not touch the owner's config")
		})
	}
}

// A symlinked spelling of the owning directory is the same repository.
func TestRepositoryPrefixCollisionSymlinkAlias(t *testing.T) {
	first := setupRepoDir(t, "src")
	alias := filepath.Join(t.TempDir(), "src")
	require.NoError(t, os.Symlink(first, alias))
	_, mi, _ := newWorktreeTestIndexer(t)
	t.Cleanup(func() { require.NoError(t, mi.Close(context.Background())) })
	_, err := mi.TrackRepoCtx(t.Context(), config.RepoEntry{Path: first})
	require.NoError(t, err)
	result, err := mi.TrackRepoCtx(t.Context(), config.RepoEntry{Path: alias})
	require.NoError(t, err)
	require.Nil(t, result)
	assert.Len(t, mi.AllMetadata(), 1)
}

// A loser that resolves its prefix after the owner published must not
// replace the owner's cached config.
func TestRepositoryPrefixCollisionLateLoserKeepsOwnerConfig(t *testing.T) {
	first, second := setupRepoDir(t, "src"), setupRepoDir(t, "src")
	writeFile(t, filepath.Join(first, ".gortex.yaml"), "workspace: owner\n")
	writeFile(t, filepath.Join(second, ".gortex.yaml"), "workspace: rejected\n")
	_, mi, cm := newWorktreeTestIndexer(t)
	t.Cleanup(func() { require.NoError(t, mi.Close(context.Background())) })
	_, err := mi.TrackRepoCtx(t.Context(), config.RepoEntry{Path: first})
	require.NoError(t, err)
	entry := config.RepoEntry{Path: second}
	prefix, _ := mi.resolveTrackPrefix(&entry, second, nil)
	assert.Equal(t, "src", prefix)
	assert.Equal(t, "owner", cm.GetRepoConfig("src").Workspace)
}

func TestRepositoryPrefixCollisionWorktree(t *testing.T) {
	canon, wt := setupCollidingWorktree(t, "src", "task", "task-ws")
	_, mi, cm := newWorktreeTestIndexer(t)
	t.Cleanup(func() { require.NoError(t, mi.Close(context.Background())) })
	_, err := mi.TrackRepoCtx(t.Context(), config.RepoEntry{Path: canon})
	require.NoError(t, err)
	ownerConfig := cm.GetRepoConfig("src")
	result, err := mi.TrackRepoCtx(t.Context(), config.RepoEntry{Path: wt})
	require.NoError(t, err)
	require.NotNil(t, result)
	assert.Equal(t, "src@task-ws", result.RepoPrefix)
	assert.Len(t, mi.AllMetadata(), 2)
	assert.Equal(t, ownerConfig, cm.GetRepoConfig("src"))
}

// Keep the owner's last good parse even if its file disappears before rejection.
func TestRepositoryPrefixCollisionPreservesCachedConfig(t *testing.T) {
	first, second := setupRepoDir(t, "src"), setupRepoDir(t, "src")
	writeFile(t, filepath.Join(first, ".gortex.yaml"), "workspace: owner\n")
	writeFile(t, filepath.Join(second, ".gortex.yaml"), "workspace: rejected\n")
	_, mi, cm := newWorktreeTestIndexer(t)
	t.Cleanup(func() { require.NoError(t, mi.Close(context.Background())) })
	_, err := mi.TrackRepoCtx(t.Context(), config.RepoEntry{Path: first})
	require.NoError(t, err)
	require.NoError(t, os.Remove(filepath.Join(first, ".gortex.yaml")))
	_, err = mi.TrackRepoCtx(t.Context(), config.RepoEntry{Path: second})
	require.Error(t, err)
	assert.Equal(t, "owner", cm.GetRepoConfig("src").Workspace)
}

func TestRepositoryPrefixCollisionAfterCoordination(t *testing.T) {
	for _, reconcile := range []bool{false, true} {
		for _, samePath := range []bool{false, true} {
			t.Run(fmt.Sprintf("reconcile=%v/same=%v", reconcile, samePath), func(t *testing.T) {
				first, second := setupRepoDir(t, "src"), setupRepoDir(t, "src")
				if samePath {
					second = first
				}
				writeFile(t, filepath.Join(first, ".gortex.yaml"), "workspace: owner\n")
				if !samePath {
					writeFile(t, filepath.Join(second, ".gortex.yaml"), "workspace: rejected\n")
				}
				g, mi, cm := newWorktreeTestIndexer(t)
				t.Cleanup(func() { require.NoError(t, mi.Close(context.Background())) })
				// Admit the winner after the outer call resolved its prefix and took
				// its first registry snapshot, but before it acquires the stable lane.
				admitted := false
				coordinator := mi.repositoryMutationCoordinator("src")
				coordinator.batchAdmissionHook = func() {
					if admitted {
						return
					}
					admitted = true
					result, err := mi.TrackRepoCtx(t.Context(), config.RepoEntry{Path: first})
					require.NoError(t, err)
					require.NotNil(t, result)
				}
				var result *IndexResult
				var err error
				if reconcile {
					result, err = mi.ReconcileRepoCtx(t.Context(), config.RepoEntry{Path: second}, map[string]int64{"main.go": 1})
				} else {
					result, err = mi.TrackRepoCtx(t.Context(), config.RepoEntry{Path: second})
				}
				require.True(t, admitted)
				if samePath || reconcile {
					require.NoError(t, err)
				} else {
					var collision *PrefixCollisionError
					require.ErrorAs(t, err, &collision)
					assert.Equal(t, first, collision.ExistingPath)
				}
				assert.Nil(t, result)
				assert.Len(t, mi.AllMetadata(), 1)
				assert.Equal(t, first, mi.GetMetadata("src").RootPath)
				assert.Equal(t, "owner", cm.GetRepoConfig("src").Workspace)
				assert.NotEmpty(t, g.GetFileNodes("src/main.go"))
				assert.Len(t, cm.Global().Repos, 1)
			})
		}
	}
}

func TestRepositoryPrefixCollisionLifecycleHasNoSideEffects(t *testing.T) {
	f := newLifecycleFixture(t)
	t.Cleanup(f.close)
	first, second := setupRepoDir(t, "src"), setupRepoDir(t, "src")
	_, err := f.lc.Register(t.Context(), config.RepoEntry{Path: first}, TrackSourceCLI)
	require.NoError(t, err)
	before, err := os.ReadFile(f.cfgPath)
	require.NoError(t, err)
	calls := append([]string(nil), f.watcher.calls...)
	invalidated, analysed := f.notify.counts()
	result, err := f.lc.Register(t.Context(), config.RepoEntry{Path: second}, TrackSourceCLI)
	var collision *PrefixCollisionError
	require.ErrorAs(t, err, &collision)
	assert.Equal(t, RegisterResult{}, result)
	after, err := os.ReadFile(f.cfgPath)
	require.NoError(t, err)
	assert.Equal(t, before, after)
	assert.Equal(t, calls, f.watcher.calls)
	afterInvalidated, afterAnalysed := f.notify.counts()
	assert.Equal(t, invalidated, afterInvalidated)
	assert.Equal(t, analysed, afterAnalysed)
}
