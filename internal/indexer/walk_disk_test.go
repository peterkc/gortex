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

	"github.com/zzet/gortex/internal/config"
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
				".", "plain", "plain/a.go", "sub", "sub/keep.go",
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
			// A wildcard-led negation must not open Git metadata folders.
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
		"!.git/hooks/", "!.gitkeep", "!/.GIT/info/", "!**/.git/config", "!.git/hooks/"))

	var warned []string
	for _, entry := range logs.All() {
		warned = append(warned, entry.ContextMap()["pattern"].(string))
	}
	assert.Equal(t, []string{"!.git/hooks/", "!/.GIT/info/", "!**/.git/config"}, warned)
}

func TestExcludeMatcher_WarnsAboutBlockedNegations(t *testing.T) {
	for _, tc := range []struct {
		name                   string
		patterns               []string
		warned, parents, rules []string
	}{
		{"closed_agent", []string{"*", "!/agent/skills/**"}, []string{"!/agent/skills/**"}, []string{"agent"}, []string{"*"}},
		{"closed_dependency", []string{"node_modules/", "!node_modules/@org/**"}, []string{"!node_modules/@org/**"}, []string{"node_modules"}, []string{"node_modules/"}},
		{"unrooted_under_root_only_exclude", []string{"/foo/", "!foo/keep.go"}, nil, nil, nil},
		{"rooted_under_root_only_exclude", []string{"/foo/", "!/foo/keep.go"}, []string{"!/foo/keep.go"}, []string{"foo"}, []string{"/foo/"}},
		{"basename_proto", []string{"node_modules/", "!*.proto"}, nil, nil, nil},
		{"basename_gitkeep", []string{"node_modules/", "!.gitkeep"}, nil, nil, nil},
		{"wildcard_leading", []string{"node_modules/", "!**/@org/**"}, nil, nil, nil},
		{"reopen_self", []string{"build/", "!build/"}, nil, nil, nil},
		{"open_child", []string{"dist/*", "!dist/keep/"}, nil, nil, nil},
		{"reopen_every_parent", []string{"/*", "!/agent/", "/agent/*", "!/agent/skills/"}, nil, nil, nil},
		{"later_parent_reopened", []string{"*", "!/agent/skills/**", "!/agent/"}, nil, nil, nil},
		{"later_parent_closed", []string{"*", "!/agent/", "!/agent/skills/**", "agent/"}, []string{"!/agent/skills/**"}, []string{"agent"}, []string{"agent/"}},
		{"deeper_parent", []string{"agent/*", "!/agent/skills/deep/**"}, []string{"!/agent/skills/deep/**"}, []string{"agent/skills"}, []string{"agent/*"}},
		{"duplicate", []string{"*", "!/agent/skills/**", "!/agent/skills/**"}, []string{"!/agent/skills/**"}, []string{"agent"}, []string{"*"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			core, logs := observer.New(zap.WarnLevel)
			idx := &Indexer{config: config.IndexConfig{Exclude: tc.patterns}, logger: zap.New(core)}
			// One lazy build, no repeated per-path warnings.
			idx.excludeMatcher()
			idx.excludeMatcher()
			var warned, parents, rules []string
			for _, entry := range logs.All() {
				warned = append(warned, entry.ContextMap()["pattern"].(string))
				parents = append(parents, entry.ContextMap()["parent"].(string))
				rules = append(rules, entry.ContextMap()["excluded_by"].(string))
				require.Contains(t, entry.Message, "reopen each parent folder")
				require.Contains(t, entry.Message, "!/agent/")
				require.Contains(t, entry.Message, "/agent/*")
				require.Contains(t, entry.Message, "!/agent/skills/")
			}
			require.Equal(t, tc.warned, warned)
			require.Equal(t, tc.parents, parents)
			require.Equal(t, tc.rules, rules)
			// A reload reports each blocked pattern once for the new effective list.
			idx.SetExcludePatterns(tc.patterns)
			require.Equal(t, 2*len(tc.warned), logs.Len())
		})
	}
}
