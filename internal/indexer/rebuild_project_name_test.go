package indexer

import (
	"context"
	"encoding/gob"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/zzet/gortex/internal/config"
	"github.com/zzet/gortex/internal/graph/store_pebble"
	"github.com/zzet/gortex/internal/indexer/source"
	"github.com/zzet/gortex/internal/search"
)

func freezeProjectNameTestManifest(t *testing.T, src source.ContentSource, root, path, contents string) *rebuildInput {
	t.Helper()
	require.NoError(t, os.WriteFile(filepath.Join(root, path), []byte(contents), 0o600))
	input, data, err := readRebuildInput(t.Context(), src, path, "manifest", 1024)
	require.NoError(t, err)
	require.Equal(t, contents, string(data))
	return &input
}

func captureProjectNameTest(t *testing.T, src *source.FilesystemSource, manifests map[string]*rebuildInput, req rebuildBuildRequest) (rebuildBuildRequest, error) {
	t.Helper()
	req.Root = src.FrozenRoot().Name()
	return captureRebuildBuildInputs(t.Context(), t.TempDir(), src, manifests, req, nil)
}

func TestRebuildProjectNameFrozenSelection(t *testing.T) {
	for _, tc := range []struct {
		name, goMod, packageJSON, want string
	}{
		{"go wins with exact bytes", "module example.com/Mix\xffCase\n", `{"name":"@org/widgets"}`, "Mix\xffCase"},
		{"short go name suppresses package", "module example.com/auth\n", `{"name":"@org/widgets"}`, ""},
		{"no go module uses package", "go 1.27\n", `{"name":"@org/widgets"}`, "widgets"},
		{"absent go mod uses package", "", `{"name":"@org/widgets"}`, "widgets"},
		{"both absent ignore checkout basename", "", "", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := filepath.Join(t.TempDir(), "checkoutbase")
			require.NoError(t, os.Mkdir(root, 0o700))
			src, err := source.NewFilesystemSource(root)
			require.NoError(t, err)
			t.Cleanup(func() { require.NoError(t, src.Close()) })
			manifests := map[string]*rebuildInput{"go.mod": nil, "package.json": nil}
			if tc.goMod != "" {
				manifests["go.mod"] = freezeProjectNameTestManifest(t, src, root, "go.mod", tc.goMod)
			}
			if tc.packageJSON != "" {
				manifests["package.json"] = freezeProjectNameTestManifest(t, src, root, "package.json", tc.packageJSON)
			}
			if tc.goMod == "" && tc.packageJSON == "" {
				require.Equal(t, "checkoutbase", search.DetectProjectName(root))
			}
			req := rebuildBuildRequest{Witness: store_pebble.Witness{Recipe: store_pebble.HeadDigest{1}}}
			bound, err := captureProjectNameTest(t, src, manifests, req)
			require.NoError(t, err)
			require.Equal(t, tc.want, bound.ProjectName)
			require.NotEqual(t, req.Witness.Recipe, bound.Witness.Recipe)
		})
	}
}

