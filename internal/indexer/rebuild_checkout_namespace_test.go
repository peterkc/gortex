//go:build unix

package indexer

import (
	"encoding/hex"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/zzet/gortex/internal/config"
	"github.com/zzet/gortex/internal/excludes"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/indexer/merkle"
	"github.com/zzet/gortex/internal/indexer/source"
	"github.com/zzet/gortex/internal/parser"
	"github.com/zzet/gortex/internal/parser/languages"
	"go.uber.org/zap"
)

func TestRebuildCheckoutNamespaceCapture(t *testing.T) {
	var first rebuildDigest
	for _, basename := range []string{"checkout", "checkout[1]"} {
		t.Run(basename, func(t *testing.T) {
			ctx := t.Context()
			ok := []string{".gortexignore", "pkg/.rgignore", "pkg/Foo.ts", "go.mod", "unadmitted.ts"}
			work := filepath.Join(t.TempDir(), basename)
			for _, dir := range []string{".Git", "empty", "ignored", "node_modules", "pkg"} {
				require.NoError(t, os.MkdirAll(filepath.Join(work, dir), 0o700))
			}
			const outerRules = "ignored/\nblocked.ts\n"
			const innerRules = "nested.ts\n"
			for rel, data := range map[string]string{
				".gortexignore": outerRules, "pkg/.rgignore": innerRules, "pkg/Foo.ts": "export {}", "go.mod": "module example.com/demo",
				"blocked.ts": "ignored", "pkg/nested.ts": "ignored", "unadmitted.ts": "not admitted",
			} {
				require.NoError(t, os.WriteFile(filepath.Join(work, rel), []byte(data), 0o600))
			}
			// Refusal if read, but none of these directory entries may be opened.
			for _, dir := range []string{".Git", "ignored", "node_modules"} {
				require.NoError(t, os.Symlink("../../outside", filepath.Join(work, dir, ".ignore")))
			}
			root, err := os.OpenRoot(work)
			require.NoError(t, err)
			t.Cleanup(func() { require.NoError(t, root.Close()) })
			var admitted []string
			lease := t.TempDir()
			raw := int64(rebuildCppRawLimit)
			var captured *rebuildResolverNamespace
			var charged int64
			digest, err := writeRebuildInputSidecar(ctx, lease, func(emit func(rebuildSidecarRow) error) error {
				ns, err := captureRebuildCheckoutNamespace(ctx, root, excludes.New([]string{"node_modules/"}), nil, &raw,
					func(rel string, mode fs.FileMode, _ rebuildGitMember) (bool, error) {
						require.True(t, mode.IsRegular())
						admitted = append(admitted, rel)
						return rel != "unadmitted.ts", nil
					}, func(row rebuildSidecarRow) error {
						charged += int64(len(row.Path)) + 1
						return emit(row)
					})
				captured = ns
				return err
			})
			require.NoError(t, err)
			require.ElementsMatch(t, ok, admitted)
			require.Equal(t, int64(rebuildCppRawLimit)-charged-int64(len(outerRules)+len(innerRules)), raw)
			if first == (rebuildDigest{}) {
				first = digest
			} else {
				require.Equal(t, first, digest)
			}
			require.NoError(t, os.Remove(filepath.Join(work, "pkg", "Foo.ts")))
			replay, err := openRebuildResolverNamespace(ctx, lease, digest)
			require.NoError(t, err)
			require.Equal(t, captured.entries, replay.entries)
			for rel, kind := range map[string]rebuildNamespaceKind{
				"pkg/Foo.ts": rebuildNamespaceRegular, "pkg/foo.ts": rebuildNamespaceAbsent, "empty/none.ts": rebuildNamespaceAbsent,
			} {
				got, err := replay.lookup(rel)
				require.NoError(t, err, "%q", rel)
				require.Equal(t, kind, got, "%q", rel)
			}
			for _, rel := range []string{".Git/.ignore", "ignored/.ignore", "node_modules/.ignore", "blocked.ts", "pkg/nested.ts", "unadmitted.ts"} {
				_, err := replay.lookup(rel)
				require.ErrorIs(t, err, errRebuildResolverInputDenied, "%q", rel)
			}
		})
	}
}

