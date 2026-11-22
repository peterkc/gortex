package indexer

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/zzet/gortex/internal/graph/store_pebble"
	"github.com/zzet/gortex/internal/indexer/source"
)

func TestRebuildSidecarRetentionReplay(t *testing.T) {
	ctx := t.Context()
	root, leases, headDir := t.TempDir(), t.TempDir(), t.TempDir()
	src, err := source.NewFilesystemSource(root)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, src.Close()) })
	neighbor := filepath.Join(leases, "other-owner")
	require.NoError(t, os.WriteFile(neighbor, []byte("keep"), 0o600))
	rows := []rebuildSidecarRow{
		{Kind: rebuildNamespaceRegularFact, Path: []byte("aliased.ts")},
		{Kind: rebuildNamespaceCloseExactFact, Path: []byte(".")},
	}
	capture := func(_ *int64, emit func(rebuildSidecarRow) error) error {
		for _, row := range rows {
			if err := emit(row); err != nil {
				return err
			}
		}
		return nil
	}
	req := rebuildBuildRequest{Root: root, Witness: store_pebble.Witness{Recipe: store_pebble.HeadDigest{1}}}
	var scratch string
	var exact []byte
	var ref store_pebble.HeadDigest
	err = withRebuildBuildInputs(ctx, leases, src, map[string]*rebuildInput{"go.mod": nil, "package.json": nil}, req, capture,
		func(lease string, bound rebuildBuildRequest) error {
			scratch = lease
			ref = bound.Witness.FrozenInputs
			require.NotEmpty(t, ref)
			require.NoError(t, bound.checkProjectRecipe())
			raw, err := os.ReadFile(filepath.Join(lease, rebuildInputSidecarName))
			require.NoError(t, err)
			exact = raw
			// The owner retains before it can hand off the referencing witness.
			require.NoError(t, retainRebuildInputSidecar(ctx, headDir, lease, rebuildDigest(ref)))
			require.NoError(t, retainRebuildInputSidecar(ctx, headDir, lease, rebuildDigest(ref)))
			return os.WriteFile(filepath.Join(lease, rebuildInputSidecarName), []byte("changed scratch"), 0o600)
		})
	require.NoError(t, err)
	_, err = os.Lstat(scratch)
	require.ErrorIs(t, err, os.ErrNotExist)
	got, err := os.ReadFile(neighbor)
	require.NoError(t, err)
	require.Equal(t, "keep", string(got))
	// A fresh lease reopens by the manifest reference alone. No original
	// worker, checkout input, or already-open sidecar is available.
	resume := t.TempDir()
	require.NoError(t, restoreRebuildInputSidecar(ctx, headDir, resume, rebuildDigest(ref)))
	got, err = os.ReadFile(filepath.Join(resume, rebuildInputSidecarName))
	require.NoError(t, err)
	require.Equal(t, exact, got)
	ns, err := openRebuildResolverNamespace(ctx, resume, rebuildDigest(ref))
	require.NoError(t, err)
	kind, err := ns.lookup("aliased.ts")
	require.NoError(t, err)
	require.Equal(t, rebuildNamespaceRegular, kind)
	kind, err = ns.lookup("missing.ts")
	require.NoError(t, err)
	require.Equal(t, rebuildNamespaceAbsent, kind)
}

func TestRebuildSidecarRetentionRefusals(t *testing.T) {
	for _, name := range []string{"invalid digest", "occupied digest", "wrong type", "symlink", "oversize"} {
		t.Run(name, func(t *testing.T) {
			ctx := t.Context()
			lease, headDir := t.TempDir(), t.TempDir()
			digest, err := writeRebuildInputSidecar(ctx, lease, func(emit func(rebuildSidecarRow) error) error {
				return emit(rebuildSidecarRow{Kind: rebuildNamespaceCloseExactFact, Path: []byte(".")})
			})
			require.NoError(t, err)
			if name == "invalid digest" {
				digest[0] ^= 1
			}
			retained := retainedRebuildInputPath(headDir, digest)
			decoy := filepath.Join(headDir, ".build-inputs-other-owner")
			require.NoError(t, os.WriteFile(decoy, []byte("keep"), 0o600))
			var occupied bool
			var collision []byte
			switch name {
			case "occupied digest":
				occupied = true
				raw, err := os.ReadFile(filepath.Join(lease, rebuildInputSidecarName))
				require.NoError(t, err)
				raw[len(raw)-2] ^= 1
				collision = raw
				require.NoError(t, os.WriteFile(retained, collision, 0o600))
			case "wrong type":
				occupied = true
				require.NoError(t, os.Mkdir(retained, 0o700))
			case "symlink":
				occupied = true
				// Valid bytes behind a wrong type still refuse.
				require.NoError(t, os.Symlink(filepath.Join(lease, rebuildInputSidecarName), retained))
			case "oversize":
				require.NoError(t, os.Truncate(filepath.Join(lease, rebuildInputSidecarName), rebuildInputEncodedLimit+1))
			}
			before, err := os.ReadDir(headDir)
			require.NoError(t, err)
			err = retainRebuildInputSidecar(ctx, headDir, lease, digest)
			require.ErrorIs(t, err, errRebuildInputSidecar)
			if name == "oversize" {
				require.ErrorIs(t, err, errRebuildCppCaptureLimit)
			}
			after, err := os.ReadDir(headDir)
			require.NoError(t, err)
			require.Len(t, after, len(before), "cleanup removes only its own attempt")
			for i, entry := range before {
				require.Equal(t, entry.Name(), after[i].Name())
				require.Equal(t, entry.Type(), after[i].Type())
			}
			if collision != nil {
				got, err := os.ReadFile(retained)
				require.NoError(t, err)
				require.Equal(t, collision, got, "do not replace an occupied digest name")
			}
			kept, err := os.ReadFile(decoy)
			require.NoError(t, err)
			require.Equal(t, "keep", string(kept))

			resume := t.TempDir()
			err = restoreRebuildInputSidecar(ctx, headDir, resume, digest)
			require.ErrorIs(t, err, errRebuildInputSidecar)
			if !occupied {
				require.ErrorIs(t, err, os.ErrNotExist)
			}
			entries, err := os.ReadDir(resume)
			require.NoError(t, err)
			require.Empty(t, entries, "no replay bytes on a failed verification")
		})
	}
}

func TestRebuildSidecarRetentionUnbound(t *testing.T) {
	ctx := t.Context()
	lease, headDir := t.TempDir(), t.TempDir()
	require.NoError(t, retainRebuildInputSidecar(ctx, headDir, lease, rebuildDigest{}))
	require.NoError(t, restoreRebuildInputSidecar(ctx, headDir, lease, rebuildDigest{}))
	for _, dir := range []string{lease, headDir} {
		entries, err := os.ReadDir(dir)
		require.NoError(t, err)
		require.Empty(t, entries)
	}
}
