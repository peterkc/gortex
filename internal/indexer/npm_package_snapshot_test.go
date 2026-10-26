package indexer

import (
	"os"
	"path/filepath"
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
