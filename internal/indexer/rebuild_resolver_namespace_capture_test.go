//go:build unix

package indexer

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestRebuildResolverDirectoryCapture(t *testing.T) {
	var first rebuildDigest
	for _, leaf := range []string{"repo", "repo[1]"} {
		t.Run(leaf, func(t *testing.T) {
			work := filepath.Join(t.TempDir(), leaf)
			for _, dir := range []string{"empty", "denied"} {
				require.NoError(t, os.MkdirAll(filepath.Join(work, dir), 0o700))
			}
			require.NoError(t, os.WriteFile(filepath.Join(work, "denied", "never-listed.ts"), nil, 0o600))
			require.NoError(t, os.Symlink("empty", filepath.Join(work, "link")))
			root, err := os.OpenRoot(work)
			require.NoError(t, err)
			t.Cleanup(func() { require.NoError(t, root.Close()) })
			lease := t.TempDir()
			raw := int64(rebuildCppRawLimit)
			var n rebuildResolverNamespace
			var classified []string
			allowed := func(key string, _ fs.FileMode) (bool, error) {
				classified = append(classified, key)
				return key != "denied", nil
			}
			digest, err := writeRebuildInputSidecar(t.Context(), lease, func(emit func(rebuildSidecarRow) error) error {
				// This fixture controls exact spelling. It does not certify
				// the host filesystem's native lookup equality for production.
				if err := captureRebuildResolverDirectory(t.Context(), root, ".", true, allowed, &n, &raw, emit); err != nil {
					return err
				}
				require.ErrorIs(t, captureRebuildResolverDirectory(t.Context(), root, "denied", true, allowed, &n, &raw, emit), errRebuildResolverNamespace)
				return captureRebuildResolverDirectory(t.Context(), root, "empty", true, allowed, &n, &raw, emit)
			})
			require.NoError(t, err)
			require.ElementsMatch(t, []string{"empty", "denied", "link"}, classified)
			if first == (rebuildDigest{}) {
				first = digest
			} else {
				require.Equal(t, first, digest)
			}
			check := func() {
				replay, err := openRebuildResolverNamespace(t.Context(), lease, digest)
				require.NoError(t, err)
				kind, err := replay.lookup("empty")
				require.NoError(t, err)
				require.Equal(t, rebuildNamespaceDirectory, kind)
				kind, err = replay.lookup("empty/missing.ts")
				require.NoError(t, err)
				require.Equal(t, rebuildNamespaceAbsent, kind)
				_, err = replay.lookup("denied/never-listed.ts")
				require.ErrorIs(t, err, errRebuildResolverNamespace)
				_, err = replay.lookup("link/missing.ts")
				require.ErrorIs(t, err, errRebuildResolverNamespace)
			}
			check()
			require.NoError(t, os.Remove(filepath.Join(work, "empty")))
			check() // Observed empty directory and child miss survive live deletion.
		})
	}
}

func TestRebuildResolverDirectoryCaptureRefusals(t *testing.T) {
	work := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(work, "present"), nil, 0o600))
	root, err := os.OpenRoot(work)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, root.Close()) })
	policyErr := errors.New("classification unknown")
	for _, tc := range []struct {
		remaining int64
		classify  error
		want      error
	}{
		{1, nil, errRebuildCppCaptureLimit},
		{rebuildCppRawLimit, policyErr, policyErr},
	} {
		var n rebuildResolverNamespace
		err := captureRebuildResolverDirectory(t.Context(), root, ".", false, func(string, fs.FileMode) (bool, error) {
			return false, tc.classify
		}, &n, &tc.remaining, func(rebuildSidecarRow) error {
			t.Fatal("failed directory was emitted or closed")
			return nil
		})
		require.ErrorIs(t, err, tc.want)
		require.Empty(t, n.entries)
	}
}
