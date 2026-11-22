//go:build unix

package indexer

import (
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/zzet/gortex/internal/indexer/source"
)

func TestRebuildGoObservationRows(t *testing.T) {
	const rootMod = "module example.org/root\n"
	const nestedMod = "module example.org/nested\n"
	const work = "go 1.27.3\n"
	files := map[string]string{
		"go.mod": rootMod, "pkg/a.go": "package a\n",
		"nested/go.mod": nestedMod, "nested/b.go": "package b\n",
		"workspace/go.work": work, "workspace/c.go": "package c\n",
		"workspace/go.mod": "ignored, and not consulted past go.work\n",
	}
	manifests := map[string]*rebuildInput{}
	for _, key := range []string{"go.mod", "nested/go.mod", "workspace/go.work"} {
		in := rebuildGoTestInput(key, "manifest", files[key])
		manifests[key] = &in
	}
	inputs := []rebuildInput{
		rebuildGoTestInput("workspace/c.go", "go", files["workspace/c.go"]),
		rebuildGoTestInput("pkg/a.go", "go", files["pkg/a.go"]),
		rebuildGoTestInput("nested/b.go", "go", files["nested/b.go"]),
	}
	want := []rebuildGoManifest{
		{Path: "go.mod", Present: true, Digest: manifests["go.mod"].Content, RootClaim: "example.org/root", OwnerModule: "example.org/root"},
		{Path: "go.work"},
		{Path: "nested/go.mod", Present: true, Digest: manifests["nested/go.mod"].Content, OwnerModule: "example.org/nested"},
		{Path: "nested/go.work"},
		{Path: "pkg/go.mod"},
		{Path: "pkg/go.work"},
		{Path: "workspace/go.work", Present: true, Digest: manifests["workspace/go.work"].Content},
	}
	for _, leaf := range []string{"repo", "repo[1]"} {
		t.Run(leaf, func(t *testing.T) {
			checkout := filepath.Join(t.TempDir(), leaf)
			for name, contents := range files {
				file := filepath.Join(checkout, filepath.FromSlash(name))
				require.NoError(t, os.MkdirAll(filepath.Dir(file), 0o700))
				require.NoError(t, os.WriteFile(file, []byte(contents), 0o600))
			}
			root, err := os.OpenRoot(checkout)
			require.NoError(t, err)
			t.Cleanup(func() { require.NoError(t, root.Close()) })
			var namespace rebuildResolverNamespace
			raw := int64(rebuildCppRawLimit)
			for _, dir := range []string{".", "nested", "pkg", "workspace"} {
				// No spelling aliases in this fixture. This is not proof that
				// an arbitrary production filesystem uses exact name equality.
				err := captureRebuildResolverDirectory(t.Context(), root, dir, true, func(key string, _ fs.FileMode) (bool, error) {
					return key != "workspace/go.mod", nil
				}, &namespace, &raw, func(rebuildSidecarRow) error { return nil })
				require.NoError(t, err)
			}
			require.NoError(t, namespace.freeze())
			before := raw
			var rows []rebuildGoManifest
			err = captureRebuildGoObservations(t.Context(), root, &namespace, inputs, manifests, &raw, func(row rebuildGoManifest) error {
				rows = append(rows, row)
				return nil
			})
			require.NoError(t, err)
			require.Equal(t, want, rows)
			require.Equal(t, int64(len(rootMod)+len(nestedMod)+len(work)), before-raw)

			require.NoError(t, os.Remove(filepath.Join(checkout, "go.mod")))
			require.NoError(t, os.WriteFile(filepath.Join(checkout, "pkg", "go.mod"), []byte(nestedMod), 0o600))
			require.NoError(t, os.Remove(filepath.Join(checkout, "workspace", "go.work")))
			lookup := func(key string) (rebuildGoManifest, error) {
				for _, row := range rows {
					if row.Path == key {
						return row, nil
					}
				}
				return rebuildGoManifest{}, errRebuildGoEvidence
			}
			owner, err := rebuildGoNearest("pkg", lookup)
			require.NoError(t, err)
			require.Equal(t, "example.org/root", owner.OwnerModule)
			owner, err = rebuildGoNearest("workspace", lookup)
			require.NoError(t, err)
			require.Empty(t, owner.OwnerModule)
			require.Equal(t, want, rows)
		})
	}
}

