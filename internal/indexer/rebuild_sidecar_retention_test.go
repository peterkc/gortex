package indexer

import (
	"os"
	"path/filepath"
	"strings"
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

func TestRebuildSidecarCheckpoint(t *testing.T) {
	for _, name := range []string{"bound", "missing", "corrupt", "unbound"} {
		t.Run(name, func(t *testing.T) {
			ctx := t.Context()
			headDir, ownerLease, inputLease := t.TempDir(), t.TempDir(), t.TempDir()
			digest, err := writeRebuildInputSidecar(ctx, inputLease, func(emit func(rebuildSidecarRow) error) error {
				return emit(rebuildSidecarRow{Kind: rebuildNamespaceCloseExactFact, Path: []byte(".")})
			})
			require.NoError(t, err)
			require.NotZero(t, digest)
			raw, err := os.ReadFile(filepath.Join(inputLease, rebuildInputSidecarName))
			require.NoError(t, err)
			if name != "missing" {
				require.NoError(t, retainRebuildInputSidecar(ctx, headDir, inputLease, digest))
			}
			w := store_pebble.Witness{
				Input: store_pebble.HeadDigest{1}, Recipe: store_pebble.HeadDigest{2},
				Profile: store_pebble.DefaultProfile, Stage: store_pebble.StageManifest,
			}
			if name != "unbound" {
				w.FrozenInputs = store_pebble.HeadDigest(digest)
			}
			file := store_pebble.ManifestFile{
				Path: "dep.ts", Language: "typescript", Kind: 'f', Size: 1,
				Content: store_pebble.HeadDigest{3}, Extractor: store_pebble.HeadDigest{4},
			}
			tables, err := store_pebble.PrepareManifest(ctx, t.TempDir(), []store_pebble.ManifestFile{file}, w, rebuildTestIncarnation)
			require.NoError(t, err)
			var expected store_pebble.LocalPublicationRecord
			dest := filepath.Join(ownerLease, rebuildCheckpointName)
			if !t.Run("take", func(t *testing.T) {
				head, err := store_pebble.OpenHead(headDir, rebuildTestIncarnation, w.Profile)
				require.NoError(t, err)
				t.Cleanup(func() { require.NoError(t, head.Close()) })
				published, err := head.PublishStage(t.Context(), tables)
				require.NoError(t, err)
				require.NotZero(t, published)
				expected = store_pebble.LocalPublicationRecord{
					Version: tables.Witness.Version(), Incarnation: rebuildTestIncarnation, WitnessDigest: published,
				}
				if name == "corrupt" {
					raw[len(raw)-1] ^= 1
					require.NoError(t, os.WriteFile(retainedRebuildInputPath(headDir, digest), raw, 0o600))
				}
				got, err := checkpointRebuildHead(t.Context(), head, headDir, ownerLease, rebuildTestIncarnation, w.Profile)
				if name == "missing" || name == "corrupt" {
					require.ErrorIs(t, err, errRebuildInputSidecar)
					if name == "missing" {
						require.ErrorIs(t, err, os.ErrNotExist)
					}
					require.Zero(t, got, "refusal must not return a registrable record")
					_, err = os.Lstat(retainedRebuildInputPath(dest, digest))
					require.ErrorIs(t, err, os.ErrNotExist)
					return
				}
				require.NoError(t, err)
				require.Equal(t, expected, got)
			}) {
				return
			}
			// Reopen the source first, so the success case also proves that the
			// checkpoint survives a restart of the original database.
			checkRebuildCheckpointManifest(t, headDir, tables.Witness, expected, file)
			if name == "missing" || name == "corrupt" {
				return
			}
			checkRebuildCheckpointManifest(t, dest, tables.Witness, expected, file)
			if name == "unbound" {
				// The source has a sidecar, but this witness refers to none.
				entries, err := os.ReadDir(dest)
				require.NoError(t, err)
				for _, entry := range entries {
					require.False(t, strings.HasPrefix(entry.Name(), rebuildInputSidecarName), entry.Name())
				}
				return
			}
			got, err := os.ReadFile(retainedRebuildInputPath(dest, digest))
			require.NoError(t, err)
			require.Equal(t, raw, got)
		})
	}
}

func checkRebuildCheckpointManifest(t *testing.T, dir string, witness store_pebble.Witness, expected store_pebble.LocalPublicationRecord, file store_pebble.ManifestFile) {
	t.Helper()
	head, err := store_pebble.OpenHead(dir, expected.Incarnation, witness.Profile)
	require.NoError(t, err)
	defer func() { require.NoError(t, head.Close()) }()
	w, record, err := head.ReconcilePublication(nil)
	require.NoError(t, err)
	require.Equal(t, witness, w)
	require.Len(t, w.Families, 1)
	require.EqualValues(t, 1, w.Families[0].Count)
	require.Equal(t, expected, record)
	snapshot := head.OpenSnapshot()
	defer func() { require.NoError(t, snapshot.Close()) }()
	got, err := snapshot.ManifestFile(file.Path)
	require.NoError(t, err)
	require.Equal(t, file, got)
}

func TestRebuildSidecarCheckpointRefusesDestination(t *testing.T) {
	const profile = store_pebble.DefaultProfile
	headDir, ownerLease := t.TempDir(), t.TempDir()
	head, err := store_pebble.OpenHead(headDir, rebuildTestIncarnation, profile)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, head.Close()) })
	dest := filepath.Join(ownerLease, rebuildCheckpointName)
	refuse := func(lease string, want error) {
		t.Helper()
		got, err := checkpointRebuildHead(t.Context(), head, headDir, lease, rebuildTestIncarnation, profile)
		require.ErrorIs(t, err, want)
		require.Zero(t, got)
	}
	refuse("relative-lease", os.ErrInvalid)
	refuse(ownerLease+string(os.PathSeparator)+".", os.ErrInvalid)
	refuse(headDir, os.ErrInvalid)
	nested := filepath.Join(headDir, "nested")
	require.NoError(t, os.Mkdir(nested, 0o700))
	alias := filepath.Join(t.TempDir(), "alias")
	require.NoError(t, os.Symlink(headDir, alias))
	refuse(filepath.Join(alias, "nested"), os.ErrInvalid)
	_, err = os.Lstat(filepath.Join(nested, rebuildCheckpointName))
	require.ErrorIs(t, err, os.ErrNotExist)
	_, err = os.Lstat(filepath.Join(headDir, rebuildCheckpointName))
	require.ErrorIs(t, err, os.ErrNotExist)

	require.NoError(t, os.Mkdir(dest, 0o700))
	keep := filepath.Join(dest, "keep")
	require.NoError(t, os.WriteFile(keep, []byte("existing"), 0o600))
	refuse(ownerLease, os.ErrExist)
	got, err := os.ReadFile(keep)
	require.NoError(t, err)
	require.Equal(t, "existing", string(got))

	linkLease := t.TempDir()
	link := filepath.Join(linkLease, rebuildCheckpointName)
	missing := filepath.Join(t.TempDir(), "missing")
	require.NoError(t, os.Symlink(missing, link))
	refuse(linkLease, os.ErrExist)
	target, err := os.Readlink(link)
	require.NoError(t, err)
	require.Equal(t, missing, target)
	_, err = os.Lstat(missing)
	require.ErrorIs(t, err, os.ErrNotExist)
}
