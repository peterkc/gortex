package indexer

import (
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/zzet/gortex/internal/config"
	"github.com/zzet/gortex/internal/graph/store_sqlite"
	"github.com/zzet/gortex/internal/pathkey"
)

func TestConfiguredStaleCatalogOwnerRebindsWhenUnconfigured(t *testing.T) {
	for _, scenario := range []string{"offline-untrack", "implicit-auto-index", "online-track"} {
		t.Run(scenario, func(t *testing.T) {
			f := newLifecycleFixture(t)
			t.Cleanup(f.close)
			old, replacement := f.gitRepo("old"), f.gitRepo("replacement")
			entry := config.RepoEntry{Path: replacement, Name: "src"}
			if scenario == "implicit-auto-index" {
				_, err := f.mi.TrackRepoCtx(t.Context(), config.RepoEntry{Path: old, Name: "src"})
				require.NoError(t, err)
				require.NoError(t, f.lc.RecordImplicit(t.Context(), old))
			} else {
				result, err := f.lc.Register(t.Context(), config.RepoEntry{Path: old, Name: "src"}, TrackSourceCLI)
				require.NoError(t, err)
				require.NoError(t, result.CatalogErr)
			}
			oldGraph, found, err := f.catalog.GetDedicatedGraph(t.Context(), GraphIDFor("src"))
			require.NoError(t, err)
			require.True(t, found)
			f.cm.Global().Repos = []config.RepoEntry{entry}
			require.NoError(t, f.cm.Global().Save())
			f.restart()
			if scenario == "online-track" {
				result, err := f.lc.Register(t.Context(), entry, TrackSourceCLI)
				require.NoError(t, err)
				require.NoError(t, result.CatalogErr)
				f.restart()
			}
			entries := f.cm.Global().ReposSnapshot()
			selected := f.mi.ResolveConfiguredRepos(t.Context(), entries)
			require.Equal(t, entries, selected, "stale catalog rows must not hide configured roots")
			err = f.lc.Seed(t.Context())
			require.NoError(t, err, "an unconfigured, non-live, unpublished owner can be rebound")
			after, found, err := f.catalog.GetDedicatedGraph(t.Context(), oldGraph.GraphID)
			require.NoError(t, err)
			require.True(t, found)
			checkout, found, err := f.catalog.GetCheckout(t.Context(), after.OwnerCheckoutID)
			require.NoError(t, err)
			require.True(t, found)
			assert.NotEqual(t, oldGraph.OwnerCheckoutID, after.OwnerCheckoutID)
			assert.True(t, pathkey.EqualPaths(pathkey.CanonicalPath(replacement), checkout.RootPath), "checkout root %q", checkout.RootPath)
		})
	}
}

func TestSeedLegacyPrefixCollisionKeepsConfiguredOwner(t *testing.T) {
	f := newLifecycleFixture(t)
	t.Cleanup(f.close)
	owner, loser := f.gitRepo("owner"), f.gitRepo("loser")
	result, err := f.lc.Register(t.Context(), config.RepoEntry{Path: owner, Name: "src"}, TrackSourceCLI)
	require.NoError(t, err)
	require.NoError(t, result.CatalogErr)
	binding, found, err := f.catalog.GetDedicatedGraph(t.Context(), GraphIDFor("src"))
	require.NoError(t, err)
	require.True(t, found)
	// Explicitly leave a pre-publication row: generic upsert may rebind it on main.
	require.NoError(t, f.catalog.DeleteDedicatedGraph(t.Context(), binding.GraphID))
	binding.ActiveGenerationID = 0
	require.NoError(t, f.catalog.UpsertDedicatedGraph(t.Context(), binding))
	_, published, err := f.catalog.DedicatedBasePublication(t.Context(), binding.GraphID)
	require.NoError(t, err)
	require.False(t, published)
	f.cm.Global().Repos = []config.RepoEntry{{Path: owner, Name: "src"}, {Path: loser, Name: "src"}}
	require.NoError(t, f.cm.Global().Save())
	f.restart()
	require.NoError(t, f.lc.Seed(t.Context()))
	after, found, err := f.catalog.GetDedicatedGraph(t.Context(), binding.GraphID)
	require.NoError(t, err)
	require.True(t, found)
	assert.Equal(t, binding, after)
	families, err := f.catalog.ListRepositoryFamilies(t.Context())
	require.NoError(t, err)
	assert.Len(t, families, 1)
}

func TestPlainLinkedWorktreeDaemonPrefix(t *testing.T) {
	f := newLifecycleFixture(t)
	t.Cleanup(f.close)
	main := f.gitRepo("src")
	linked := filepath.Join(t.TempDir(), "src")
	runGit(t, main, "worktree", "add", "-q", "-b", "linked", linked)
	result, err := f.lc.Register(t.Context(), config.RepoEntry{Path: main}, TrackSourceCLI)
	require.NoError(t, err)
	require.NoError(t, result.CatalogErr)
	result, err = f.lc.Register(t.Context(), config.RepoEntry{Path: linked}, TrackSourceCLI)
	require.NoError(t, err)
	require.NoError(t, result.CatalogErr)
	assert.Equal(t, "src@src", result.Prefix)
}

func TestDedicatedBindingKeepsConfiguredOrLiveOwner(t *testing.T) {
	for _, scenario := range []string{"configured", "configured-renamed", "live", "published"} {
		t.Run(scenario, func(t *testing.T) {
			f := newLifecycleFixture(t)
			t.Cleanup(f.close)
			old, replacement := f.gitRepo("old"), f.gitRepo("replacement")
			result, err := f.lc.Register(t.Context(), config.RepoEntry{Path: old, Name: "src"}, TrackSourceCLI)
			require.NoError(t, err)
			require.NoError(t, result.CatalogErr)
			before, found, err := f.catalog.GetDedicatedGraph(t.Context(), GraphIDFor("src"))
			require.NoError(t, err)
			require.True(t, found)
			entries := []config.RepoEntry{{Path: replacement, Name: "src"}}
			if scenario == "configured" {
				entries = append(entries, config.RepoEntry{Path: old, Name: "src"})
			}
			if scenario == "configured-renamed" {
				entries = append(entries, config.RepoEntry{Path: old, Name: "src-old"})
			}
			f.cm.Global().Repos = entries
			require.NoError(t, f.cm.Global().Save())
			if scenario == "published" {
				guardConfiguredOwner(t, f, "src")
			}
			if scenario != "live" {
				f.restart()
			}
			_, err = f.lc.recordCheckout(t.Context(), "src", replacement, TrackSourceCLI, false)
			require.ErrorIs(t, err, store_sqlite.ErrCatalogStaleGuard)
			after, found, err := f.catalog.GetDedicatedGraph(t.Context(), before.GraphID)
			require.NoError(t, err)
			require.True(t, found)
			assert.Equal(t, before.OwnerCheckoutID, after.OwnerCheckoutID)
		})
	}
}
