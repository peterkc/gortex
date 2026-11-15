package indexer

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/zzet/gortex/internal/config"
	"github.com/zzet/gortex/internal/graph/store_sqlite"
	"github.com/zzet/gortex/internal/graphview"
)

func TestTrackPreflightCollisionDoesNotPublishWorkspace(t *testing.T) {
	f := newLifecycleFixture(t)
	t.Cleanup(f.close)
	owner, loser := f.gitRepo("owner"), f.gitRepo("loser")
	_, err := f.lc.Register(t.Context(), config.RepoEntry{Path: owner, Name: "src"}, TrackSourceCLI)
	require.NoError(t, err)
	before, revision := f.cm.GetRepoConfig("src"), f.cm.Revision()
	writeFile(t, filepath.Join(loser, ".gortex.yaml"), "workspace: rejected\nexclude:\n  - rejected-only\n")
	var collision *PrefixCollisionError
	require.ErrorAs(t, f.lc.PreflightTrack(t.Context(), config.RepoEntry{Path: loser, Name: "src"}), &collision)
	require.Equal(t, owner, collision.ExistingPath)
	require.Equal(t, before, f.cm.GetRepoConfig("src"))
	require.Equal(t, revision, f.cm.Revision())
	require.NoError(t, f.lc.PreflightTrack(t.Context(), config.RepoEntry{Path: owner}), "a root registered under another name remains a no-op")
}

func TestTrackPreflightRetainsDedicatedAdminName(t *testing.T) {
	f := newLifecycleFixture(t)
	t.Cleanup(f.close)
	main := f.gitRepo("src")
	_, err := f.lc.Register(t.Context(), config.RepoEntry{Path: main}, TrackSourceCLI)
	require.NoError(t, err)
	linked := filepath.Join(t.TempDir(), "src")
	runGit(t, main, "worktree", "add", "-q", "-b", "review", linked)
	require.NoError(t, f.lc.PreflightTrack(t.Context(), config.RepoEntry{Path: linked}))
	prepared, err := f.lc.prepareRegistrationEntry(t.Context(), config.RepoEntry{Path: linked}, time.Now().Add(prefixOwnershipTimeout))
	require.NoError(t, err)
	require.Equal(t, f.lc.dedicatedPrefixFor(t.Context(), linked), prepared.Name)
	require.NotEmpty(t, prepared.Name)
	require.NotEqual(t, "src", prepared.Name)
}

func TestPreparedPrefixRejectsConfigurationReplacement(t *testing.T) {
	_, mi, cm := newWorktreeTestIndexer(t)
	t.Cleanup(func() { require.NoError(t, mi.Close(context.Background())) })
	root := t.TempDir()
	before := cm.Revision()
	check := func(string, string) error {
		return cm.Global().AddRepo(config.RepoEntry{Path: t.TempDir(), Name: "new-owner"})
	}
	entry := config.RepoEntry{Path: root, Name: "src"}
	_, err := mi.resolveTrackPrefixContext(t.Context(), &entry, root, nil, nil, check, time.Now().Add(prefixOwnershipTimeout), nil)
	require.ErrorIs(t, err, errPrefixConfigChanged)
	require.Equal(t, before, cm.Revision(), "rejected prepared workspace cannot publish a config revision")
	require.Nil(t, mi.GetMetadata("src"))
}

func TestConfiguredTrackAliasUsesBoundedIdentity(t *testing.T) {
	f := newLifecycleFixture(t)
	t.Cleanup(f.close)
	owner := f.gitRepo("src")
	alias := filepath.Join(t.TempDir(), "src")
	if err := os.Symlink(owner, alias); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	require.NoError(t, f.cm.Global().AddRepo(config.RepoEntry{Path: owner, Name: "pinned"}))
	entry, err := f.lc.prepareRegistrationEntry(t.Context(), config.RepoEntry{Path: alias}, time.Now().Add(prefixOwnershipTimeout))
	require.NoError(t, err)
	require.Equal(t, "pinned", entry.Name)
}

