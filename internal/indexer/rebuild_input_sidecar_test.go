package indexer

import (
	"bufio"
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/zeebo/blake3"
)

func TestRebuildInputSidecarNamespace(t *testing.T) {
	ctx := t.Context()
	rows := []rebuildNamespaceRow{
		{"dir", rebuildNamespaceDirectory},
		{"dir/item-\xff.js", rebuildNamespaceRegular},
		{"dir", rebuildNamespaceCloseExact},
		{"empty", rebuildNamespaceDirectory},
		{"empty", rebuildNamespaceCloseExact},
		{"denied", rebuildNamespaceDenied},
		{"link", rebuildNamespaceUnsupported},
		{".", rebuildNamespaceCloseExact},
	}
	live := t.TempDir()
	require.NoError(t, os.Mkdir(filepath.Join(live, "dir"), 0o700))
	liveName := filepath.Join(live, "dir", "missing.js")
	require.NoError(t, os.WriteFile(liveName, nil, 0o600))
	lease := t.TempDir()
	left := int64(rebuildCppRawLimit)
	digest, err := writeRebuildInputSidecar(ctx, lease, func(emit func(rebuildSidecarRow) error) error {
		for _, row := range rows {
			if err := writeRebuildNamespaceFact(&left, row, emit); err != nil {
				return err
			}
		}
		return nil
	})
	require.NoError(t, err)
	var charge int64
	for _, row := range rows {
		charge += int64(len(row.key)) + 1
	}
	require.Equal(t, int64(rebuildCppRawLimit)-charge, left)

	for _, mutate := range []func(){func() {}, func() { require.NoError(t, os.Remove(liveName)) }} {
		mutate()
		ns, err := openRebuildResolverNamespace(ctx, lease, digest)
		require.NoError(t, err)
		for key, want := range map[string]rebuildNamespaceKind{
			"dir/item-\xff.js": rebuildNamespaceRegular,
			"dir/missing.js":   rebuildNamespaceAbsent,
			"empty":            rebuildNamespaceDirectory,
			"empty/missing.js": rebuildNamespaceAbsent,
		} {
			got, err := ns.lookup(key)
			require.NoError(t, err, key)
			require.Equal(t, want, got, key)
		}
		for _, key := range []string{"denied", "denied/unknown", "link/unknown"} {
			_, err := ns.lookup(key)
			require.ErrorIs(t, err, errRebuildResolverNamespace, key)
		}
	}
	ns, err := openRebuildResolverNamespace(ctx, lease, rebuildDigest{1})
	require.ErrorIs(t, err, errRebuildInputSidecar)
	require.Nil(t, ns)
}

func TestRebuildInputSidecarNamespaceRefusals(t *testing.T) {
	row := rebuildNamespaceRow{".", rebuildNamespaceCloseExact}
	left := int64(1)
	err := writeRebuildNamespaceFact(&left, row, func(rebuildSidecarRow) error {
		t.Fatal("over-cap row was sent")
		return nil
	})
	require.ErrorIs(t, err, errRebuildCppCaptureLimit)
	require.Equal(t, int64(1), left)
	require.ErrorIs(t, (rebuildSidecarRow{Kind: rebuildNamespaceRegularFact, Path: []byte(".")}).check(), errRebuildInputSidecar)
	require.ErrorIs(t, (rebuildSidecarRow{Kind: rebuildNamespaceCloseExactFact, Path: []byte("."), Digest: rebuildDigest{1}}).check(), errRebuildInputSidecar)
	lease := t.TempDir()
	digest, err := writeRebuildInputSidecar(t.Context(), lease, func(emit func(rebuildSidecarRow) error) error {
		return emit(rebuildSidecarRow{Kind: rebuildNamespaceRegularFact, Path: []byte("file")})
	})
	require.NoError(t, err)
	ns, err := openRebuildResolverNamespace(t.Context(), lease, digest)
	require.ErrorIs(t, err, errRebuildResolverNamespace)
	require.Nil(t, ns)
}