func TestRebuildProjectNameRefusesUnverified(t *testing.T) {
	root, src := rebuildInputFixture(t)
	pkg := freezeProjectNameTestManifest(t, src, root, "package.json", `{"name":"validfallback"}`)
	req := rebuildBuildRequest{Witness: store_pebble.Witness{Recipe: store_pebble.HeadDigest{1}}}
	t.Run("missing evidence is not absence", func(t *testing.T) {
		bound, err := captureProjectNameTest(t, src, map[string]*rebuildInput{"package.json": pkg}, req)
		require.ErrorIs(t, err, errRebuildProjectNameInput)
		require.Empty(t, bound)
	})
	t.Run("wrong root identity", func(t *testing.T) {
		bound, err := captureProjectNameTest(t, src, map[string]*rebuildInput{"go.mod": pkg, "package.json": pkg}, req)
		require.ErrorIs(t, err, errRebuildProjectNameInput)
		require.Empty(t, bound)
	})
	t.Run("same length change refuses instead of falling back", func(t *testing.T) {
		goMod := freezeProjectNameTestManifest(t, src, root, "go.mod", "module example.com/oldname\n")
		require.NoError(t, os.WriteFile(filepath.Join(root, "go.mod"), []byte("module example.com/newname\n"), 0o600))
		bound, err := captureProjectNameTest(t, src, map[string]*rebuildInput{"go.mod": goMod, "package.json": pkg}, req)
		require.ErrorIs(t, err, ErrBuildInputChanged)
		require.Empty(t, bound)
		require.NoError(t, os.Remove(filepath.Join(root, "go.mod")))
	})
	t.Run("symlink bytes are not file contents", func(t *testing.T) {
		if runtime.GOOS == "windows" {
			t.Skip("creating a symlink requires a Windows privilege the test runner may not have")
		}
		freezeProjectNameTestManifest(t, src, root, "go-target", "module example.com/linkedname\n")
		require.NoError(t, os.Symlink("go-target", filepath.Join(root, "go.mod")))
		link, data, err := readRebuildInput(t.Context(), src, "go.mod", "manifest", 1024)
		require.NoError(t, err)
		require.Equal(t, "go-target", string(data))
		require.Equal(t, "linkedname", search.DetectProjectName(root))
		bound, err := captureProjectNameTest(t, src, map[string]*rebuildInput{"go.mod": &link, "package.json": pkg}, req)
		require.NoError(t, err)
		require.Equal(t, "linkedname", bound.ProjectName)
		require.NotEmpty(t, bound.Witness.FrozenInputs)
	})
}

func TestRebuildCheckedManifestLink(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("creating a symlink requires a Windows privilege the test runner may not have")
	}
	const data = "module example.org/checked\n"
	for _, tc := range []struct {
		name, linkTarget string
		admitted         bool
		limit            int64
		want             error
	}{
		{"admitted", "manifests/module.txt", true, 1024, nil},
		{"no admitted identity", "manifests/module.txt", false, 1024, errRebuildResolverInputDenied},
		{"opaque target directory", "ignored/data", true, 1024, errRebuildResolverInputDenied},
		{"outside", "../outside", true, 1024, source.ErrOutsideRoot},
		{"dangling", "manifests/missing", true, 1024, errRebuildResolverNamespace},
		{"second link unsupported", "second", true, 1024, errRebuildResolverNamespace},
		{"over caller limit", "manifests/module.txt", true, 1, errRebuildCppCaptureLimit},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := t.Context()
			root, src := rebuildInputFixture(t)
			const targetPath = "manifests/module.txt"
			require.NoError(t, os.Mkdir(filepath.Join(root, "manifests"), 0o700))
			target := freezeProjectNameTestManifest(t, src, root, targetPath, data)
			require.NoError(t, os.Mkdir(filepath.Join(root, "ignored"), 0o700))
			require.NoError(t, os.Symlink("ignored/data", filepath.Join(root, "second")))
			require.NoError(t, os.Symlink(tc.linkTarget, filepath.Join(root, "go.mod")))
			link, _, err := readRebuildInput(ctx, src, "go.mod", "manifest", 1024)
			require.NoError(t, err)
			var ns rebuildResolverNamespace
			left := int64(rebuildCppRawLimit)
			for _, dir := range []string{".", "manifests"} {
				require.NoError(t, captureRebuildResolverDirectory(ctx, src.FrozenRoot(), dir, true,
					func(key string, _ fs.FileMode) (bool, error) { return key != "ignored", nil },
					&ns, &left, func(rebuildSidecarRow) error { return nil }))
			}
			require.NoError(t, ns.freeze())
			admitted := make(map[string]*rebuildInput)
			if tc.admitted {
				admitted[targetPath] = target
			}
			got, checked, row, err := readCheckedRebuildManifestLink(ctx, src, &ns, link, admitted, tc.limit)
			if tc.want != nil {
				require.ErrorIs(t, err, tc.want)
				require.Empty(t, got)
				require.Nil(t, checked)
				require.Empty(t, row)
				return
			}
			require.NoError(t, err)
			require.Equal(t, *target, got)
			require.Equal(t, data, string(checked))
			require.Equal(t, rebuildSidecarRow{Kind: rebuildFollowedManifest, Path: []byte("go.mod"), Present: true, Digest: target.Content}, row)
			require.NotEqual(t, link.Content, row.Digest)
		})
	}
}