func TestTrackPreflightDoesNotInspectUnrelatedConfiguredRoots(t *testing.T) {
	f := newLifecycleFixture(t)
	t.Cleanup(f.close)
	owner := f.gitRepo("owner")
	_, err := f.lc.Register(t.Context(), config.RepoEntry{Path: owner, Name: "src"}, TrackSourceCLI)
	require.NoError(t, err)
	entries := f.cm.Global().ReposSnapshot()
	offline := t.TempDir()
	for i := range 1000 {
		entries = append(entries, config.RepoEntry{Path: filepath.Join(offline, fmt.Sprintf("offline-%04d", i))})
	}
	f.cm.Global().Repos = entries
	require.NoError(t, f.cm.Global().Save())
	f.mi.ExpectRepos(entries) // startup resolves the absent roots before admission
	loser := t.TempDir()
	var foreign string
	f.mi.prefixStats.stat = func(root string) (os.FileInfo, error) {
		if root != owner && root != loser {
			foreign = root
		}
		return os.Stat(root)
	}
	var collision *PrefixCollisionError
	require.ErrorAs(t, f.lc.PreflightTrack(t.Context(), config.RepoEntry{Path: loser, Name: "src"}), &collision)
	require.Empty(t, foreign, "track admission must not stat the other 1000 configured roots")
}

func TestColdKnownConfiguredOwnerStillReturnsTypedCollision(t *testing.T) {
	_, mi, cm := newWorktreeTestIndexer(t)
	t.Cleanup(func() { require.NoError(t, mi.Close(context.Background())) })
	owner, other := filepath.Join(t.TempDir(), "owner"), filepath.Join(t.TempDir(), "other")
	for _, root := range []string{owner, other} {
		require.NoError(t, os.MkdirAll(root, 0o755))
	}
	require.NoError(t, cm.Global().AddRepo(config.RepoEntry{Path: owner, Name: "src"}))
	_, err := mi.TrackRepoCtx(t.Context(), config.RepoEntry{Path: other, Name: "src"})
	var collision *PrefixCollisionError
	require.ErrorAs(t, err, &collision, "freshly proved distinct configured ownership is not downgraded to uncertainty")
	require.Equal(t, owner, collision.ExistingPath)
	require.Equal(t, "present", collision.OwnerState)
	require.Empty(t, mi.AllMetadata())
}

