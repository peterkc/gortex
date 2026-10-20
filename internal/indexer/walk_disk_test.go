package indexer

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
	"go.uber.org/zap/zaptest/observer"

	"github.com/zzet/gortex/internal/excludes"
)

func TestWalkDisk_PrunesDirectories(t *testing.T) {
	root := t.TempDir()
	for _, path := range []string{
		"gen/drop.go", "gen/keep.go", "plain/a.go", "vendor/x.go",
		"sub/drop/x.go", "sub/keep.go",
	} {
		writeExcludeFixture(t, filepath.Join(root, filepath.FromSlash(path)), "package sample\n")
	}

	for _, tc := range []struct {
		name  string
		scope string
		want  []string
	}{
		{
			name:  "root",
			scope: ".",
			want: []string{
				".", "gen", "gen/drop.go", "gen/keep.go", "plain", "plain/a.go", "sub", "sub/keep.go",
			},
		},
		{
			name:  "below_root",
			scope: "sub",
			want:  []string{"sub", "sub/keep.go"},
		},
		{
			name:  "pruned_scope",
			scope: "vendor",
			want:  nil,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			idx := newAdmissionTestIndexer(t, root, "gen/", "!gen/keep.go", "sub/drop/")
			var visited []string
			err := idx.walkDisk(root, filepath.Join(root, tc.scope), func(path string, d fs.DirEntry, err error) error {
				require.NoError(t, err)
				require.NotNil(t, d)
				rel, relErr := filepath.Rel(root, path)
				require.NoError(t, relErr)
				visited = append(visited, filepath.ToSlash(rel))
				return nil
			})
			require.NoError(t, err)
			require.Equal(t, tc.want, visited)
		})
	}
}

func TestWalkDisk_SkipsGitFolders(t *testing.T) {
	root := t.TempDir()
	for _, path := range []string{
		".git/HEAD", ".git/objects/ab/cdef", "a.go", "sub/.git/config", "sub/b.go", "up/.GIT/config",
	} {
		writeExcludeFixture(t, filepath.Join(root, filepath.FromSlash(path)), "package sample\n")
	}
	// A linked worktree's .git is a file that points at the repository; it is not skipped.
	writeExcludeFixture(t, filepath.Join(root, "wt", ".git"), "gitdir: ../.git/worktrees/wt\n")

	for _, tc := range []struct {
		name  string
		scope string
		want  []string
	}{
		{name: "root", scope: ".", want: []string{".", "a.go", "sub", "sub/b.go", "up", "wt", "wt/.git"}},
		{name: "below_root", scope: "sub", want: []string{"sub", "sub/b.go"}},
		{name: "git_scope", scope: ".git", want: nil},
		{name: "inside_git", scope: ".git/objects", want: nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			// A wildcard-led negation keeps every ignored folder open for the
			// prune check, so only the .git rule keeps the walk out of .git/.
			idx := newAdmissionTestIndexer(t, root, "!*.xcodeproj/x")
			var visited []string
			err := idx.walkDisk(root, filepath.Join(root, tc.scope), func(path string, d fs.DirEntry, err error) error {
				require.NoError(t, err)
				rel, relErr := filepath.Rel(root, path)
				require.NoError(t, relErr)
				visited = append(visited, filepath.ToSlash(rel))
				return nil
			})
			require.NoError(t, err)
			require.Equal(t, tc.want, visited)
		})
	}
}

func TestWalkDisk_CallbackResult(t *testing.T) {
	root := t.TempDir()
	writeExcludeFixture(t, filepath.Join(root, "a", "child.go"), "package sample\n")
	writeFile(t, filepath.Join(root, "b.go"), "package sample\n")
	callbackErr := errors.New("callback failed")

	for _, tc := range []struct {
		name    string
		result  error
		want    []string
		wantErr error
	}{
		{"SkipDir", filepath.SkipDir, []string{".", "a", "b.go"}, nil},
		{"SkipAll", filepath.SkipAll, []string{".", "a"}, nil},
		{"custom_error", callbackErr, []string{".", "a"}, callbackErr},
	} {
		t.Run(tc.name, func(t *testing.T) {
			idx := newAdmissionTestIndexer(t, root)
			var visited []string
			err := idx.walkDisk(root, root, func(path string, d fs.DirEntry, err error) error {
				require.NoError(t, err)
				require.NotNil(t, d)
				rel, relErr := filepath.Rel(root, path)
				require.NoError(t, relErr)
				visited = append(visited, filepath.ToSlash(rel))
				if rel == "a" {
					return tc.result
				}
				return nil
			})
			if tc.wantErr != nil {
				require.Same(t, tc.wantErr, err)
			} else {
				require.NoError(t, err)
			}
			require.Equal(t, tc.want, visited)
		})
	}
}

func TestWalkDisk_MissingScope(t *testing.T) {
	root := t.TempDir()
	scope := filepath.Join(root, "missing")
	idx := newAdmissionTestIndexer(t, root)
	calls := 0
	var walkErr error
	err := idx.walkDisk(root, scope, func(path string, d fs.DirEntry, err error) error {
		calls++
		require.Equal(t, scope, path)
		require.Nil(t, d)
		require.ErrorIs(t, err, fs.ErrNotExist)
		walkErr = err
		return err
	})
	require.Equal(t, 1, calls)
	require.Same(t, walkErr, err)
}

func TestWalkDisk_DirectoryReadError(t *testing.T) {
	if runtime.GOOS == "windows" || os.Geteuid() == 0 {
		t.Skip("directory permissions do not force a read error on Windows or as root")
	}
	root := t.TempDir()
	locked := filepath.Join(root, "locked")
	writeExcludeFixture(t, filepath.Join(locked, "child.go"), "package sample\n")
	require.NoError(t, os.Chmod(locked, 0o000))
	t.Cleanup(func() { require.NoError(t, os.Chmod(locked, 0o755)) })

	idx := newAdmissionTestIndexer(t, root)
	callbackErr := errors.New("directory read failed")
	var visited []string
	err := idx.walkDisk(root, root, func(path string, d fs.DirEntry, err error) error {
		require.NotNil(t, d)
		rel, relErr := filepath.Rel(root, path)
		require.NoError(t, relErr)
		if err != nil {
			require.Equal(t, locked, path)
			require.True(t, d.IsDir())
			require.ErrorIs(t, err, fs.ErrPermission)
			visited = append(visited, filepath.ToSlash(rel)+":error")
			return callbackErr
		}
		visited = append(visited, filepath.ToSlash(rel))
		return nil
	})
	require.Same(t, callbackErr, err)
	require.Equal(t, []string{".", "locked", "locked:error"}, visited)
}

// A re-include under .git no longer reaches the walk, so each one is
// reported when the ignore list is compiled.
func TestExcludeMatcher_WarnsAboutGitReincludes(t *testing.T) {
	core, logs := observer.New(zap.WarnLevel)
	idx := newAdmissionTestIndexer(t, t.TempDir())
	idx.logger = zap.New(core)
	idx.SetExcludePatterns(append(append([]string{}, excludes.Builtin...),
		"!.git/hooks/", "!.gitkeep", "!/.GIT/info/", "!**/.git/config"))

	var warned []string
	for _, entry := range logs.All() {
		warned = append(warned, entry.ContextMap()["pattern"].(string))
	}
	assert.Equal(t, []string{"!.git/hooks/", "!/.GIT/info/", "!**/.git/config"}, warned)
}
