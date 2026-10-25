package indexer

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Given a cached npm/pnpm workspace, when new resolver passes retry imports,
// then unchanged manifests cost no reads; manifest, member and config changes
// rebuild once and produce the same targets as a cold lookup.
func TestNpmPackageLookupCachesManifestWork(t *testing.T) {
	for _, mode := range []string{"npm", "pnpm"} {
		t.Run(mode, func(t *testing.T) {
			root := t.TempDir()
			manifest := `{"name":"root","workspaces":["packages/*"]}`
			if mode == "pnpm" {
				manifest = `{"name":"root"}`
				writeWorkspaceFile(t, root, "pnpm-workspace.yaml", "packages:\n  - 'packages/*'\n")
			}
			writePackageJSON(t, root, manifest)
			writePackageJSON(t, filepath.Join(root, "packages", "a"), `{"name":"p","main":"index.ts"}`)
			require.NoError(t, os.MkdirAll(filepath.Join(root, "packages", "empty"), 0o755))
			cm := newTestConfigManager(t)
			mi := &MultiIndexer{configMgr: cm, repos: map[string]*RepoMetadata{"lib": {RootPath: root}}}
			original := readDiskFile
			reads := 0
			readDiskFile = func(file string) ([]byte, bool) { reads++; return original(file) }
			t.Cleanup(func() { readDiskFile = original })
			first := mi.npmPackageLookup()
			require.Contains(t, first("p"), "lib/packages/a")
			require.Positive(t, reads)
			reads = 0
			for range 3 {
				assert.Equal(t, first("p"), mi.npmPackageLookup()("p"))
			}
			assert.Zero(t, reads, "unchanged resolver passes must reuse manifest/workspace evidence")
			writePackageJSON(t, filepath.Join(root, "packages", "a"), `{"name":"renamed","main":"other.ts"}`)
			fresh := mi.npmPackageLookup()
			assert.Empty(t, fresh("p"))
			require.Contains(t, fresh("renamed"), "lib/packages/a")
			require.Positive(t, reads, "a member-manifest edit must rebuild")
			assert.Contains(t, first("p"), "lib/packages/a", "an active pass keeps immutable evidence")
			for _, dir := range []string{"empty", "new"} {
				reads = 0
				writePackageJSON(t, filepath.Join(root, "packages", dir), `{"name":"added","main":"index.ts"}`)
				lookup := mi.npmPackageLookup()
				cold := (&MultiIndexer{configMgr: cm, repos: mi.repos}).npmPackageLookup()
				assert.Equal(t, cold("added"), lookup("added"), "membership changes must match cold")
				assert.Contains(t, lookup("added"), "lib/packages/"+dir)
				require.Positive(t, reads)
			}
			reads = 0
			cm.LoadWorkspaceConfig("lib", root)
			require.NotEmpty(t, mi.npmPackageLookup()("added"))
			require.Positive(t, reads, "config revisions rebuild workspace placement")
			mi.repos = map[string]*RepoMetadata{}
			assert.Nil(t, mi.npmPackageLookup(), "removed roots cannot retain claims")
		})
	}
}