func TestPendingCleanupRefusesTrackAliasesBeforeSideEffects(t *testing.T) {
	for _, surface := range []string{"preflight", "register", "raw_track", "raw_reconcile"} {
		for _, alias := range []string{"same_root", "renamed_prefix", "symlink", "reserved_prefix"} {
			t.Run(surface+"/"+alias, func(t *testing.T) {
				// Given a real pending release whose config entry is already gone.
				f := newLifecycleFixture(t)
				t.Cleanup(f.close)
				root := f.gitRepo("closing-track")
				tracked, err := f.lc.Register(t.Context(), config.RepoEntry{Path: root, Name: "held"}, TrackSourceCLI)
				require.NoError(t, err)
				require.NoError(t, tracked.CatalogErr)
				f.lc.EnableDeferredRepositoryCleanup()
				out, err := f.lc.Untrack(t.Context(), root)
				require.NoError(t, err)
				require.True(t, out.Pending)
				require.NoError(t, f.cm.Global().RemoveRepo(root))
				entry := config.RepoEntry{Path: root, Name: tracked.Prefix}
				switch alias {
				case "renamed_prefix":
					entry.Name = "replacement-name"
				case "symlink":
					entry.Path = filepath.Join(t.TempDir(), "renamed-link")
					require.NoError(t, os.Symlink(root, entry.Path))
					entry.Name = "replacement-name"
				case "reserved_prefix":
					entry.Path = f.gitRepo("different-root")
				}
				before, revision := f.cm.Global().ReposSnapshot(), f.cm.Revision()
				configBytes, err := os.ReadFile(f.cfgPath)
				require.NoError(t, err)
				metadata := f.mi.GetMetadata(tracked.Prefix)
				payload := f.store.AllNodes()
				hookCalls, catalogCalls := 0, 0
				f.mi.SetOnRepoTracked(func(string, string) { hookCalls++ })
				f.lc.afterRegisterTrack = func() { catalogCalls++ }
				f.watcher.mu.Lock()
				watcherCalls := len(f.watcher.calls)
				f.watcher.mu.Unlock()

				// When any real or advisory registration door receives the alias.
				switch surface {
				case "preflight":
					err = f.lc.PreflightTrack(t.Context(), entry)
				case "register":
					_, err = f.lc.Register(t.Context(), entry, TrackSourceCLI)
				case "raw_track":
					_, err = f.mi.TrackRepoCtx(t.Context(), entry)
				case "raw_reconcile":
					_, err = f.mi.ReconcileRepoCtx(t.Context(), entry, map[string]int64{"main.go": 1})
				}

				// Then refusal precedes physical indexing and every publication.
				require.ErrorIs(t, err, graphview.ErrRepositoryAdmissionClosed)
				require.Zero(t, hookCalls)
				require.Zero(t, catalogCalls)
				require.Same(t, metadata, f.mi.GetMetadata(tracked.Prefix))
				require.ElementsMatch(t, payload, f.store.AllNodes())
				require.Equal(t, before, f.cm.Global().ReposSnapshot())
				require.Equal(t, revision, f.cm.Revision())
				afterBytes, err := os.ReadFile(f.cfgPath)
				require.NoError(t, err)
				require.Equal(t, configBytes, afterBytes)
				f.watcher.mu.Lock()
				gotWatcherCalls := len(f.watcher.calls)
				f.watcher.mu.Unlock()
				require.Equal(t, watcherCalls, gotWatcherCalls)
				intents, err := f.catalog.ListTrackingIntents(t.Context(), tracked.CheckoutID)
				require.NoError(t, err)
				for _, intent := range intents {
					require.False(t, intent.Active)
				}
			})
		}
	}
}

func TestPendingCleanupAllowsDistinctNestedAndUnrelatedRoots(t *testing.T) {
	// Given a closing parent root held before physical release.
	f := newLifecycleFixture(t)
	t.Cleanup(f.close)
	root := f.gitRepo("closing-parent")
	tracked, err := f.lc.Register(t.Context(), config.RepoEntry{Path: root}, TrackSourceCLI)
	require.NoError(t, err)
	require.NoError(t, tracked.CatalogErr)
	f.lc.EnableDeferredRepositoryCleanup()
	out, err := f.lc.Untrack(t.Context(), root)
	require.NoError(t, err)
	require.True(t, out.Pending)

	// When distinct repositories inside and outside that root register.
	nested := f.gitRepo("nested")
	nestedPath := filepath.Join(root, "nested")
	require.NoError(t, os.Rename(nested, nestedPath))
	for _, path := range []string{nestedPath, f.gitRepo("unrelated")} {
		entry := config.RepoEntry{Path: path, Name: filepath.Base(path)}
		require.NoError(t, f.lc.PreflightTrack(t.Context(), entry))
		registered, err := f.lc.Register(t.Context(), entry, TrackSourceCLI)

		// Then exact identity, not ancestor containment or a global fence,
		// admits their independent corpus and watcher while the parent waits.
		require.NoError(t, err)
		require.NoError(t, registered.CatalogErr)
		require.NotNil(t, registered.Index)
		require.True(t, f.watcher.isAttached(registered.Prefix))
	}
	require.True(t, f.lc.RepositoryAdmissionClosed(tracked.Prefix))
}

