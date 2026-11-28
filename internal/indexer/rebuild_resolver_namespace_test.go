package indexer

import (
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestRebuildResolverNamespaceReplay(t *testing.T) {
	root := t.TempDir()
	empty := filepath.Join(root, "empty")
	ignored := filepath.Join(root, "ignored")
	file := filepath.Join(root, "index.ts")
	missing := filepath.Join(empty, "later.ts")
	require.NoError(t, os.Mkdir(empty, 0o700))
	require.NoError(t, os.Mkdir(ignored, 0o700))
	require.NoError(t, os.WriteFile(filepath.Join(ignored, "secret.ts"), nil, 0o600))
	require.NoError(t, os.WriteFile(file, nil, 0o600))
	children, err := os.ReadDir(empty)
	require.NoError(t, err)
	require.Empty(t, children)
	_, err = os.Lstat(missing)
	require.ErrorIs(t, err, fs.ErrNotExist)

	// The fixture supplies checked observations and exact closure certificates;
	// the reducer does not enumerate or certify the filesystem.
	rows := []rebuildNamespaceRow{
		{"empty", rebuildNamespaceDirectory},
		{"ignored", rebuildNamespaceDenied},
		{"index.ts", rebuildNamespaceRegular},
		{".", rebuildNamespaceCloseExact},
		{"empty", rebuildNamespaceCloseExact},
	}
	check := func(n *rebuildResolverNamespace) {
		t.Helper()
		for _, want := range []rebuildNamespaceRow{
			{"empty", rebuildNamespaceDirectory},
			{"index.ts", rebuildNamespaceRegular},
			{"absent.ts", rebuildNamespaceAbsent},
			{"empty/later.ts", rebuildNamespaceAbsent},
		} {
			kind, err := n.lookup(want.key)
			require.NoError(t, err, "%s", want.key)
			require.Equal(t, want.kind, kind, "%s", want.key)
		}
		for _, key := range []string{"ignored", "ignored/secret.ts", "ignored/unseen.ts"} {
			kind, err := n.lookup(key)
			require.ErrorIs(t, err, errRebuildResolverNamespace, "%s", key)
			require.ErrorIs(t, err, errRebuildResolverInputDenied, "%s", key)
			require.Equal(t, rebuildNamespaceUnknown, kind, "%s", key)
			require.ErrorContains(t, err, key)
		}
	}
	var captured rebuildResolverNamespace
	var recorded []rebuildNamespaceRow
	for _, row := range rows {
		require.NoError(t, captured.add(row))
		recorded = append(recorded, row)
	}
	require.NoError(t, captured.freeze())
	check(&captured)

	// Make live answers differ in both directions, then replay only the rows.
	require.NoError(t, os.Remove(file))
	require.NoError(t, os.Remove(empty))
	require.NoError(t, os.Mkdir(empty, 0o700))
	require.NoError(t, os.WriteFile(missing, nil, 0o600))
	_, err = os.Lstat(missing)
	require.NoError(t, err)
	var replayed rebuildResolverNamespace
	for _, row := range recorded {
		require.NoError(t, replayed.add(row))
	}
	require.NoError(t, replayed.freeze())
	check(&replayed)
}

func TestRebuildResolverNamespaceLookupRefusals(t *testing.T) {
	var n rebuildResolverNamespace
	for _, row := range []rebuildNamespaceRow{
		{"exact", rebuildNamespaceDirectory},
		{"exact/Foo.ts", rebuildNamespaceRegular},
		{"exact/caf\u00e9.ts", rebuildNamespaceRegular},
		{"open", rebuildNamespaceDirectory},
		{"aliases", rebuildNamespaceDirectory},
		{"aliases/Seen.ts", rebuildNamespaceRegular},
		{"aliases/byte-\xff.ts", rebuildNamespaceRegular},
		{"regular", rebuildNamespaceRegular},
		{"link", rebuildNamespaceUnsupported},
		{"fifo", rebuildNamespaceUnsupported},
		{"exact", rebuildNamespaceCloseExact},
		{"aliases", rebuildNamespaceClose},
		{".", rebuildNamespaceCloseExact},
	} {
		require.NoError(t, n.add(row))
	}
	_, err := n.lookup("exact/no")
	require.ErrorIs(t, err, errRebuildResolverNamespace)
	require.NoError(t, n.freeze())
	for _, key := range []string{
		"open/no", "aliases/seen.ts", "missing/child", "link", "link/child", "fifo",
		"regular/child", "exact/", "regular/", "regular/../exact", "../exact",
		"exact//no", "./exact/no", "exact/./no", "/exact/no", `C:\exact\no`, "C:/exact/no", "exact/\x00",
	} {
		kind, err := n.lookup(key)
		require.ErrorIs(t, err, errRebuildResolverNamespace, "%s", key)
		require.NotErrorIs(t, err, errRebuildResolverInputDenied, "%s", key)
		require.Equal(t, rebuildNamespaceUnknown, kind, "%s", key)
	}
	for _, key := range []string{"exact", "open", "aliases"} {
		kind, err := n.lookup(key)
		require.NoError(t, err)
		require.Equal(t, rebuildNamespaceDirectory, kind)
	}
	for _, key := range []string{"aliases/Seen.ts", "aliases/byte-\xff.ts", "exact/Foo.ts", "exact/caf\u00e9.ts"} {
		kind, err := n.lookup(key)
		require.NoError(t, err)
		require.Equal(t, rebuildNamespaceRegular, kind)
	}
	require.ErrorIs(t, n.add(rebuildNamespaceRow{"exact/no", rebuildNamespaceRegular}), errRebuildResolverNamespace)
	for _, key := range []string{"exact/no", "exact/foo.ts", "exact/cafe\u0301.ts"} {
		kind, err := n.lookup(key)
		require.NoError(t, err, "%s", key)
		require.Equal(t, rebuildNamespaceAbsent, kind, "%s", key)
	}
}

func TestRebuildResolverNamespaceFileProbe(t *testing.T) {
	var n rebuildResolverNamespace
	for _, row := range []rebuildNamespaceRow{
		{"exact", rebuildNamespaceDirectory},
		{"exact/Foo.ts", rebuildNamespaceRegular},
		{"exact/opaque", rebuildNamespaceDenied},
		{"exact/link", rebuildNamespaceUnsupported},
		{"exact", rebuildNamespaceCloseExact},
		{"open", rebuildNamespaceDirectory},
		{".", rebuildNamespaceCloseExact},
	} {
		require.NoError(t, n.add(row))
	}
	_, err := n.probeFile("exact/Foo.ts")
	require.ErrorIs(t, err, errRebuildResolverNamespace)
	require.NoError(t, n.freeze())
	for key, want := range map[string]bool{
		"exact/Foo.ts": true, "exact/foo.ts": false, "exact/Foo.ts/child.ts": false,
		"absent/parent/index.ts": false, "exact": false, ".": false,
	} {
		got, err := n.probeFile(key)
		require.NoError(t, err, "%q", key)
		require.Equal(t, want, got, "%q", key)
	}
	for _, key := range []string{"exact/opaque/leaf.ts", "exact/link/leaf.ts", "open/no.ts", "open/../no.ts"} {
		got, err := n.probeFile(key)
		require.False(t, got)
		require.ErrorIs(t, err, errRebuildResolverNamespace, "%q", key)
		require.ErrorContains(t, err, key)
		require.Contains(t, buildRefusal(err).Detail, key)
		if key == "exact/opaque/leaf.ts" {
			require.ErrorIs(t, err, errRebuildResolverInputDenied)
		} else {
			require.NotErrorIs(t, err, errRebuildResolverInputDenied)
		}
	}
	// Held-out paths cannot turn denied or unproved namespace answers into misses.
	census := &rebuildExtractorCensus{admitted: 3, byPath: map[string]string{
		"exact/Foo.ts": "python", "exact/opaque/leaf.ts": "python", "open/no.ts": "python",
	}}
	probe := rebuildLinkCandidateProbe(&n, census, nil)
	for _, key := range []string{"exact/Foo.ts", "exact/foo.ts"} {
		got, err := probe(key)
		require.NoError(t, err)
		require.False(t, got)
	}
	for _, key := range []string{"exact/opaque/leaf.ts", "exact/link/leaf.ts", "open/no.ts"} {
		got, err := probe(key)
		require.False(t, got)
		require.ErrorIs(t, err, errRebuildResolverNamespace)
		if key == "exact/opaque/leaf.ts" {
			require.ErrorIs(t, err, errRebuildResolverInputDenied)
		}
	}
	_, err = rebuildLinkCandidateProbe(&n, &rebuildExtractorCensus{admitted: 1}, nil)("exact/Foo.ts")
	require.ErrorIs(t, err, errRebuildCallerAttribution)
}

func TestRebuildResolverNamespaceRejectsRows(t *testing.T) {
	for _, tc := range []struct {
		name   string
		prefix []rebuildNamespaceRow
		bad    rebuildNamespaceRow
	}{
		{"duplicate", []rebuildNamespaceRow{{"x", rebuildNamespaceRegular}}, rebuildNamespaceRow{"x", rebuildNamespaceRegular}},
		{"conflict", []rebuildNamespaceRow{{"x", rebuildNamespaceRegular}}, rebuildNamespaceRow{"x", rebuildNamespaceDirectory}},
		{"child before parent", nil, rebuildNamespaceRow{"x/y", rebuildNamespaceRegular}},
		{"close before parent", nil, rebuildNamespaceRow{"x", rebuildNamespaceCloseExact}},
		{"double close", []rebuildNamespaceRow{{".", rebuildNamespaceCloseExact}}, rebuildNamespaceRow{".", rebuildNamespaceCloseExact}},
		{"upgrade closure", []rebuildNamespaceRow{{".", rebuildNamespaceClose}}, rebuildNamespaceRow{".", rebuildNamespaceCloseExact}},
		{"close before child", []rebuildNamespaceRow{{".", rebuildNamespaceCloseExact}}, rebuildNamespaceRow{"x", rebuildNamespaceRegular}},
		{"denied descendant", []rebuildNamespaceRow{{"x", rebuildNamespaceDenied}}, rebuildNamespaceRow{"x/y", rebuildNamespaceRegular}},
		{"close denied", []rebuildNamespaceRow{{"x", rebuildNamespaceDenied}}, rebuildNamespaceRow{"x", rebuildNamespaceCloseExact}},
		{"unknown type", nil, rebuildNamespaceRow{"x", rebuildNamespaceUnknown}},
		{"forged negative", nil, rebuildNamespaceRow{"x", rebuildNamespaceAbsent}},
		{"raw dotdot", nil, rebuildNamespaceRow{"x/../y", rebuildNamespaceRegular}},
		{"absolute", nil, rebuildNamespaceRow{"/x", rebuildNamespaceRegular}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var n rebuildResolverNamespace
			for _, row := range tc.prefix {
				require.NoError(t, n.add(row))
			}
			require.ErrorIs(t, n.add(tc.bad), errRebuildResolverNamespace)
			require.ErrorIs(t, n.freeze(), errRebuildResolverNamespace)
			kind, err := n.lookup("another")
			require.ErrorIs(t, err, errRebuildResolverNamespace)
			require.Equal(t, rebuildNamespaceUnknown, kind)
		})
	}
}

