//go:build unix

package indexer

import (
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/zzet/gortex/internal/excludes"
	"github.com/zzet/gortex/internal/gitcmd"
)

func TestRebuildGitCheckoutNativeParity(t *testing.T) {
	// The production queries inherit this environment too. Keep any caller's
	// Git config, repository selectors and default excludes out of both arms.
	for _, env := range os.Environ() {
		key, _, _ := strings.Cut(env, "=")
		if strings.HasPrefix(key, "GIT_") {
			t.Setenv(key, "")
			require.NoError(t, os.Unsetenv(key))
		}
	}
	builderIsolateGit(t)
	common := builderTempDir(t, "common")
	base := filepath.Dir(common)
	t.Setenv("HOME", base)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(base, "xdg"))
	ctx := t.Context()
	git := func(dir string, args ...string) string {
		t.Helper()
		out, err := gitcmd.Run(ctx, dir, args...)
		require.NoError(t, err, "git %q: %s", args, out)
		return string(out)
	}
	put := func(dir, rel, data string) {
		t.Helper()
		file := filepath.Join(dir, filepath.FromSlash(rel))
		require.NoError(t, os.MkdirAll(filepath.Dir(file), 0o700))
		require.NoError(t, os.WriteFile(file, []byte(data), 0o600))
	}

	// Use an actual linked worktree so info/exclude is in the common Git
	// directory, not beside the index selected for this checkout.
	git(common, "init", "--quiet", "--template=")
	git(common, "commit", "--quiet", "--allow-empty", "--no-gpg-sign", "-m", "fixture")
	put(common, ".git/info/exclude", "/info.ts\n")
	put(base, "user-excludes", "/user.ts\n")
	git(common, "config", "core.excludesFile", filepath.Join(base, "user-excludes"))
	dir := filepath.Join(base, "checkout")
	git(common, "worktree", "add", "--quiet", "--detach", dir, "HEAD")

	const rawName = "pkg/raw name\n.ts"
	put(dir, ".gitignore", "/blocked/\n!/blocked/reinclude.ts\n/ignored-tracked/\ndir-only/\n/tracked.tmp\n")
	put(dir, "pkg/.gitignore", "*.tmp\n!keep.tmp\n")
	for _, rel := range []string{
		"blocked/reinclude.ts", "dir-only/hidden.ts", "pkg/dir-only",
		"ignored-tracked/deep/tracked.ts", "ignored-tracked/deep/untracked.ts", "tracked.tmp",
		"pkg/drop.tmp", "pkg/keep.tmp", "root.tmp", rawName, "info.ts", "user.ts",
		"gortex-only/local.ts", "gitlink/local.ts", "nested/tracked.ts", "nested/untracked.ts",
	} {
		put(dir, rel, "fixture\n")
	}
	git(dir, "add", "-f", "--", "tracked.tmp", "ignored-tracked/deep/tracked.ts", "nested/tracked.ts")
	oid := strings.TrimSpace(git(common, "rev-parse", "HEAD"))
	git(dir, "update-index", "--add", "--cacheinfo", "160000,"+oid+",gitlink")
	nested := filepath.Join(dir, "nested")
	git(nested, "init", "--quiet", "--template=")
	git(nested, "add", "--", "tracked.ts") // Tracked in both indexes, but opaque to the outer checkout.

	wire, ended := strings.CutSuffix(git(dir, "ls-files", "-z", "--cached", "--others", "--exclude-standard"), "\x00")
	require.True(t, ended, "native path list is not NUL-terminated")
	native := strings.Split(wire, "\x00")
	for _, rel := range []string{"gortex-only/local.ts", "gitlink", "nested/tracked.ts"} {
		require.Contains(t, native, rel, "the fixture must exercise this exclusion")
	}
	var want []string
	for _, rel := range native {
		// The fixture's configured Gortex exclusion and the two §8
		// boundaries are the only adjustments to Git's file list.
		if rel == "gitlink" || strings.HasPrefix(rel, "gitlink/") ||
			rel == "nested" || strings.HasPrefix(rel, "nested/") || strings.HasPrefix(rel, "gortex-only/") {
			continue
		}
		want = append(want, rel)
	}
	wantTracked := []string{"ignored-tracked/deep/tracked.ts", "tracked.tmp"}
	require.ElementsMatch(t, []string{
		".gitignore", "ignored-tracked/deep/tracked.ts", "pkg/.gitignore", "pkg/dir-only",
		"pkg/keep.tmp", rawName, "root.tmp", "tracked.tmp",
	}, want, "native fixture membership")

	root, err := os.OpenRoot(dir)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, root.Close()) })
	raw := int64(rebuildCppRawLimit)
	inventory, err := readRebuildGitInventory(ctx, root, &raw)
	require.NoError(t, err)
	var got, gotTracked []string
	ns, err := captureRebuildCheckoutNamespace(ctx, root, excludes.New([]string{"gortex-only/"}), &inventory, &raw,
		func(rel string, mode fs.FileMode, member rebuildGitMember) (bool, error) {
			require.True(t, mode.IsRegular(), "%q", rel)
			if member == rebuildGitTracked {
				gotTracked = append(gotTracked, rel)
			} else {
				require.Equal(t, rebuildGitUntracked, member, "%q", rel)
			}
			got = append(got, rel)
			return true, nil
		}, func(row rebuildSidecarRow) error {
			key := strings.TrimSuffix(string(row.Path), "/")
			for _, opaque := range []string{"blocked/", "dir-only/", "gortex-only/", "gitlink/", "nested/"} {
				require.False(t, strings.HasPrefix(key, opaque), "recorded %q below opaque %q", key, opaque)
			}
			return nil
		})
	require.NoError(t, err)
	require.ElementsMatch(t, want, got, "admitted leaf callbacks versus native Git")
	require.ElementsMatch(t, wantTracked, gotTracked, "tracked leaf callbacks")
	require.NotNil(t, ns)
	for _, rel := range []string{"blocked/reinclude.ts", "ignored-tracked/deep/untracked.ts", "gitlink/local.ts", "nested/tracked.ts"} {
		_, err := ns.lookup(rel)
		require.ErrorIs(t, err, errRebuildResolverInputDenied, "%q", rel)
	}
	kind, err := ns.lookup("pkg/missing.ts")
	require.NoError(t, err)
	require.Equal(t, rebuildNamespaceAbsent, kind)
}
