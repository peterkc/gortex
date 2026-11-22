package indexer

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestRebuildGitMembershipRecords(t *testing.T) {
	oid := strings.Repeat("a", 40)
	tracked := "100644 " + oid + " 0\tsp ace\n\xff.go\x00" + "160000 " + oid + " 0\tsub\x00"
	others := "nonignored.ts\x00embedded/\x00"
	got, err := parseRebuildGitMembership(t.Context(), []byte(tracked), []byte(others))
	require.NoError(t, err)
	require.Equal(t, map[string]rebuildGitMember{
		"sp ace\n\xff.go": rebuildGitTracked, "sub": rebuildGitBoundary,
		"nonignored.ts": rebuildGitUntracked, "embedded": rebuildGitBoundary,
	}, got)
	for _, bad := range []struct{ index, other string }{
		{tracked, others + "sub\x00"},
		{tracked, "../escape.go\x00"},
		{tracked, "truncated"},
		{tracked, "\x00"},
		{strings.Replace(tracked, " 0\t", " 2\t", 1), others},
		{strings.Replace(tracked, "100644", "040000", 1), others},
	} {
		got, err := parseRebuildGitMembership(t.Context(), []byte(bad.index), []byte(bad.other))
		require.ErrorIs(t, err, errRebuildGitMembership)
		require.Nil(t, got)
	}
}
