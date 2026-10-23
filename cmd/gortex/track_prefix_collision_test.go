package main

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/spf13/cobra"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/zzet/gortex/internal/config"
	"github.com/zzet/gortex/internal/indexer"
	"github.com/zzet/gortex/internal/pathkey"
)

func TestRunTrackPrefixCollision(t *testing.T) {
	for _, wait := range []bool{false, true} {
		for _, preexisting := range []bool{false, true} {
			t.Run(fmt.Sprintf("wait=%v/preexisting=%v", wait, preexisting), func(t *testing.T) {
				t.Setenv("XDG_CONFIG_HOME", t.TempDir())
				t.Setenv("GORTEX_DAEMON_SOCKET", filepath.Join(t.TempDir(), "absent.sock"))
				first, second := pathkey.CanonicalExistingRoot(t.TempDir()), pathkey.CanonicalExistingRoot(t.TempDir())
				gc, err := config.LoadGlobal()
				require.NoError(t, err)
				require.NoError(t, gc.AddRepo(config.RepoEntry{Path: first, Name: "src"}))
				if preexisting {
					require.NoError(t, gc.AddRepo(config.RepoEntry{Path: second, Name: "src"}))
				}
				require.NoError(t, gc.Save())
				origWait, origName, origAsWorktree := trackWait, trackName, trackAsWorktree
				origEnsure, origNotify := trackEnsureDaemonReadyFn, trackNotifyDaemonTrackFn
				trackWait, trackName, trackAsWorktree = wait, "src", false
				trackEnsureDaemonReadyFn = func(bool) daemonDecision { return daemonReady }
				trackNotifyDaemonTrackFn = func(string, time.Time) error {
					return &trackPrefixCollisionError{message: (&indexer.PrefixCollisionError{
						Prefix: "src", ExistingPath: first, Path: second,
					}).Error()}
				}
				t.Cleanup(func() {
					trackWait, trackName, trackAsWorktree = origWait, origName, origAsWorktree
					trackEnsureDaemonReadyFn, trackNotifyDaemonTrackFn = origEnsure, origNotify
				})
				cmd := &cobra.Command{}
				output := &bytes.Buffer{}
				cmd.SetErr(output)
				err = runTrack(cmd, []string{second})
				require.Error(t, err, "a rejected track must return non-zero even without --wait")
				assert.Contains(t, err.Error(), first)
				assert.Contains(t, err.Error(), "gortex track --name <other>")
				assert.NotContains(t, output.String(), "Tracked repository")
				gc, err = config.LoadGlobal()
				require.NoError(t, err)
				want := 1
				if preexisting {
					want = 2
				}
				assert.Len(t, gc.Repos, want, "only a newly-added config entry may be removed")
				assert.Equal(t, first, gc.Repos[0].Path)
			})
		}
	}
}

// The rollback removes only the entry this run added, never another
// spelling of the same directory that was already in config.
func TestRunTrackPrefixCollisionKeepsAliasEntry(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv("GORTEX_DAEMON_SOCKET", filepath.Join(t.TempDir(), "absent.sock"))
	first, second := pathkey.CanonicalExistingRoot(t.TempDir()), pathkey.CanonicalExistingRoot(t.TempDir())
	alias := filepath.Join(t.TempDir(), "alias")
	if err := os.Symlink(second, alias); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	gc, err := config.LoadGlobal()
	require.NoError(t, err)
	require.NoError(t, gc.AddRepo(config.RepoEntry{Path: first, Name: "src"}))
	require.NoError(t, gc.AddRepo(config.RepoEntry{Path: alias, Name: "kept"}))
	require.NoError(t, gc.Save())
	origWait, origName, origAsWorktree := trackWait, trackName, trackAsWorktree
	origEnsure, origNotify := trackEnsureDaemonReadyFn, trackNotifyDaemonTrackFn
	trackWait, trackName, trackAsWorktree = false, "src", false
	trackEnsureDaemonReadyFn = func(bool) daemonDecision { return daemonReady }
	trackNotifyDaemonTrackFn = func(string, time.Time) error {
		return &trackPrefixCollisionError{message: "prefix collision"}
	}
	t.Cleanup(func() {
		trackWait, trackName, trackAsWorktree = origWait, origName, origAsWorktree
		trackEnsureDaemonReadyFn, trackNotifyDaemonTrackFn = origEnsure, origNotify
	})
	cmd := &cobra.Command{}
	cmd.SetErr(&bytes.Buffer{})
	require.Error(t, runTrack(cmd, []string{second}))
	gc, err = config.LoadGlobal()
	require.NoError(t, err)
	var paths []string
	for _, entry := range gc.Repos {
		paths = append(paths, entry.Path)
	}
	assert.Equal(t, []string{first, alias}, paths)
}

func TestNotifyDaemonTrackPrefixCollision(t *testing.T) {
	_, _, first := spinUpDaemonWithConfig(t)
	second := filepath.Join(t.TempDir(), filepath.Base(first))
	require.NoError(t, os.MkdirAll(second, 0o755))
	origName := trackName
	trackName = ""
	t.Cleanup(func() { trackName = origName })
	err := notifyDaemonTrack(second, time.Time{})
	var collision *trackPrefixCollisionError
	require.ErrorAs(t, err, &collision)
	assert.Equal(t, "track rejected: "+(&indexer.PrefixCollisionError{
		Prefix: filepath.Base(first), ExistingPath: first, Path: second,
	}).Error(), err.Error())
}