func TestRebuildResolverNamespaceCaps(t *testing.T) {
	t.Run("key length", func(t *testing.T) {
		var n rebuildResolverNamespace
		key := strings.Repeat("x", rebuildNamespaceKeyLimit)
		require.NoError(t, n.add(rebuildNamespaceRow{key, rebuildNamespaceRegular}))
		require.ErrorIs(t, n.add(rebuildNamespaceRow{key + "x", rebuildNamespaceRegular}), errRebuildResolverNamespace)
	})
	t.Run("retained key bytes", func(t *testing.T) {
		var n rebuildResolverNamespace
		pad := strings.Repeat("x", rebuildNamespaceKeyLimit-8)
		for i := range rebuildNamespaceKeyBytesLimit / rebuildNamespaceKeyLimit {
			key := fmt.Sprintf("%08d%s", i, pad)
			if err := n.add(rebuildNamespaceRow{key, rebuildNamespaceRegular}); err != nil {
				t.Fatalf("key %d: %v", i, err)
			}
		}
		require.ErrorIs(t, n.add(rebuildNamespaceRow{"extra", rebuildNamespaceRegular}), errRebuildResolverNamespace)
		require.ErrorIs(t, n.freeze(), errRebuildResolverNamespace)
	})
	t.Run("entries", func(t *testing.T) {
		var n rebuildResolverNamespace
		for i := range rebuildNamespaceEntryLimit {
			if err := n.add(rebuildNamespaceRow{fmt.Sprintf("x%d", i), rebuildNamespaceRegular}); err != nil {
				t.Fatalf("entry %d: %v", i, err)
			}
		}
		require.ErrorIs(t, n.add(rebuildNamespaceRow{"extra", rebuildNamespaceRegular}), errRebuildResolverNamespace)
		require.ErrorIs(t, n.freeze(), errRebuildResolverNamespace)
	})
}