func TestRebuildGoObservationAuthorityRefusals(t *testing.T) {
	in := rebuildGoTestInput("go.mod", "manifest", "module example.org/root\n")
	wrongPath := rebuildGoTestInput("other/go.mod", "manifest", "module example.org/root\n")
	for _, tc := range []struct {
		name  string
		kind  rebuildNamespaceKind
		close rebuildNamespaceKind
		claim *rebuildInput
		want  error
	}{
		{"denied", rebuildNamespaceDenied, rebuildNamespaceCloseExact, &in, errRebuildResolverNamespace},
		{"symlink or special", rebuildNamespaceUnsupported, rebuildNamespaceCloseExact, &in, errRebuildResolverNamespace},
		{"open", rebuildNamespaceRegular, rebuildNamespaceUnknown, &in, errRebuildResolverNamespace},
		{"ambiguous miss", rebuildNamespaceUnknown, rebuildNamespaceClose, nil, errRebuildResolverNamespace},
		{"directory", rebuildNamespaceDirectory, rebuildNamespaceCloseExact, &in, errRebuildGoEvidence},
		{"regular without identity", rebuildNamespaceRegular, rebuildNamespaceCloseExact, nil, errRebuildGoEvidence},
		{"identity for a different name", rebuildNamespaceRegular, rebuildNamespaceCloseExact, &wrongPath, errRebuildGoEvidence},
		{"negative contradicted by identity", rebuildNamespaceUnknown, rebuildNamespaceCloseExact, &in, errRebuildGoEvidence},
	} {
		t.Run(tc.name, func(t *testing.T) {
			// The closed handle makes accidental live access observable as a
			// read/drift error instead of the required authority refusal.
			root, err := os.OpenRoot(t.TempDir())
			require.NoError(t, err)
			require.NoError(t, root.Close())
			var namespace rebuildResolverNamespace
			if tc.kind != rebuildNamespaceUnknown {
				require.NoError(t, namespace.add(rebuildNamespaceRow{key: "go.mod", kind: tc.kind}))
			}
			if tc.close == rebuildNamespaceClose {
				require.NoError(t, namespace.add(rebuildNamespaceRow{key: "GO.MOD", kind: rebuildNamespaceRegular}))
			}
			if tc.close != rebuildNamespaceUnknown {
				require.NoError(t, namespace.add(rebuildNamespaceRow{key: ".", kind: tc.close}))
			}
			require.NoError(t, namespace.freeze())
			raw := int64(rebuildGoManifestLimit)
			err = captureRebuildGoObservations(t.Context(), root, &namespace, nil, map[string]*rebuildInput{"go.mod": tc.claim}, &raw,
				func(row rebuildGoManifest) error { t.Fatalf("emitted an unproved row: %+v", row); return nil })
			require.ErrorIs(t, err, tc.want)
			require.NotErrorIs(t, err, ErrBuildInputChanged)
			require.Equal(t, int64(rebuildGoManifestLimit), raw)
		})
	}
}

func TestRebuildGoObservationDriftAndLimits(t *testing.T) {
	const mod = "module example.org/root\n"
	const work = "go 1.27.3\n"
	for _, tc := range []struct {
		name   string
		key    string
		frozen string
		live   string
		raw    int64
		want   error
	}{
		{"mod bytes changed at same size", "go.mod", mod, "module example.org/edit\n", rebuildGoManifestLimit, ErrBuildInputChanged},
		{"workspace bytes changed at same size", "go.work", work, "go 1.27.4\n", rebuildGoManifestLimit, ErrBuildInputChanged},
		{"manifest deleted", "go.mod", mod, "", rebuildGoManifestLimit, ErrBuildInputChanged},
		{"manifest replaced by symlink", "go.mod", mod, "->other.mod", rebuildGoManifestLimit, ErrBuildInputChanged},
		{"aggregate cap before touching missing path", "go.mod", mod, "", int64(len(mod) - 1), errRebuildGoCaptureLimit},
		{"workspace cap before touching missing path", "go.work", strings.Repeat("x", rebuildGoManifestLimit+1), "", 2 * rebuildGoManifestLimit, errRebuildGoCaptureLimit},
	} {
		t.Run(tc.name, func(t *testing.T) {
			checkout := t.TempDir()
			file := filepath.Join(checkout, tc.key)
			require.NoError(t, os.WriteFile(file, []byte(tc.frozen), 0o600))
			var inputs []rebuildInput
			if tc.key == "go.work" {
				const app = "package app\n"
				require.NoError(t, os.WriteFile(filepath.Join(checkout, "app.go"), []byte(app), 0o600))
				inputs = []rebuildInput{rebuildGoTestInput("app.go", "go", app)}
			}
			src, err := source.NewFilesystemSource(checkout)
			require.NoError(t, err)
			t.Cleanup(func() { require.NoError(t, src.Close()) })
			in, _, err := readRebuildInput(t.Context(), src, tc.key, "manifest", int64(len(tc.frozen)))
			require.NoError(t, err)
			var namespace rebuildResolverNamespace
			raw := int64(rebuildCppRawLimit)
			require.NoError(t, captureRebuildResolverDirectory(t.Context(), src.FrozenRoot(), ".", true,
				func(string, fs.FileMode) (bool, error) { return true, nil }, &namespace, &raw, func(rebuildSidecarRow) error { return nil }))
			require.NoError(t, namespace.freeze())
			switch target, link := strings.CutPrefix(tc.live, "->"); {
			case link:
				require.NoError(t, os.WriteFile(filepath.Join(checkout, target), []byte(tc.frozen), 0o600))
				require.NoError(t, os.Remove(file))
				require.NoError(t, os.Symlink(target, file))
			case tc.live == "":
				require.NoError(t, os.Remove(file))
			default:
				require.Len(t, tc.live, len(tc.frozen))
				require.NoError(t, os.WriteFile(file, []byte(tc.live), 0o600))
			}
			raw = tc.raw
			err = captureRebuildGoObservations(t.Context(), src.FrozenRoot(), &namespace, inputs, map[string]*rebuildInput{tc.key: &in}, &raw,
				func(row rebuildGoManifest) error {
					t.Fatalf("emitted a row for changed or oversized input: %+v", row)
					return nil
				})
			require.ErrorIs(t, err, tc.want)
			require.Equal(t, tc.raw, raw)
		})
	}
}
