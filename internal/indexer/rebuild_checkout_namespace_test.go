//go:build unix

package indexer

import (
	"io/fs"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/zzet/gortex/internal/excludes"
	"github.com/zzet/gortex/internal/indexer/source"
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
				ns, err := captureRebuildCheckoutNamespace(ctx, root, excludes.New([]string{"node_modules/"}), &raw,
					func(rel string, mode fs.FileMode) (bool, error) {
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
			ns, err := captureRebuildCheckoutNamespace(t.Context(), root, nil, &raw, func(string, fs.FileMode) (bool, error) {
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
