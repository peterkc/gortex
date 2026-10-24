package config

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/zzet/gortex/internal/pathkey"
)

func TestSnapshotRepoPathsCountLinearAllocations(t *testing.T) {
	old := pathkey.CaseInsensitivePaths
	pathkey.CaseInsensitivePaths = true
	t.Cleanup(func() { pathkey.CaseInsensitivePaths = old })
	gc := &GlobalConfig{}
	root := t.TempDir()
	for i := 0; i < 200; i++ {
		gc.Repos = append(gc.Repos, RepoEntry{Path: filepath.Join(root, fmt.Sprintf("Repo-%03d", i))})
	}
	allocs := testing.AllocsPerRun(1, func() {
		lookup := gc.snapshotRepoPaths(func(root string) string { return root })
		for _, entry := range gc.Repos {
			if lookup(entry.Path) == nil {
				t.Fatal("snapshot lost a configured root")
			}
		}
	})
	t.Logf("allocations for a 200-root snapshot and lookup pass: %.0f", allocs)
	assert.Less(t, allocs, float64(2500), "case folding must not allocate for every root pair")
}

func TestPrepareWorkspaceConfigRetainsPreparedChain(t *testing.T) {
	root := t.TempDir()
	cm, err := NewConfigManager(filepath.Join(t.TempDir(), "global.yaml"))
	require.NoError(t, err)
	_, publish := cm.PrepareWorkspaceConfig("src", root, nil)
	require.Contains(t, cm.excludeCache.chains, root)
	publish("src")
	assert.Contains(t, cm.excludeCache.chains, root, "publication must retain the chain discovered during preparation")
}

func TestLastGoodWorkspaceSymlinkIdentity(t *testing.T) {
	root, other := t.TempDir(), t.TempDir()
	alias := filepath.Join(t.TempDir(), "alias")
	if err := os.Symlink(root, alias); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	cm, err := NewConfigManager(filepath.Join(t.TempDir(), "global.yaml"))
	require.NoError(t, err)
	path := filepath.Join(root, ".gortex.yaml")
	require.NoError(t, os.WriteFile(path, []byte("workspace: team\nexclude:\n  - kept\n"), 0644))
	cm.LoadWorkspaceConfig("src", root)
	cached := cm.LastGoodWorkspace("src", root)
	require.NotNil(t, cached)
	assert.True(t, cached == cm.LastGoodWorkspace("src", alias), "an alias must retain the same last good workspace")
	assert.Nil(t, cm.LastGoodWorkspace("src", other), "a different root must not inherit the prefix owner's config")
	require.NoError(t, os.WriteFile(path, []byte("workspace: ["), 0644))
	prepared, _ := cm.PrepareWorkspaceConfig("src", alias, nil)
	assert.Equal(t, "team", prepared.Workspace)
	assert.Contains(t, prepared.Index.Exclude, "kept")
}

func TestSnapshotRepoPathsCaseCollisionMatchesLiveLookup(t *testing.T) {
	old := pathkey.CaseInsensitivePaths
	pathkey.CaseInsensitivePaths = true
	t.Cleanup(func() { pathkey.CaseInsensitivePaths = old })
	root := t.TempDir()
	upper, lower := filepath.Join(root, "Repo"), filepath.Join(root, "repo")
	require.NoError(t, os.MkdirAll(upper, 0755))
	require.NoError(t, os.MkdirAll(lower, 0755))
	gc := &GlobalConfig{Repos: []RepoEntry{{Path: upper, Name: "first"}, {Path: lower, Name: "second"}}}
	lookup := gc.SnapshotRepoPaths()
	assert.Equal(t, gc.FindRepoByPath(upper), lookup(upper))
	assert.Equal(t, gc.FindRepoByPath(lower), lookup(lower))
}

func TestConfigManagerRepoPathLookupRefreshesOnMutationAndReload(t *testing.T) {
	root, other := t.TempDir(), t.TempDir()
	cm, err := NewConfigManager(filepath.Join(t.TempDir(), "global.yaml"))
	require.NoError(t, err)
	require.NoError(t, cm.Global().AddRepo(RepoEntry{Path: root, Name: "src", Exclude: []string{"first"}}))
	require.NoError(t, cm.Global().Save())
	first := cm.RepoPathLookup()
	require.Contains(t, first(root).Exclude, "first")
	assert.Same(t, first(root), cm.RepoPathLookup()(root), "unchanged config must reuse one path snapshot")
	require.NoError(t, cm.Global().RemoveRepo(root))
	require.Nil(t, cm.RepoPathLookup()(root), "removed roots must not survive the cached lookup")
	require.NoError(t, cm.Global().AddRepo(RepoEntry{Path: other, Name: "other"}))
	require.NotNil(t, cm.RepoPathLookup()(other))
	require.NoError(t, cm.Global().Save())
	gc, err := LoadGlobal(cm.Global().ConfigPath())
	require.NoError(t, err)
	gc.Repos[0].Exclude = []string{"reloaded"}
	require.NoError(t, gc.Save())
	require.NoError(t, cm.Reload())
	require.Equal(t, []string{"reloaded"}, cm.RepoPathLookup()(other).Exclude)
}

func TestPrepareWorkspaceConfigNilLookupReusesRootSnapshot(t *testing.T) {
	root := t.TempDir()
	cm, err := NewConfigManager(filepath.Join(t.TempDir(), "global.yaml"))
	require.NoError(t, err)
	cm.Global().Repos = []RepoEntry{{Path: root, Name: "src", Exclude: []string{"repo-only"}}}
	require.NoError(t, os.WriteFile(filepath.Join(root, ".gortex.yaml"), []byte("workspace: team\n"), 0o644))
	for range 3 {
		cfg, publish := cm.PrepareWorkspaceConfig("src", root, nil)
		require.Contains(t, cfg.Index.Exclude, "repo-only")
		publish("src")
	}
	require.NotNil(t, cm.pathSnapshot, "daemon's nil-lookup path must populate the revision cache")
	first := cm.pathSnapshot.Lookup(root)
	_, _ = cm.PrepareWorkspaceConfig("src", root, nil)
	require.Same(t, first, cm.pathSnapshot.Lookup(root))
}
