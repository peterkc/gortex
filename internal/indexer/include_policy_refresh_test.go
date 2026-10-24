package indexer

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/zzet/gortex/internal/config"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
)

func TestIncludePolicyRefreshLifecycle(t *testing.T) {
	for _, scenario := range []string{"add_retrack", "remove_retrack", "add_reload"} {
		t.Run(scenario, func(t *testing.T) {
			root := t.TempDir()
			require.NoError(t, os.Mkdir(filepath.Join(root, ".git"), 0755))
			target := filepath.Join(root, "vendor/acme/keep.go")
			writeExcludeFixture(t, target, "package app\nfunc LiveIncluded() {}\n")
			cfgPath := filepath.Join(root, ".gortex.yaml")
			body := "include:\n  - vendor/acme/keep.go\n"
			if scenario == "remove_retrack" {
				writeFile(t, cfgPath, body)
			}
			mi, w, reconciled := newGitignoreRefreshWatcher(t, root, "", "")
			idx := mi.GetIndexer("app")
			before := idx.excludeMatcher()
			watchBefore := w.currentExcludeMatcher()
			_, err := mi.IncrementalReindexRepo("app", nil)
			require.NoError(t, err)
			require.Same(t, before, idx.excludeMatcher(), "unchanged producer policy reuses matcher")
			require.Same(t, watchBefore, w.currentExcludeMatcher(), "unchanged watcher policy reuses matcher")
			select {
			case err := <-reconciled:
				t.Fatalf("unchanged policy reconciled: %v", err)
			default:
			}
			if scenario == "remove_retrack" {
				require.NoError(t, os.Remove(cfgPath))
			} else {
				writeFile(t, cfgPath, body)
			}
			if scenario == "add_reload" {
				mi.RefreshRepoConfigs()
			} else {
				_, err = mi.TrackRepoCtx(context.Background(), config.RepoEntry{Path: root, Name: "app"})
				require.NoError(t, err)
			}
			_, err = mi.IncrementalReindexRepo("app", nil)
			require.NoError(t, err)
			want := scenario != "remove_retrack"
			require.Equal(t, want, len(mi.graph.FindNodesByName("LiveIncluded")) > 0, "producer membership after include change")
			require.Equal(t, want, (&watcherExcludeFilter{w: w}).ShouldInclude(target), "backend must refresh includes")
			require.Equal(t, !want, w.isExcluded(target), "semantic watcher must refresh includes")
			awaitGitignoreReconcile(t, reconciled)
			writeFile(t, target, "package app\nfunc LiveIncludedEdited() {}\n")
			if want {
				require.NoError(t, w.patchGraphAfterReceiptCheck(target, ChangeModified))
				require.NotEmpty(t, mi.graph.FindNodesByName("LiveIncludedEdited"), "later save reaches the graph")
			} else {
				ticket, err := w.EnqueueFileMutation(context.Background(), target)
				require.NoError(t, err)
				require.Nil(t, ticket, "removed include cannot acquire a mutation")
			}
			before = idx.excludeMatcher()
			watchBefore = w.currentExcludeMatcher()
			_, err = mi.IncrementalReindexRepo("app", nil)
			require.NoError(t, err)
			require.Same(t, before, idx.excludeMatcher())
			require.Same(t, watchBefore, w.currentExcludeMatcher())
		})
	}
}

func TestSourceSearchIncludeCompilationHoldsLock(t *testing.T) {
	for _, buffer := range []bool{false, true} {
		t.Run(map[bool]string{false: "disk", true: "buffer"}[buffer], func(t *testing.T) {
			root := t.TempDir()
			idx := newAdmissionTestIndexer(t, root)
			idx.logger = nil
			idx.SetIgnorePatterns([]string{"vendor/"}, []string{"*.go"})
			idx.excludeMatcher()
			warnings := 0
			// A real warning runs during compilation, before any filesystem walk.
			core := zap.NewExample().Core()
			idx.logger = zap.New(core, zap.Hooks(func(e zapcore.Entry) error {
				warnings++
				if idx.excludeMu.TryLock() {
					idx.excludeMu.Unlock()
					t.Error("include compilation read config without excludeMu")
				}
				return nil
			}))
			writeFile(t, filepath.Join(root, "main.go"), "package app\n")
			if buffer {
				require.True(t, idx.AdmitSourceSearchBuffer(context.Background(), root, filepath.Join(root, "main.go"), []byte("package app\n"), []string{"vendor/"}))
			} else {
				files, err := idx.CurrentSourceSearchFiles(context.Background(), root, nil, nil, 0, []string{"vendor/"})
				require.NoError(t, err)
				require.Equal(t, []string{"main.go"}, files)
			}
			require.Positive(t, warnings)
		})
	}
}

// Pin the real WalkDir contract rather than synthesizing an impossible Type().
func TestWalkDiskRejectsEscapingSymlink(t *testing.T) {
	root := t.TempDir()
	outside := filepath.Join(t.TempDir(), "outside.go")
	writeFile(t, outside, "package outside\nfunc Escaped() {}\n")
	link := filepath.Join(root, "escape.go")
	require.NoError(t, os.Symlink(outside, link))
	idx := newAdmissionTestIndexer(t, root)
	seen := false
	require.NoError(t, idx.walkDisk(root, root, func(path string, d os.DirEntry, err error) error {
		require.NoError(t, err)
		if path == link {
			seen = true
			require.NotZero(t, d.Type()&os.ModeSymlink)
			require.False(t, idx.admitFileInOpenDir(root, path, -1, d.Type()).admit)
		}
		return nil
	}))
	require.True(t, seen)
}

func TestExcludeMatcherIncludeOnlyRefresh(t *testing.T) {
	root := t.TempDir()
	idx := newAdmissionTestIndexer(t, root)
	patterns := idx.config.Exclude
	before := idx.excludeMatcher()
	includes := []string{"vendor/keep.go"}
	idx.refreshEffectiveExcludePatterns(patterns, includes)
	require.NotSame(t, before, idx.excludeMatcher())
	require.True(t, idx.excludeMatcher().IncludesRel("vendor/keep.go"))
	current := idx.excludeMatcher()
	idx.refreshEffectiveExcludePatterns(patterns, includes)
	require.Same(t, current, idx.excludeMatcher())
	idx.refreshEffectiveExcludePatterns(patterns, nil)
	require.False(t, idx.excludeMatcher().IncludesRel("vendor/keep.go"))
}