func TestRebuildInputSidecarByteIdentity(t *testing.T) {
	ctx := t.Context()
	rows := []rebuildSidecarRow{
		rebuildSidecarCppRow(rebuildCppFact{Kind: rebuildCppDatabase, Path: compileDBName}),
		rebuildSidecarCppRow(rebuildCppFact{Kind: rebuildCppDatabase, Path: "build/" + compileDBName, Present: true, Digest: blake3.Sum256([]byte("db"))}),
		rebuildSidecarCppRow(rebuildCppFact{Kind: rebuildCppTU, Path: "a.c", IncludeDirs: []string{"b", "a"}}),
		rebuildSidecarCppRow(rebuildCppFact{Kind: rebuildCppTU, Path: "a.c"}),
		{Kind: rebuildFollowedManifest, Path: []byte("package.json"), Present: true, Digest: blake3.Sum256([]byte("manifest bytes"))},
	}
	leases := []string{filepath.Join(t.TempDir(), "repo1"), filepath.Join(t.TempDir(), "repo[1]")}
	var digests []rebuildDigest
	for _, lease := range leases {
		require.NoError(t, os.Mkdir(lease, 0o700))
		digest, err := writeRebuildInputSidecar(ctx, lease, func(emit func(rebuildSidecarRow) error) error {
			for _, row := range rows {
				if err := emit(row); err != nil {
					return err
				}
			}
			return nil
		})
		require.NoError(t, err)
		digests = append(digests, digest)
		var got []rebuildSidecarRow
		require.NoError(t, visitRebuildInputSidecar(ctx, lease, digest, func(row rebuildSidecarRow) error {
			got = append(got, row)
			return nil
		}))
		require.Equal(t, rows, got, "keep order and empty TU override")
	}
	require.Equal(t, digests[0], digests[1], "lease path is not input data")
}

func TestRebuildInputSidecarRefusesCorruption(t *testing.T) {
	ctx := t.Context()
	lease := t.TempDir()
	row := rebuildSidecarCppRow(rebuildCppFact{Kind: rebuildCppHeuristicDir, Path: "include-\xff"})
	digest, err := writeRebuildInputSidecar(ctx, lease, func(emit func(rebuildSidecarRow) error) error { return emit(row) })
	require.NoError(t, err)
	checked := 0
	visit := func(got rebuildSidecarRow) error {
		require.Equal(t, row, got, "directory spelling survives encoding")
		checked++
		return nil
	}
	require.NoError(t, visitRebuildInputSidecar(ctx, lease, digest, visit))
	require.Equal(t, 1, checked)
	missing := errors.New("private index refused a row")
	require.ErrorIs(t, visitRebuildInputSidecar(ctx, lease, digest, func(rebuildSidecarRow) error { return missing }), missing)
	require.ErrorIs(t, visitRebuildInputSidecar(ctx, lease, rebuildDigest{1}, visit), errRebuildInputSidecar)
	file := filepath.Join(lease, rebuildInputSidecarName)
	data, err := os.ReadFile(file)
	require.NoError(t, err)
	require.NoError(t, os.Truncate(file, int64(len(data)-1)))
	require.ErrorIs(t, visitRebuildInputSidecar(ctx, lease, digest, visit), errRebuildInputSidecar)
}

func TestRebuildInputSidecarRefusesCaps(t *testing.T) {
	ctx := t.Context()
	var data bytes.Buffer
	w := bufio.NewWriter(&data)
	left := rebuildInputEncodedLimit
	wide := rebuildSidecarRow{Kind: rebuildCppTU, Path: []byte("a.c"), Dirs: [][]byte{bytes.Repeat([]byte{'w'}, 4*rebuildCppEntryLimit/5)}}
	require.ErrorIs(t, writeRebuildSidecarRow(ctx, w, &left, wide), errRebuildCppCaptureLimit)
	require.Equal(t, rebuildInputEncodedLimit, left)
	small := rebuildSidecarRow{Kind: rebuildCppTU, Path: []byte("a.c")}
	require.NoError(t, writeRebuildSidecarRow(ctx, w, &left, small))
	exact := rebuildInputEncodedLimit - left
	left = exact
	require.NoError(t, writeRebuildSidecarRow(ctx, w, &left, small))
	require.Zero(t, left)
	require.ErrorIs(t, writeRebuildSidecarRow(ctx, w, &left, small), errRebuildCppCaptureLimit)
	require.NoError(t, w.Flush())
	require.Equal(t, 2*exact, data.Len(), "rejected rows wrote no partial frame")
}