func TestRebuildCheckoutManifestCapture(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("creating a symlink requires a Windows privilege the test runner may not have")
	}
	const goMod = "module example.org/sharedname\n"
	const pkg = `{"name":"ignoredbygo","dependencies":{"alias":"npm:actual@1.0.0"}}`
	for _, reject := range []bool{false, true} {
		name := "shares the verified target bytes"
		if reject {
			name = "changed target refuses"
		}
		t.Run(name, func(t *testing.T) {
			ctx := t.Context()
			root, src := rebuildInputFixture(t)
			for _, leaf := range []struct{ key, contents string }{
				{"go.manifest", goMod}, {"npm.manifest", pkg}, {"a.go", "package main\n"}, {"main.ts", `import "alias/x"`},
			} {
				freezeProjectNameTestManifest(t, src, root, leaf.key, leaf.contents)
			}
			require.NoError(t, os.Symlink("go.manifest", filepath.Join(root, "go.mod")))
			require.NoError(t, os.Symlink("npm.manifest", filepath.Join(root, "package.json")))
			lease := t.TempDir()
			digest, err := writeRebuildInputSidecar(ctx, lease, func(emit func(rebuildSidecarRow) error) error {
				nsBudget := int64(rebuildCppRawLimit)
				epoch, err := freezeRebuildCheckoutInputs(ctx, src.FrozenRoot(), nil, &nsBudget,
					func(key string, _ fs.FileMode) (*rebuildInput, bool, error) {
						lang := map[string]string{"a.go": "go", "main.ts": "typescript"}[key]
						parse := lang != ""
						if !parse {
							lang = "manifest"
						}
						in, _, err := readRebuildInput(ctx, src, key, lang, 1024)
						return &in, parse, err
					}, emit)
				if err != nil {
					return err
				}
				if reject {
					require.NoError(t, os.WriteFile(filepath.Join(root, "go.manifest"), []byte("module example.org/editedname\n"), 0o600))
				}
				raw := int64(len(goMod) + len(pkg))
				selected, err := captureRebuildCheckoutResolverInputs(ctx, src, epoch, true, &raw, func(row rebuildSidecarRow) error {
					if row.Kind == rebuildGoManifestFact && string(row.Path) == "go.mod" {
						// The selector must use the exact bytes already read
						// and charged by the Go config path, not reopen them.
						require.NoError(t, os.Remove(filepath.Join(root, "go.manifest")))
					}
					return emit(row)
				})
				if !reject {
					require.NoError(t, err)
					require.Equal(t, "sharedname", selected)
					require.Zero(t, raw, "charge each logical manifest once")
				}
				return err
			})
			if reject {
				require.ErrorIs(t, err, ErrBuildInputChanged)
				require.Zero(t, digest)
				return
			}
			require.NoError(t, err)
			require.NoError(t, os.Remove(filepath.Join(root, "npm.manifest")))
			goFacts, _, err := openRebuildResolverConfigFacts(ctx, lease, digest)
			require.NoError(t, err)
			require.Equal(t, "example.org/sharedname", goFacts["go.mod"].RootClaim)
			require.Empty(t, goFacts["go.mod"].OwnerModule, "the native strict Go probe stops at this link")
			ns, manifests, followed, err := openRebuildNpmManifestFacts(ctx, lease, digest)
			require.NoError(t, err)
			require.NotZero(t, followed)
			require.Equal(t, followed, manifests["package.json"].Digest)
			lookup := newRebuildNpmLookup(ctx, rebuildNpmInputs{
				namespace: ns, manifests: manifests, followedRoot: followed, isFile: func(_ context.Context, key string) (bool, error) {
					return ns.probeFile(key)
				}, matchDirs: func(context.Context, string, int) ([]string, error) {
					t.Fatal("fixture declares no workspace globs")
					return nil, nil
				},
			})
			got, err := lookup.rewrite("main.ts", "alias/x")
			require.NoError(t, err)
			require.Equal(t, "actual/x", got)
		})
	}
}