func TestFreezeRebuildCheckoutInputs(t *testing.T) {
	for _, bad := range []bool{false, true} {
		name := "parse root excludes resolver-only input"
		if bad {
			name = "mismatched identity refuses"
		}
		t.Run(name, func(t *testing.T) {
			ctx := t.Context()
			root, src := rebuildInputFixture(t)
			require.NoError(t, os.Mkdir(filepath.Join(root, "ignored"), 0o700))
			require.NoError(t, os.WriteFile(filepath.Join(root, "ignored", "child.ts"), nil, 0o600))
			require.NoError(t, os.WriteFile(filepath.Join(root, "main.ts"), []byte("export {}"), 0o600))
			require.NoError(t, os.WriteFile(filepath.Join(root, "go.mod"), []byte("module example.org/demo\n"), 0o600))
			require.NoError(t, os.Symlink("main.ts", filepath.Join(root, "alias.ts")))
			raw := int64(rebuildCppRawLimit)
			var keys []string
			out, err := freezeRebuildCheckoutInputs(ctx, src.FrozenRoot(), excludes.New([]string{"ignored/"}), nil, &raw,
				func(key string, _ fs.FileMode, _ rebuildGitMember, _ *int64, _ func(rebuildSidecarRow) error) (rebuildCheckoutLeaf, error) {
					keys = append(keys, key)
					lang, parse := "typescript", key != "go.mod"
					if !parse {
						lang = "manifest"
					}
					in, _, err := readRebuildInput(ctx, src, key, lang, 1024)
					if bad && key == "alias.ts" {
						in.Kind = merkle.RegularFile
					}
					return rebuildCheckoutLeaf{input: &in, parse: parse}, err
				}, func(rebuildSidecarRow) error {
					require.False(t, bad, "no rows from a failed root classification")
					return nil
				})
			if bad {
				require.ErrorIs(t, err, ErrBuildInputChanged)
				require.Empty(t, out)
				return
			}
			require.NoError(t, err)
			require.Equal(t, []string{"alias.ts", "go.mod", "main.ts"}, keys)
			require.Len(t, out.admitted, 3)
			require.Len(t, out.files, 2)
			require.Equal(t, merkle.Symlink, out.admitted["alias.ts"].Kind)
			want, err := rebuildInputRoot([]rebuildInput{*out.admitted["main.ts"], *out.admitted["alias.ts"]})
			require.NoError(t, err)
			require.Equal(t, want, hex.EncodeToString(out.digest[:]))
			_, err = out.namespace.lookup("ignored/child.ts")
			require.ErrorIs(t, err, errRebuildResolverInputDenied)
			_, err = out.namespace.lookup("alias.ts")
			require.ErrorIs(t, err, errRebuildResolverNamespace, "a parse link is not resolver file authority")
		})
	}
}

func TestRebuildCheckoutNamespacePolicyRefusal(t *testing.T) {
	for _, name := range []string{"outside symlink", "over raw limit"} {
		t.Run(name, func(t *testing.T) {
			parent := t.TempDir()
			work := filepath.Join(parent, "repo")
			require.NoError(t, os.Mkdir(work, 0o700))
			raw := int64(rebuildCppRawLimit)
			var cause error
			ignore := filepath.Join(work, ".gortexignore")
			if name == "outside symlink" {
				require.NoError(t, os.WriteFile(filepath.Join(parent, "outside"), []byte("foo.ts\n"), 0o600))
				require.NoError(t, os.Symlink("../outside", ignore))
				cause = source.ErrNotRegularFile
			} else {
				require.NoError(t, os.WriteFile(ignore, make([]byte, 1024), 0o600))
				raw = 100
				cause = errRebuildCppCaptureLimit
			}
			root, err := os.OpenRoot(work)
			require.NoError(t, err)
			t.Cleanup(func() { require.NoError(t, root.Close()) })
			ns, err := captureRebuildCheckoutNamespace(t.Context(), root, nil, nil, &raw, func(string, fs.FileMode, rebuildGitMember) (bool, error) {
				t.Fatal("the owner consumed an unproved ignore policy")
				return false, nil
			}, func(rebuildSidecarRow) error {
				t.Fatal("emitted an unproved listing")
				return nil
			})
			require.ErrorIs(t, err, errRebuildIgnorePolicy)
			require.ErrorIs(t, err, cause)
			require.Nil(t, ns)
		})
	}
}

