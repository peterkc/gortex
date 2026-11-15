package indexer

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	"github.com/zzet/gortex/internal/config"
)

func TestWatcherManualScanDirectoryPolicy(t *testing.T) {
	// Given real gitignore rules and a Gortex configuration exclusion.
	root := t.TempDir()
	require.NoError(t, os.Mkdir(filepath.Join(root, ".git"), 0o755))
	writeFile(t, filepath.Join(root, ".gitignore"), "tmp/\n*.log\nclosed/\n!closed/keep.go\nopen/*\n!open/keep/\n")
	cm := newTestConfigManager(t)
	cm.Global().Exclude = []string{"configured/"}
	cm.LoadWorkspaceConfig("app", root)
	w, err := NewWatcher(&Indexer{rootPath: root}, cm.GetRepoConfig("app").Watch, zap.NewNop())
	require.NoError(t, err)
	filter := &watcherExcludeFilter{w: w}

	for _, tc := range []struct {
		dir  string
		skip bool
	}{
		{"tmp", true},
		{"configured", true},
		{"x.log", true}, // Existing matcher treats this pattern as a directory exclusion too.
		{"closed", true},
		{"open", false},
		{"open/keep", false},
		{"src", false},
		{".git", false}, // Startup probes live directly inside .git.
		{".git/objects", true},
		{"sub/.git", false},
		{probeMarker + "directory", false},
		{".", false},
		{".claude", false},
		{".kiro/settings", false},
		{".claude/worktrees", true},
	} {
		t.Run(tc.dir, func(t *testing.T) {
			dir := filepath.Join(root, tc.dir)
			require.NoError(t, os.MkdirAll(dir, 0o755))
			// When the rescan asks about a directory, including a trailing slash.
			for _, path := range []string{dir, dir + string(filepath.Separator)} {
				// Then directory-only patterns prune without filesystem type guesses.
				require.Equal(t, tc.skip, filter.ShouldSkipDir(path))
			}
		})
	}
	// A negation cannot restore a child of an excluded parent under today's policy.
	require.False(t, filter.ShouldInclude(filepath.Join(root, "closed/keep.go")))
	require.False(t, filter.ShouldInclude(filepath.Join(root, "x.log/keep.go")))
}

func TestWatcherManualScanPreservesForceIncludes(t *testing.T) {
	// Given an explicit include that opens an otherwise excluded ancestor.
	root := t.TempDir()
	w, err := NewWatcher(&Indexer{rootPath: root}, config.WatchConfig{
		Exclude: []string{"generated/"}, Include: []string{"generated/deep/keep.go"},
	}, zap.NewNop())
	require.NoError(t, err)
	filter := &watcherExcludeFilter{w: w}

	// When the scan reaches the ancestors of the included file.
	for _, rel := range []string{"generated", "generated/deep"} {
		// Then it must descend to the file even though the parent matches an exclude.
		require.False(t, filter.ShouldSkipDir(filepath.Join(root, rel)))
	}
	require.True(t, filter.ShouldInclude(filepath.Join(root, "generated/deep/keep.go")))
}

func TestWatcherManualScanMissingPolicyKeepsDirectories(t *testing.T) {
	// Given filters without a usable matcher or a relative repository root.
	for _, filter := range []*watcherExcludeFilter{
		nil,
		{},
		{w: &Watcher{}},
	} {
		// When the backend asks whether a subtree can be pruned.
		// Then absent policy must preserve the existing full walk.
		require.False(t, filter.ShouldSkipDir("ignored"))
	}
	rootless, err := NewWatcher(&Indexer{}, config.WatchConfig{Exclude: []string{"tmp/"}}, zap.NewNop())
	require.NoError(t, err)
	require.True(t, (&watcherExcludeFilter{w: rootless}).ShouldSkipDir("tmp/"))
	rootless.indexer.rootPath = "relative"
	require.False(t, (&watcherExcludeFilter{w: rootless}).ShouldSkipDir(filepath.Join(t.TempDir(), "tmp")))
}
