package indexer

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// These fixtures supply the admission and closure observations. The proof
// loads those rows only after their sidecar digest is checked.
func recordTestRebuildWorkspaceGlob(t *testing.T, rows []rebuildNamespaceRow) func() *rebuildWorkspaceGlob {
	t.Helper()
	lease := t.TempDir()
	left := int64(rebuildCppRawLimit)
	digest, err := writeRebuildInputSidecar(t.Context(), lease, func(emit func(rebuildSidecarRow) error) error {
		for _, row := range rows {
			if err := writeRebuildNamespaceFact(&left, row, emit); err != nil {
				return err
			}
		}
		return nil
	})
	require.NoError(t, err)
	return func() *rebuildWorkspaceGlob {
		t.Helper()
		ns, err := openRebuildResolverNamespace(t.Context(), lease, digest)
		require.NoError(t, err)
		g, err := newRebuildWorkspaceGlob(t.Context(), ns)
		require.NoError(t, err)
		return g
	}
}

func TestRebuildWorkspaceGlobNativeReplay(t *testing.T) {
	rows := []rebuildNamespaceRow{
		{"pkg", rebuildNamespaceDirectory},
		{"pkg/a", rebuildNamespaceDirectory},
		{"pkg/a/leaf", rebuildNamespaceDirectory},
		{"pkg/a", rebuildNamespaceClose},
		{"pkg/a-1", rebuildNamespaceDirectory},
		{"pkg/a-1/leaf", rebuildNamespaceDirectory},
		{"pkg/a-1", rebuildNamespaceClose},
		{"pkg/[tag]", rebuildNamespaceDirectory},
		{"pkg/other", rebuildNamespaceDenied},
		{"pkg/file", rebuildNamespaceRegular},
		{"pkg", rebuildNamespaceCloseExact},
		{".", rebuildNamespaceCloseExact},
	}
	parent := t.TempDir()
	plain := filepath.Join(parent, "checkout")
	meta := filepath.Join(parent, "checkout[ab]")
	for _, root := range []string{plain, meta} {
		for _, dir := range []string{"pkg/a/leaf", "pkg/a-1/leaf", "pkg/[tag]", "pkg/other/dont-read"} {
			require.NoError(t, os.MkdirAll(filepath.Join(root, filepath.FromSlash(dir)), 0o700))
		}
		require.NoError(t, os.WriteFile(filepath.Join(root, "pkg", "file"), nil, 0o600))
	}
	type globCase struct {
		pattern string
		want    []string
	}
	cases := []globCase{
		{"pkg/a*/leaf", []string{"pkg/a-1/leaf", "pkg/a/leaf"}},
		{"./pkg//a[!-~]*/leaf", []string{"pkg/a-1/leaf"}},
		{"pkg/a**/leaf", []string{"pkg/a-1/leaf", "pkg/a/leaf"}},
		{"pkg/a*/le?f", []string{"pkg/a-1/leaf", "pkg/a/leaf"}},
		// exhaustive listed names, not a negative stat
		{"pkg/a*/Leaf", nil},
		{"pkg/a/leaf", []string{"pkg/a/leaf"}},
		{"pkg/[[]tag]", []string{"pkg/[tag]"}},
		// native Join cleans before matching
		{"pkg/other/../missing", nil},
		// do not consult a nonmatching open directory
		{"pkg/no*/*", nil},
		{"pkg/file/child", nil},
		// a matched regular file is filtered out
		{"pkg/f*", nil},
		{".", []string{"."}},
		{"", nil},
		{"pkg/[", nil},
	}
	if filepath.Separator == '/' {
		cases = append(cases, globCase{`pkg/\[tag]`, []string{"pkg/[tag]"}})
	}
	for _, tc := range cases {
		require.Equal(t, tc.want, newDiskManifestTree(plain).matchDirs(tc.pattern), tc.pattern)
	}
	// Pinned native accidentally treats the metacharacters in the checkout
	// name as glob syntax. Frozen answers use the same literal relative keys.
	require.Empty(t, newDiskManifestTree(meta).matchDirs("pkg/a*/leaf"))
	open := recordTestRebuildWorkspaceGlob(t, rows)
	check := func() {
		g := open()
		for _, tc := range cases {
			got, err := g.matchDirs(t.Context(), tc.pattern, rebuildNpmFactBytes)
			require.NoError(t, err, tc.pattern)
			require.Equal(t, tc.want, got, tc.pattern)
		}
	}
	check()
	require.NoError(t, os.RemoveAll(plain))
	require.NoError(t, os.RemoveAll(meta))
	require.Empty(t, newDiskManifestTree(plain).matchDirs("pkg/a*/leaf"))
	check()
}