func TestRebuildCheckoutNestedMarker(t *testing.T) {
	for _, late := range []bool{false, true} {
		name := "opaque without following marker"
		if late {
			name = "appeared after parent listing"
		}
		t.Run(name, func(t *testing.T) {
			dir, src := rebuildInputFixture(t)
			require.NoError(t, os.Mkdir(filepath.Join(dir, "nested"), 0o700))
			require.NoError(t, os.WriteFile(filepath.Join(dir, "nested", "source.go"), nil, 0o600))
			marker := filepath.Join(dir, "nested", ".git")
			if !late {
				require.NoError(t, os.Symlink("../absent", marker))
			}
			raw := int64(rebuildCppRawLimit)
			ns, err := captureRebuildCheckoutNamespace(t.Context(), src.FrozenRoot(), nil, nil, &raw,
				func(string, fs.FileMode, rebuildGitMember) (bool, error) {
					t.Fatal("opened a leaf in a nested checkout")
					return false, nil
				}, func(row rebuildSidecarRow) error {
					if late && row.Kind == rebuildNamespaceDirectoryFact && string(row.Path) == "nested" {
						require.NoError(t, os.Symlink("../absent", marker))
					}
					return nil
				})
			if late {
				require.ErrorIs(t, err, ErrBuildInputChanged)
				require.Nil(t, ns)
				return
			}
			require.NoError(t, err)
			_, err = ns.lookup("nested/source.go")
			require.ErrorIs(t, err, errRebuildResolverInputDenied)
		})
	}
}

func TestFreezeRebuildCheckoutRegularLeaves(t *testing.T) {
	ctx := t.Context()
	dir, src := rebuildInputFixture(t)
	require.NoError(t, os.WriteFile(filepath.Join(dir, "main.go"), []byte("package demo\n"), 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "large.go"), []byte(strings.Repeat("x", 129)), 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "plain.b2raw"), []byte("not a source file\n"), 0o600))
	require.NoError(t, os.Mkdir(filepath.Join(dir, "denied"), 0o700))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "denied", "hidden.go"), nil, 0o600))
	cfg := config.Default().Index
	cfg.MaxFileSize = 128
	reg := parser.NewRegistry()
	reg.Register(languages.NewGoExtractor())
	idx := New(graph.New(), reg, cfg, zap.NewNop())
	t.Cleanup(idx.Close)
	gate := idx.newContentAdmissionGate()
	raw := int64(rebuildCppRawLimit)
	var classified []string
	epoch, err := freezeRebuildCheckoutInputs(ctx, src.FrozenRoot(), excludes.New([]string{"denied/"}), nil, &raw,
		func(key string, mode fs.FileMode, member rebuildGitMember, raw *int64, emit func(rebuildSidecarRow) error) (rebuildCheckoutLeaf, error) {
			return idx.freezeRebuildCheckoutLeaf(ctx, src.FrozenRoot(), key, mode, member, gate, 128, raw, emit)
		}, func(row rebuildSidecarRow) error {
			if row.Kind == rebuildClassificationRow {
				classified = append(classified, string(row.Path))
			}
			return nil
		})
	require.NoError(t, err)
	require.Equal(t, []string{"large.go", "plain.b2raw"}, classified)
	require.Len(t, epoch.files, 1)
	require.Len(t, epoch.admitted, 1)
	require.Equal(t, "main.go", epoch.files[0].Path)
	require.Equal(t, epoch.files[0], *epoch.admitted["main.go"])
	for _, key := range classified {
		present, err := epoch.namespace.probeFile(key)
		require.NoError(t, err)
		require.True(t, present, "type-only evidence for %q", key)
		require.NotContains(t, epoch.admitted, key)
	}
	_, err = epoch.namespace.lookup("denied/hidden.go")
	require.ErrorIs(t, err, errRebuildResolverInputDenied)
	bad, err := freezeRebuildCheckoutInputs(ctx, src.FrozenRoot(), excludes.New([]string{"denied/"}), nil, &raw,
		func(string, fs.FileMode, rebuildGitMember, *int64, func(rebuildSidecarRow) error) (rebuildCheckoutLeaf, error) {
			return rebuildCheckoutLeaf{typeOnly: true, parse: true}, nil
		}, func(rebuildSidecarRow) error {
			t.Fatal("emitted a row after contradictory type-only proof")
			return nil
		})
	require.ErrorIs(t, err, ErrBuildInputChanged)
	require.Empty(t, bad)
}