func TestPreparedTrackRechecksCleanupRootUnderItsMutationLane(t *testing.T) {
	// Given a prepared alias which predates the cleanup reservation.
	f := newLifecycleFixture(t)
	t.Cleanup(f.close)
	root := f.gitRepo("prepared-closing")
	tracked, err := f.lc.Register(t.Context(), config.RepoEntry{Path: root, Name: "held"}, TrackSourceCLI)
	require.NoError(t, err)
	require.NoError(t, tracked.CatalogErr)
	alias := filepath.Join(t.TempDir(), "prepared-link")
	require.NoError(t, os.Symlink(root, alias))
	require.NoError(t, f.cm.Global().RemoveRepo(root))
	prepared, err := f.mi.prepareTrackRepoContext(t.Context(), config.RepoEntry{Path: alias, Name: "prepared-alias"})
	require.NoError(t, err)
	require.NotNil(t, prepared)
	f.lc.EnableDeferredRepositoryCleanup()
	out, err := f.lc.Untrack(t.Context(), root)
	require.NoError(t, err)
	require.True(t, out.Pending)
	revision := f.cm.Revision()
	payload := f.store.AllNodes()

	// When that stale preparation reaches its distinct prefix's actual lane.
	result, err := f.mi.trackPreparedRepoSourceCtx(t.Context(), prepared, nil)

	// Then preparation is not authority to parse or publish after closure.
	require.ErrorIs(t, err, graphview.ErrRepositoryAdmissionClosed)
	require.Nil(t, result)
	require.Nil(t, f.mi.GetMetadata(prepared.prefix))
	require.ElementsMatch(t, payload, f.store.AllNodes())
	require.Equal(t, revision, f.cm.Revision())
}

func TestRestartRestoresCleanupRootBeforeTrackPreparation(t *testing.T) {
	// Given durable closing identity and a journal, with stale config removed.
	f := newLifecycleFixture(t)
	t.Cleanup(f.close)
	root := f.gitRepo("restarted-closing")
	tracked, err := f.lc.Register(t.Context(), config.RepoEntry{Path: root, Name: "held"}, TrackSourceCLI)
	require.NoError(t, err)
	require.NoError(t, tracked.CatalogErr)
	f.lc.EnableDeferredRepositoryCleanup()
	out, err := f.lc.Untrack(t.Context(), root)
	require.NoError(t, err)
	require.True(t, out.Pending)
	require.NoError(t, f.cm.Global().RemoveRepo(root))
	require.NoError(t, f.cm.Global().Save())
	alias := filepath.Join(t.TempDir(), "restarted-link")
	require.NoError(t, os.Symlink(root, alias))

	// When startup restores it without a live MI metadata entry.
	f.restart()
	f.lc.EnableDeferredRepositoryCleanup()
	require.NoError(t, f.lc.Seed(t.Context()))
	require.Nil(t, f.mi.GetMetadata(tracked.Prefix))
	revision := f.cm.Revision()
	entry := config.RepoEntry{Path: alias, Name: "fresh-name"}

	// Then both advisory and actual raw registration retain the same fence.
	require.ErrorIs(t, f.lc.PreflightTrack(t.Context(), entry), graphview.ErrRepositoryAdmissionClosed)
	_, err = f.mi.TrackRepoCtx(t.Context(), entry)
	require.ErrorIs(t, err, graphview.ErrRepositoryAdmissionClosed)
	require.Nil(t, f.mi.GetMetadata(entry.Name))
	require.Equal(t, revision, f.cm.Revision())
}

type heldTrackCleanupPublisher struct {
	*cleanupReadyObserver
	held      atomic.Bool
	attempted chan struct{}
	once      sync.Once
}

func (p *heldTrackCleanupPublisher) FinalizeDedicatedBaseOwner(graphID string, owner store_sqlite.DedicatedBaseOwner, drained <-chan struct{}) error {
	if p.held.Load() {
		p.once.Do(func() { close(p.attempted) })
		return fmt.Errorf("test publisher finalization is held")
	}
	return p.cleanupReadyObserver.FinalizeDedicatedBaseOwner(graphID, owner, drained)
}