func TestRebuildProjectNameRecipeRequest(t *testing.T) {
	ctx := t.Context()
	root, src := rebuildInputFixture(t)
	req := rebuildBuildRequest{
		Root: root, ParseDir: t.TempDir(), ParseLimit: 1024, Incarnation: 1,
		Bounds:  rebuildTransformBounds{CaptureBytes: 10, FileBytes: 1024},
		Witness: store_pebble.Witness{Stage: store_pebble.StageManifest, Input: store_pebble.HeadDigest{1}, Recipe: store_pebble.HeadDigest{2}},
	}
	bind := func(name string) rebuildBuildRequest {
		goMod := freezeProjectNameTestManifest(t, src, root, "go.mod", "module example.com/"+name+"\n")
		bound, err := captureProjectNameTest(t, src, map[string]*rebuildInput{"go.mod": goMod}, req)
		require.NoError(t, err)
		require.Equal(t, name, bound.ProjectName)
		return bound
	}
	bound := bind("Mixed\xffCase")
	_, err := captureProjectNameTest(t, src, nil, bound)
	require.ErrorIs(t, err, errRebuildBuildWire, "must not fold the name into the recipe twice")
	lease := t.TempDir()
	cfg := config.Default().Index
	require.ErrorIs(t, writeRebuildBuildRequest(ctx, lease, req, cfg, nil), errRebuildBuildWire, "unbound requests must refuse")
	require.NoError(t, writeRebuildBuildRequest(ctx, lease, bound, cfg, nil))

	// The staged parse root can stay the same when only a non-admitted
	// project-name manifest changes. The bound recipe must still distinguish it.
	changed := bind("AnotherName")
	require.Equal(t, bound.Witness.Input, changed.Witness.Input)
	require.NotEqual(t, bound.Witness.Recipe, changed.Witness.Recipe)
	require.NotEqual(t, bound.Witness.Version(), changed.Witness.Version())
	require.Equal(t, "AnotherName", search.DetectProjectName(root))
	got, _, err := readRebuildBuildRequest(ctx, lease)
	require.NoError(t, err)
	require.Equal(t, "Mixed\xffCase", got.ProjectName)
	require.Equal(t, req.Witness.Recipe, got.BaseRecipe)
	require.Equal(t, bound.Witness.Version(), got.Witness.Version())

	// A changed name with the old witness refuses at the owner and child
	// boundaries, without requiring the child to open the live source.
	bound.ProjectName = changed.ProjectName
	badLease := t.TempDir()
	badPath := filepath.Join(badLease, rebuildRequestName)
	require.ErrorIs(t, writeRebuildBuildRequest(ctx, badLease, bound, cfg, nil), errRebuildBuildWire)
	_, err = os.Stat(badPath)
	require.ErrorIs(t, err, os.ErrNotExist)
	f, err := os.Create(badPath)
	require.NoError(t, err)
	encodeErr := gob.NewEncoder(f).Encode(&bound)
	require.NoError(t, f.Close())
	require.NoError(t, encodeErr)
	_, inputs, err := readRebuildBuildRequest(ctx, badLease)
	require.ErrorIs(t, err, errRebuildBuildWire)
	require.Empty(t, inputs)
}
