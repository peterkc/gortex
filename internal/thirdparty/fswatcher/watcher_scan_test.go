package fswatcher

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

type scanExcludeFilter struct {
	excluded   string
	includeDir bool
	calls      []string
	dirs       []string
}

func (f *scanExcludeFilter) ShouldInclude(path string) bool {
	f.calls = append(f.calls, path)
	return (f.includeDir && path == f.excluded) || (path != f.excluded && !isSubpath(f.excluded, path))
}

func (f *scanExcludeFilter) ShouldSkipDir(path string) bool {
	f.dirs = append(f.dirs, path)
	return path == f.excluded || isSubpath(f.excluded, path)
}

func newManualScanWatcher(t *testing.T, options ...WatcherOpt) *watcher {
	t.Helper()
	options = append(options, WithCooldown(MaxCooldownDuration), WithBufferSize(256))
	backend, err := New(options...)
	if err != nil {
		t.Fatal(err)
	}
	w := backend.(*watcher)
	t.Cleanup(w.aggregator.close)
	return w
}

func writeScanFile(t *testing.T, path string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, nil, 0o644); err != nil {
		t.Fatal(err)
	}
}

func assertScanEvents(t *testing.T, w *watcher, want ...string) {
	t.Helper()
	w.aggregator.close()
	got := make([]string, 0, len(w.events))
	for len(w.events) > 0 {
		event := <-w.events
		if !slices.Equal(event.Types, []EventType{EventMod}) {
			t.Errorf("event for %s has types %v, want Mod", event.Path, event.Types)
		}
		got = append(got, event.Path)
	}
	slices.Sort(got)
	slices.Sort(want)
	if !slices.Equal(got, want) {
		t.Fatalf("scan events = %v, want %v", got, want)
	}
}

func TestManualScanSkipsExcludedSubtrees(t *testing.T) {
	for _, scope := range []string{"global", "watch_path", "scan_root", "directory_event"} {
		t.Run(scope, func(t *testing.T) {
			// Given an excluded tree with many entries and an included sibling.
			root := t.TempDir()
			excluded := filepath.Join(root, "ignored")
			for i := range 64 {
				writeScanFile(t, filepath.Join(excluded, strings.Repeat("x", i+1)+".go"))
			}
			included := filepath.Join(root, "src", "keep.go")
			writeScanFile(t, included)
			filter := &scanExcludeFilter{excluded: excluded, includeDir: scope == "directory_event"}
			w := newManualScanWatcher(t, WithPath(root, WithPathFilter(filter)))
			if scope == "global" {
				w.filter = filter
				w.watchedPaths[root].filter = nil
			}
			scanRoot := root
			if scope == "scan_root" {
				scanRoot = excluded
			}

			// When the manual rescan runs synchronously.
			w.scanDirectory(scanRoot)

			// Then no descendant of the excluded directory reaches event filtering.
			for _, path := range append(filter.calls, filter.dirs...) {
				if isSubpath(excluded, path) {
					t.Fatalf("walk reached excluded descendant %s", path)
				}
			}
			switch scope {
			case "scan_root":
				assertScanEvents(t, w)
			case "directory_event":
				assertScanEvents(t, w, excluded, filepath.Dir(included), included)
			default:
				assertScanEvents(t, w, filepath.Dir(included), included)
			}
		})
	}
}

func TestManualScanPreservesNestedWatchUnderExcludedParent(t *testing.T) {
	// Given a more specific watch below an excluded ancestor.
	root := t.TempDir()
	excluded := filepath.Join(root, "ignored")
	nested := filepath.Join(excluded, "deep", "repo")
	keep := filepath.Join(nested, "src", "keep.go")
	writeScanFile(t, keep)
	writeScanFile(t, filepath.Join(excluded, "other", "drop.go"))
	filter := &scanExcludeFilter{excluded: excluded}
	w := newManualScanWatcher(t, WithPath(root, WithPathFilter(filter)), WithPath(nested))

	// When only the parent receives a rescan, including on a repeat scan.
	w.scanDirectory(root)
	w.scanDirectory(excluded)

	// Then the nested watch still recovers its entries without visiting ignored siblings.
	for _, path := range append(filter.calls, filter.dirs...) {
		if strings.Contains(path, string(filepath.Separator)+"other"+string(filepath.Separator)) {
			t.Fatalf("walk reached ignored sibling %s", path)
		}
	}
	assertScanEvents(t, w, nested, filepath.Dir(keep), keep)
}

