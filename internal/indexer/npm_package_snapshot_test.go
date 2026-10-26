package indexer

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestNpmPackageSnapshotDetectsSameStampContentChanges(t *testing.T) {
	for _, kind := range []string{"root_manifest", "member_manifest", "pnpm_manifest"} {
		t.Run(kind, func(t *testing.T) {
			root := t.TempDir()
			file := filepath.Join(root, "package.json")
			before, after := `{"name":"p","main":"index.ts"}`, `{"name":"q","main":"other.ts"}`
			switch kind {
			case "member_manifest":
				writePackageJSON(t, root, `{"name":"root","workspaces":["packages/*"]}`)
				file = filepath.Join(root, "packages", "lib", "package.json")
			case "pnpm_manifest":
				writePackageJSON(t, root, `{"name":"root"}`)
				writePackageJSON(t, filepath.Join(root, "one", "a"), before)
				writePackageJSON(t, filepath.Join(root, "two", "b"), after)
				file = filepath.Join(root, "pnpm-workspace.yaml")
				before, after = "packages:\n  - 'one/*'\n", "packages:\n  - 'two/*'\n"
			}
			require.Equal(t, len(before), len(after))
			require.NoError(t, os.MkdirAll(filepath.Dir(file), 0o755))
			require.NoError(t, os.WriteFile(file, []byte(before), 0o644))
			frozen := time.Unix(1700000000, 0)
			require.NoError(t, os.Chtimes(file, frozen, frozen))
			initial, err := os.Stat(file)
			require.NoError(t, err)
			mi := &MultiIndexer{repos: map[string]*RepoMetadata{"lib": {RootPath: root}}}
			old := mi.npmPackageLookup()
			require.NotEmpty(t, old("p"))
			require.Empty(t, old("q"))
			// Given equal-size input bytes, when contents change but mtime is
			// restored, a fresh pass must not reuse the previous parsed lookup.
			require.NoError(t, os.WriteFile(file, []byte(after), 0o644))
			require.NoError(t, os.Chtimes(file, initial.ModTime(), initial.ModTime()))
			current, err := os.Stat(file)
			require.NoError(t, err)
			require.Equal(t, initial.Size(), current.Size())
			require.Equal(t, initial.ModTime(), current.ModTime())
			fresh := mi.npmPackageLookup()
			assert.Empty(t, fresh("p"))
			assert.NotEmpty(t, fresh("q"))
			cold := (&MultiIndexer{repos: mi.repos}).npmPackageLookup()
			assert.Equal(t, cold("q"), fresh("q"))
			assert.NotEmpty(t, old("p"), "an active pass retains its captured lookup")
			assert.Empty(t, old("q"))
		})
	}
}

func TestNpmPackageSnapshotDetectsSameStampDirectoryMember(t *testing.T) {
	root := t.TempDir()
	writePackageJSON(t, root, `{"name":"root","workspaces":["packages/*"]}`)
	packages := filepath.Join(root, "packages")
	member := filepath.Join(packages, "member")
	require.NoError(t, os.MkdirAll(packages, 0o755))
	require.NoError(t, os.WriteFile(member, []byte("not a directory"), 0o644))
	frozen := time.Unix(1700000000, 0)
	require.NoError(t, os.Chtimes(packages, frozen, frozen))
	initial, err := os.Stat(packages)
	require.NoError(t, err)
	mi := &MultiIndexer{repos: map[string]*RepoMetadata{"lib": {RootPath: root}}}
	old := mi.npmPackageLookup()
	require.Contains(t, old("root"), "lib")
	require.Empty(t, old("q"))
	// Given an ignored file at a glob match, when it becomes a package
	// directory with the same parent size/mtime, membership must invalidate.
	require.NoError(t, os.Remove(member))
	writePackageJSON(t, member, `{"name":"q","main":"index.ts"}`)
	require.NoError(t, os.Chtimes(packages, initial.ModTime(), initial.ModTime()))
	current, err := os.Stat(packages)
	require.NoError(t, err)
	require.Equal(t, initial.Size(), current.Size())
	require.Equal(t, initial.ModTime(), current.ModTime())
	fresh := mi.npmPackageLookup()
	require.Contains(t, fresh("q"), "lib/packages/member")
	cold := (&MultiIndexer{repos: mi.repos}).npmPackageLookup()
	assert.Equal(t, cold("q"), fresh("q"))
	assert.Empty(t, old("q"), "the old pass retains its original member set")
}

func TestNpmPackageSnapshotRejectsFailedStamp(t *testing.T) {
	// Two failed reads must not be treated as equal, valid input versions.
	const invalidPath = "\x00"
	snapshot := npmPackageSnapshot{files: map[string]npmPackageFileStamp{invalidPath: npmPackageStamp(invalidPath)}}
	require.False(t, snapshot.unchanged())
}

func TestNpmPackageSnapshotWatchesOnlyLiteralPaths(t *testing.T) {
	for _, mode := range []string{"npm", "pnpm"} {
		t.Run(mode, func(t *testing.T) {
			root := t.TempDir()
			if mode == "npm" {
				writePackageJSON(t, root, `{"name":"root","workspaces":["packages/*","apps/*/web"]}`)
			} else {
				writePackageJSON(t, root, `{"name":"root"}`)
				writeWorkspaceFile(t, root, "pnpm-workspace.yaml", "packages:\n  - 'packages/*'\n  - 'apps/*/web'\n")
			}
			writePackageJSON(t, filepath.Join(root, "packages", "a"), `{"name":"p","main":"index.ts"}`)
			writePackageJSON(t, filepath.Join(root, "apps", "x", "web"), `{"name":"w","main":"index.ts"}`)
			mi := &MultiIndexer{repos: map[string]*RepoMetadata{"lib": {RootPath: root}}}
			first := mi.npmPackageLookup()
			require.Contains(t, first("w"), "lib/apps/x/web")
			// Given glob workspace patterns, when the snapshot is built, then it
			// stamps real paths only: Windows rejects "*" in a path, and an
			// invalid stamp would rebuild the lookup on every unchanged pass.
			cached := mi.npmPackageCache
			require.NotNil(t, cached)
			for file, stamp := range cached.files {
				assert.False(t, strings.ContainsAny(filepath.ToSlash(strings.TrimPrefix(file, root)), "*?["), "glob watched as a path: %s", file)
				assert.True(t, stamp.valid, "invalid stamp: %s", file)
			}
			mi.npmPackageLookup()
			assert.Same(t, cached, mi.npmPackageCache, "an unchanged pass reuses the snapshot")
			// When a new member appears under a glob level, then the parent
			// listings still invalidate the snapshot and match a cold lookup.
			writePackageJSON(t, filepath.Join(root, "apps", "y", "web"), `{"name":"v","main":"index.ts"}`)
			fresh := mi.npmPackageLookup()
			require.Contains(t, fresh("v"), "lib/apps/y/web")
			cold := (&MultiIndexer{repos: mi.repos}).npmPackageLookup()
			assert.Equal(t, cold("v"), fresh("v"))
			writePackageJSON(t, filepath.Join(root, "packages", "b"), `{"name":"q","main":"index.ts"}`)
			require.Contains(t, mi.npmPackageLookup()("q"), "lib/packages/b")
		})
	}
}