func TestTrackReservationLastsThroughFullCleanupFinalization(t *testing.T) {
	// Given physical purge can finish, but publisher finalization still fails.
	f := newLifecycleFixture(t)
	t.Cleanup(f.close)
	runtime, err := NewDedicatedBaseRuntime(f.store, f.lc.ViewLeases())
	require.NoError(t, err)
	observer := &cleanupReadyObserver{DedicatedBaseRuntime: runtime, finalized: make(chan string, 8)}
	publisher := &heldTrackCleanupPublisher{cleanupReadyObserver: observer, attempted: make(chan struct{})}
	publisher.held.Store(true)
	require.NoError(t, f.lc.SetDedicatedBaseCleanupRuntime(publisher))
	root := f.gitRepo("finalizing-track")
	tracked, err := f.lc.Register(t.Context(), config.RepoEntry{Path: root, Name: "held"}, TrackSourceCLI)
	require.NoError(t, err)
	require.NoError(t, tracked.CatalogErr)
	f.lc.EnableDeferredRepositoryCleanup()
	out, err := f.lc.Untrack(t.Context(), root)
	require.NoError(t, err)
	require.True(t, out.Pending)
	old := drainedCleanupState(t, f.lc, tracked.GraphID)
	f.lc.MarkRepositoryCleanupReady()
	select {
	case <-publisher.attempted:
	case <-time.After(10 * time.Second):
		t.Fatal("cleanup never reached held publisher finalization")
	}
	old.mu.Lock()
	purged, finalized := old.payloadPurged, old.finalized
	old.mu.Unlock()
	require.True(t, purged)
	require.False(t, finalized)
	require.Nil(t, f.mi.existingRepositoryMutationCoordinator(tracked.Prefix), "the earlier MI finalizer already released its lane")

	// When an alias retries after lane deletion but before full finalization.
	alias := filepath.Join(t.TempDir(), "finalizing-link")
	require.NoError(t, os.Symlink(root, alias))
	entry := config.RepoEntry{Path: alias, Name: "fresh-name"}
	require.ErrorIs(t, f.lc.PreflightTrack(t.Context(), entry), graphview.ErrRepositoryAdmissionClosed)
	_, err = f.mi.TrackRepoCtx(t.Context(), entry)
	require.ErrorIs(t, err, graphview.ErrRepositoryAdmissionClosed)
	_, err = f.mi.TrackRepoCtx(t.Context(), config.RepoEntry{Path: root, Name: tracked.Prefix})
	require.ErrorIs(t, err, graphview.ErrRepositoryAdmissionClosed)

	// Then only successful full finalization lets the path register again.
	publisher.held.Store(false)
	f.lc.repositoryCleanup.signal()
	awaitCleanupReadyFinalization(t, observer, old)
	retracked, err := f.lc.Register(t.Context(), config.RepoEntry{Path: root, Name: tracked.Prefix}, TrackSourceCLI)
	require.NoError(t, err)
	require.NoError(t, retracked.CatalogErr)
	require.NotNil(t, retracked.Index)
	pin := f.lc.ViewLeases().AcquireBaseCorpus(retracked.Prefix)
	t.Cleanup(pin.Release)
	out, err = f.lc.Untrack(t.Context(), root)
	require.NoError(t, err)
	require.True(t, out.Pending)
	next := cleanupReadyState(t, f.lc, retracked.GraphID)
	require.NotSame(t, old, next)
	require.NotSame(t, old.owner, next.owner)

	// An old finalization retry has no authority over that replacement fence.
	finished, err := f.lc.finalizeRepositoryCleanupState(t.Context(), old)
	require.NoError(t, err)
	require.True(t, finished)
	require.ErrorIs(t, f.lc.PreflightTrack(t.Context(), entry), graphview.ErrRepositoryAdmissionClosed)
	pin.Release()
	awaitCleanupReadyFinalization(t, observer, next)
}