func TestRebuildWorkspaceGlobRefusals(t *testing.T) {
	root := t.TempDir()
	for _, dir := range []string{"pkg/a-ok/sub", "pkg/b-denied/sub", "pkg/open/seen"} {
		require.NoError(t, os.MkdirAll(filepath.Join(root, filepath.FromSlash(dir)), 0o700))
	}
	require.NoError(t, os.Symlink("a-ok", filepath.Join(root, "pkg", "link")))
	open := recordTestRebuildWorkspaceGlob(t, []rebuildNamespaceRow{
		{"pkg", rebuildNamespaceDirectory},
		{"pkg/a-ok", rebuildNamespaceDirectory},
		{"pkg/a-ok/sub", rebuildNamespaceDirectory},
		{"pkg/a-ok", rebuildNamespaceClose},
		{"pkg/b-denied", rebuildNamespaceDenied},
		{"pkg/open", rebuildNamespaceDirectory},
		{"pkg/open/seen", rebuildNamespaceDirectory},
		{"pkg/link", rebuildNamespaceUnsupported},
		{"pkg", rebuildNamespaceClose},
		{".", rebuildNamespaceCloseExact},
	})
	g := open()
	native := newDiskManifestTree(root)
	cases := []struct {
		pattern string
		want    []string
	}{
		{"pkg/[ab]-*", []string{"pkg/a-ok", "pkg/b-denied"}},
		// plain Close is not enough for a missing literal
		{"pkg/missing/*", nil},
		// suffix literal needs a listing
		{"pkg/o*/seen", []string{"pkg/open/seen"}},
		// matched symlink intermediate
		{"pkg/l*/sub", []string{"pkg/link/sub"}},
		// matched symlink leaf
		{"pkg/l*", []string{"pkg/link"}},
		{"pkg/link/sub", []string{"pkg/link/sub"}},
		{"pkg/link", []string{"pkg/link"}},
	}
	for _, tc := range cases {
		require.Equal(t, tc.want, native.matchDirs(tc.pattern), tc.pattern)
		got, err := g.matchDirs(t.Context(), tc.pattern, rebuildNpmFactBytes)
		require.ErrorIs(t, err, errRebuildResolverNamespace, tc.pattern)
		require.Nil(t, got, tc.pattern)
	}
	got, err := g.matchDirs(t.Context(), "../outside/*", rebuildNpmFactBytes)
	require.ErrorIs(t, err, errRebuildResolverNamespace)
	require.Nil(t, got)
	// The same missing name is proved absent from a listing rather than a
	// literal probe. And a positive literal needs no listing of its parent.
	got, err = g.matchDirs(t.Context(), "pkg/miss*/*", rebuildNpmFactBytes)
	require.NoError(t, err)
	require.Equal(t, native.matchDirs("pkg/miss*/*"), got)
	got, err = g.matchDirs(t.Context(), "pkg/open/seen", rebuildNpmFactBytes)
	require.NoError(t, err)
	require.Equal(t, native.matchDirs("pkg/open/seen"), got)
}

func TestRebuildWorkspaceGlobCaps(t *testing.T) {
	var rows []rebuildNamespaceRow
	for i := range 24 {
		rows = append(rows, rebuildNamespaceRow{fmt.Sprintf("%02d%s", i, strings.Repeat("x", 253)), rebuildNamespaceDirectory})
	}
	rows = append(rows, rebuildNamespaceRow{".", rebuildNamespaceCloseExact})
	g := recordTestRebuildWorkspaceGlob(t, rows)()
	got, err := g.matchDirs(t.Context(), "*", 24*256)
	require.NoError(t, err)
	require.Len(t, got, 24)
	got, err = g.matchDirs(t.Context(), "*", 24*256-1)
	require.ErrorIs(t, err, errRebuildResolverNamespace)
	require.ErrorContains(t, err, "result cap")
	require.Nil(t, got)
	got, err = g.matchDirs(t.Context(), "*"+strings.Repeat("x", 253), rebuildNpmFactBytes)
	require.ErrorIs(t, err, errRebuildResolverNamespace)
	require.ErrorContains(t, err, "work cap")
	require.Nil(t, got)
	got, err = g.matchDirs(t.Context(), strings.Repeat("*", rebuildNpmFactBytes+1), rebuildNpmFactBytes)
	require.ErrorIs(t, err, errRebuildResolverNamespace)
	require.Nil(t, got)
}
