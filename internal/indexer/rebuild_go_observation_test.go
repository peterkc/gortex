//go:build unix

package indexer

import (
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/zeebo/blake3"
	"github.com/zzet/gortex/internal/indexer/merkle"
	"github.com/zzet/gortex/internal/indexer/source"
	"github.com/zzet/gortex/internal/resolver"
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
			err = captureRebuildGoObservations(t.Context(), root, &namespace, inputs, manifests, nil, nil, &raw, func(row rebuildGoManifest) error {
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
			err = captureRebuildGoObservations(t.Context(), root, &namespace, nil, map[string]*rebuildInput{"go.mod": tc.claim}, nil, nil, &raw,
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
			err = captureRebuildGoObservations(t.Context(), src.FrozenRoot(), &namespace, inputs, map[string]*rebuildInput{tc.key: &in}, nil, nil, &raw,
				func(row rebuildGoManifest) error {
					t.Fatalf("emitted a row for changed or oversized input: %+v", row)
					return nil
				})
			require.ErrorIs(t, err, tc.want)
			require.Equal(t, tc.raw, raw)
		})
	}
}

func TestRebuildGoObservationFollowedRootReplay(t *testing.T) {
	const mod = "module example.org/root\n"
	const target = "root.manifest"
	checkout := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(checkout, target), []byte(mod), 0o600))
	require.NoError(t, os.Symlink(target, filepath.Join(checkout, "go.mod")))
	const goSrc = "package a\n"
	inputs := []rebuildInput{rebuildGoTestInput("a.go", "go", goSrc), rebuildGoTestInput("b.go", "go", goSrc)}
	for _, in := range inputs {
		require.NoError(t, os.WriteFile(filepath.Join(checkout, in.Path), []byte(goSrc), 0o600))
	}
	src, err := source.NewFilesystemSource(checkout)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, src.Close()) })
	link, _, err := readRebuildInput(t.Context(), src, "go.mod", "manifest", 1024)
	require.NoError(t, err)
	require.Equal(t, merkle.Symlink, link.Kind)

	candidate := resolver.GoImportCandidate{ImporterFilePath: "a.go", CandidateFilePath: "b.go", ImportPath: "example.org/root"}
	native, err := goPackageOwnershipFromFiles(t.Context(), src, "", map[string]string{"a.go": "", "b.go": ""})
	require.NoError(t, err)
	require.Equal(t, resolver.GoPackageOwnershipUnknown, native(candidate))

	// Stand in for the owner: resolve once, then share this checked read and
	// the actual followed-manifest row with the normalizer. The fixture's
	// only link target is a known admitted in-checkout regular file.
	entry, err := src.ResolveFrozenRootLink(t.Context(), "go.mod", func(raw string) error {
		if int64(len(raw)) != link.Size || blake3.Sum256([]byte(raw)) != link.Content {
			return ErrBuildInputChanged
		}
		return nil
	}, nil)
	require.NoError(t, err)
	require.Equal(t, target, entry.Path())
	data, err := entry.Read(t.Context(), rebuildGoManifestLimit)
	require.NoError(t, err)
	require.Equal(t, mod, string(data))
	effective := rebuildGoTestInput(target, "manifest", mod)
	followed := rebuildRootManifest{link: link, target: effective, data: data,
		row: rebuildSidecarRow{Kind: rebuildFollowedManifest, Path: []byte("go.mod"), Present: true, Digest: effective.Content}}
	require.NotEqual(t, link.Content, followed.row.Digest)

	var namespace rebuildResolverNamespace
	raw := int64(rebuildCppRawLimit)
	require.NoError(t, captureRebuildResolverDirectory(t.Context(), src.FrozenRoot(), ".", true,
		func(string, fs.FileMode) (bool, error) { return true, nil }, &namespace, &raw, func(rebuildSidecarRow) error { return nil }))
	require.NoError(t, namespace.freeze())
	require.NoError(t, os.WriteFile(filepath.Join(checkout, target), []byte("module example.org/edit\n"), 0o600))
	before := raw
	lease := t.TempDir()
	var captured []rebuildGoManifest
	digest, err := writeRebuildInputSidecar(t.Context(), lease, func(emit func(rebuildSidecarRow) error) error {
		if err := emit(followed.row); err != nil {
			return err
		}
		return captureRebuildGoObservations(t.Context(), src.FrozenRoot(), &namespace, inputs,
			map[string]*rebuildInput{"go.mod": &link}, &followed, nil, &raw, func(row rebuildGoManifest) error {
				captured = append(captured, row)
				return emit(rebuildGoManifestSidecarRow(row))
			})
	})
	require.NoError(t, err)
	want := rebuildGoManifest{Path: "go.mod", Present: true, Digest: blake3.Sum256([]byte(mod)), RootClaim: "example.org/root"}
	require.Equal(t, []rebuildGoManifest{want, {Path: "go.work"}}, captured)
	require.Equal(t, int64(len(mod)), before-raw)

	require.NoError(t, os.Remove(filepath.Join(checkout, target)))
	require.NoError(t, os.Remove(filepath.Join(checkout, "go.mod")))
	goFacts, _, err := openRebuildResolverConfigFacts(t.Context(), lease, digest)
	require.NoError(t, err)
	require.Len(t, goFacts, 2)
	require.Equal(t, captured[0], goFacts["go.mod"])
	require.Equal(t, captured[1], goFacts["go.work"])
	nearest, err := rebuildGoNearest(".", goFacts.lookup)
	require.NoError(t, err)
	require.Equal(t, want, nearest, "a positive loose claim is still a strict non-certificate")
	got, err := rebuildGoOwnership(candidate, func(string) (bool, error) { return true, nil }, goFacts.lookup)
	require.NoError(t, err)
	require.Equal(t, resolver.GoPackageOwnershipUnknown, got)
}