func TestManualScanPreservesIncludedEvents(t *testing.T) {
	for _, trailingSlash := range []bool{false, true} {
		t.Run(map[bool]string{false: "clean", true: "trailing_slash"}[trailingSlash], func(t *testing.T) {
			// Given an included recursive tree and a directory symlink.
			root := t.TempDir()
			file := filepath.Join(root, "src", "keep.go")
			writeScanFile(t, file)
			link := filepath.Join(root, "linked")
			if err := os.Symlink(filepath.Dir(file), link); err != nil {
				t.Skipf("symlinks unavailable: %v", err)
			}
			w := newManualScanWatcher(t, WithPath(root))
			scanRoot := root
			if trailingSlash {
				scanRoot += string(filepath.Separator)
			}

			// When the manual scan runs.
			w.scanDirectory(scanRoot)

			// Then included entries retain their Mod events and symlinks are not followed.
			assertScanEvents(t, w, filepath.Dir(file), file, link)
		})
	}
}

func TestManualScanDoesNotPruneEventOnlyRegexFilters(t *testing.T) {
	for _, scope := range []string{"global", "watch_path"} {
		t.Run(scope, func(t *testing.T) {
			// Given regex filters that reject a directory name but admit its child.
			root := t.TempDir()
			file := filepath.Join(root, "x.log", "keep.go")
			writeScanFile(t, file)
			options := []WatcherOpt{WithPath(root, WithPathIncRegex(`\.go$`), WithPathExcRegex(`\.log$`))}
			if scope == "global" {
				options = []WatcherOpt{WithPath(root), WithIncRegex(`\.go$`), WithExcRegex(`\.log$`)}
			}
			w := newManualScanWatcher(t, options...)

			// When a manual scan reaches the directory.
			w.scanDirectory(root)

			// Then an event-only filter must not hide the included child.
			assertScanEvents(t, w, file)
		})
	}
}

func TestManualScanPreservesTopLevelDepth(t *testing.T) {
	// Given a top-level watch with a recursive directory below it.
	root := t.TempDir()
	file := filepath.Join(root, "src", "keep.go")
	writeScanFile(t, file)
	w := newManualScanWatcher(t, WithPath(root, WithDepth(WatchTopLevel)))

	// When the manual scan runs.
	w.scanDirectory(root)

	// Then the child directory emits its event, but its contents do not.
	assertScanEvents(t, w, filepath.Dir(file))
}

func TestManualScanStopsOnCancellationAndShutdown(t *testing.T) {
	for _, stop := range []string{"cancel_before", "cancel_during", "shutdown_before", "shutdown_during"} {
		t.Run(stop, func(t *testing.T) {
			// Given a tree whose rescan can be stopped before or during traversal.
			root := t.TempDir()
			writeScanFile(t, filepath.Join(root, "a.go"))
			writeScanFile(t, filepath.Join(root, "b.go"))
			w := newManualScanWatcher(t, WithPath(root))
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			w.scanCtx = ctx
			switch stop {
			case "cancel_before":
				cancel()
			case "shutdown_before":
				w.isShuttingDown.Store(true)
			default:
				w.aggregator.beforeAddLock = func() {
					if stop == "cancel_during" {
						cancel()
					} else {
						w.isShuttingDown.Store(true)
					}
				}
			}

			// When the rescan runs and observes the stop signal.
			w.scanDirectory(root)

			// Then no later entries are emitted.
			if strings.HasSuffix(stop, "before") {
				assertScanEvents(t, w)
			} else {
				assertScanEvents(t, w, filepath.Join(root, "a.go"))
			}
		})
	}
}
