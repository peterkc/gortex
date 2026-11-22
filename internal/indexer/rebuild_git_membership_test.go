package indexer

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestRebuildGitMembershipRecords(t *testing.T) {
	oid := strings.Repeat("a", 40)
	tracked := "100644 " + oid + " 0\tsp ace\n\xff.go\x00" + "160000 " + oid + " 0\tsub\x00"
	got, err := parseRebuildGitIndex(t.Context(), []byte(tracked))
	require.NoError(t, err)
	require.Equal(t, map[string]rebuildGitMember{
		"sp ace\n\xff.go": rebuildGitTracked, "sub": rebuildGitBoundary,
	}, got)
	for _, bad := range []string{
		tracked + "100644 " + oid + " 0\tsub\x00",
		tracked + "100644 " + oid + " 0\t../outside.go\x00",
		tracked + "truncated",
		tracked + "\x00",
		strings.Replace(tracked, " 0\t", " 2\t", 1),
		strings.Replace(tracked, "100644", "040000", 1),
	} {
		got, err := parseRebuildGitIndex(t.Context(), []byte(bad))
		require.ErrorIs(t, err, errRebuildGitMembership)
		require.Nil(t, got)
	}
}

func TestRebuildGitInventoryRejectsIndexOverride(t *testing.T) {
	root, err := os.OpenRoot(t.TempDir())
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, root.Close()) })
	t.Setenv("GIT_INDEX_FILE", "alternate-index")
	raw := int64(rebuildCppRawLimit)
	got, err := readRebuildGitInventory(t.Context(), root, &raw)
	require.ErrorIs(t, err, errRebuildGitMembership)
	require.ErrorContains(t, err, "repository selector")
	require.Empty(t, got)
}

func TestRebuildGitInventoryRejectsMovedRoot(t *testing.T) {
	parent := t.TempDir()
	dir, moved := filepath.Join(parent, "checkout"), filepath.Join(parent, "moved")
	require.NoError(t, os.Mkdir(dir, 0o700))
	root, err := os.OpenRoot(dir)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, root.Close()) })
	require.NoError(t, checkRebuildGitWorktreeRoot(root))
	require.NoError(t, os.Rename(dir, moved))
	require.NoError(t, os.Mkdir(dir, 0o700))
	require.ErrorIs(t, checkRebuildGitWorktreeRoot(root), ErrBuildInputChanged)
}