func TestRebuildGoObservationFollowedRootRefusals(t *testing.T) {
	const mod = "module example.org/root\n"
	link := rebuildGoTestInput("go.mod", "manifest", "root.manifest")
	link.Kind = merkle.Symlink
	target := rebuildGoTestInput("root.manifest", "manifest", mod)
	valid := rebuildRootManifest{link: link, target: target, data: []byte(mod),
		row: rebuildSidecarRow{Kind: rebuildFollowedManifest, Path: []byte("go.mod"), Present: true, Digest: target.Content}}
	root, err := os.OpenRoot(t.TempDir())
	require.NoError(t, err)
	require.NoError(t, root.Close())
	wrongLink, wrongRow, changedDigest, changedBytes := valid, valid, valid, valid
	wrongLink.link.Content[0] ^= 1
	wrongRow.row.Path = []byte("package.json")
	changedDigest.target.Content[0] ^= 1
	changedBytes.data = []byte("module example.org/edit\n")
	for _, tc := range []struct {
		name    string
		binding *rebuildRootManifest
		raw     int64
		want    error
	}{
		{"unproved target", nil, rebuildGoManifestLimit, errRebuildResolverNamespace},
		{"different original link", &wrongLink, rebuildGoManifestLimit, errRebuildGoEvidence},
		{"wrong followed row", &wrongRow, rebuildGoManifestLimit, errRebuildGoEvidence},
		{"target does not match followed digest", &changedDigest, rebuildGoManifestLimit, ErrBuildInputChanged},
		{"checked bytes no longer match target", &changedBytes, rebuildGoManifestLimit, ErrBuildInputChanged},
		{"target exceeds remaining raw limit", &valid, int64(len(mod) - 1), errRebuildGoCaptureLimit},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var namespace rebuildResolverNamespace
			require.NoError(t, namespace.add(rebuildNamespaceRow{key: "go.mod", kind: rebuildNamespaceUnsupported}))
			require.NoError(t, namespace.add(rebuildNamespaceRow{key: ".", kind: rebuildNamespaceCloseExact}))
			require.NoError(t, namespace.freeze())
			raw := tc.raw
			err := captureRebuildGoObservations(t.Context(), root, &namespace, nil, map[string]*rebuildInput{"go.mod": &link}, tc.binding, nil, &raw,
				func(row rebuildGoManifest) error { t.Fatalf("emitted an unproved row: %+v", row); return nil })
			require.ErrorIs(t, err, tc.want)
			require.Equal(t, tc.raw, raw)
		})
	}
	t.Run("nested link remains unsupported with a valid root binding", func(t *testing.T) {
		var namespace rebuildResolverNamespace
		for _, row := range []rebuildNamespaceRow{
			{key: "go.mod", kind: rebuildNamespaceUnsupported},
			{key: "nested", kind: rebuildNamespaceDirectory},
			{key: "nested/go.mod", kind: rebuildNamespaceUnsupported},
			{key: "nested", kind: rebuildNamespaceCloseExact},
			{key: ".", kind: rebuildNamespaceCloseExact},
		} {
			require.NoError(t, namespace.add(row))
		}
		require.NoError(t, namespace.freeze())
		raw := int64(rebuildGoManifestLimit)
		inputs := []rebuildInput{rebuildGoTestInput("nested/a.go", "go", "package a\n")}
		err := captureRebuildGoObservations(t.Context(), root, &namespace, inputs, map[string]*rebuildInput{"go.mod": &link}, &valid, nil, &raw,
			func(row rebuildGoManifest) error { t.Fatalf("emitted before refusal: %+v", row); return nil })
		require.ErrorIs(t, err, errRebuildResolverNamespace)
	})
}
