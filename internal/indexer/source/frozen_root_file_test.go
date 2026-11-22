//go:build unix

package source

import (
	"context"
	"errors"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"golang.org/x/sys/unix"
)

func testFrozenRoot(t *testing.T, dir string) *os.Root {
	t.Helper()
	root, err := os.OpenRoot(dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = root.Close() })
	return root
}

func testFrozenSymlink(t *testing.T, dir, name, target string) {
	t.Helper()
	if err := os.Symlink(target, filepath.Join(dir, name)); err != nil {
		t.Fatal(err)
	}
}

func TestFrozenRootDirectoryLiteral(t *testing.T) {
	working := t.TempDir()
	regularWrite(t, working, "asset/data", "content is not inspected")
	regularWrite(t, working, "file", "")
	if err := os.Mkdir(filepath.Join(working, "empty"), 0o700); err != nil {
		t.Fatal(err)
	}
	testFrozenSymlink(t, working, "link", "asset")
	root := testFrozenRoot(t, working)
	got := map[string]fs.FileMode{}
	err := VisitFrozenDirectory(t.Context(), root, ".", func(name string, mode fs.FileMode) error {
		got[name] = mode
		return nil
	}, nil)
	if err != nil || len(got) != 4 || got["asset"] != fs.ModeDir || got["empty"] != fs.ModeDir || got["link"] != fs.ModeSymlink {
		t.Fatalf("literal listing = %v, %v", got, err)
	}
	if mode, ok := got["file"]; !ok || !mode.IsRegular() {
		t.Fatalf("missing regular file: %v", got)
	}
	reject := func(string, fs.FileMode) error {
		t.Fatal("unexpected child visit")
		return nil
	}
	if err := VisitFrozenDirectory(t.Context(), root, "empty", reject, nil); err != nil {
		t.Fatalf("empty directory did not close: %v", err)
	}
	if err := VisitFrozenDirectory(t.Context(), root, "link", reject, nil); !errors.Is(err, ErrFrozenRootPath) {
		t.Fatalf("link was not refused: %v", err)
	}
}

func TestFrozenRootDirectoryRefusesDrift(t *testing.T) {
	working := t.TempDir()
	regularWrite(t, working, "child/one", "")
	root := testFrozenRoot(t, working)
	var visited bool
	err := VisitFrozenDirectory(t.Context(), root, "child", func(string, fs.FileMode) error {
		visited = true
		return nil
	}, func() error {
		if !visited {
			t.Fatal("classified the directory before listing its entries")
		}
		return os.Rename(filepath.Join(working, "child"), filepath.Join(working, "moved"))
	})
	if !errors.Is(err, ErrFrozenRootChanged) {
		t.Fatalf("renamed directory closed without a drift refusal: %v", err)
	}
}

func TestFrozenRootLinkRechecksTargetAdmission(t *testing.T) {
	ctx := t.Context()
	working := t.TempDir()
	regularWrite(t, working, "allowed", "module example/allowed\n")
	testFrozenSymlink(t, working, "go.mod", "allowed")
	src, err := NewFilesystemSource(working)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = src.Close() })
	denied := errors.New("target not admitted")
	var visited []string
	obs, err := src.ResolveFrozenRootLink(ctx, "go.mod", func(string) error { return nil }, func(name string) (fs.FileMode, error) {
		visited = append(visited, name)
		if name != "allowed" {
			return 0, denied
		}
		return 0, nil
	})
	if err != nil || !slices.Equal(visited, []string{"allowed"}) {
		t.Fatalf("initial visit = %v, %v", visited, err)
	}
	if err := os.Remove(filepath.Join(working, "go.mod")); err != nil {
		t.Fatal(err)
	}
	// The new target is not even on disk. Admission must fail first.
	testFrozenSymlink(t, working, "go.mod", "denied")
	visited = nil
	data, err := obs.Read(ctx, 1024)
	if data != nil || !errors.Is(err, denied) || !errors.Is(err, ErrFrozenRootChanged) || !slices.Equal(visited, []string{"denied"}) {
		t.Fatalf("changed target admission = %v, %q, %v", visited, data, err)
	}
}

func TestFrozenRootPathReadRelocated(t *testing.T) {
	for _, name := range []string{"repo", "repo[1]"} {
		t.Run(name, func(t *testing.T) {
			dir := filepath.Join(t.TempDir(), name)
			regularWrite(t, dir, "a/b/child.h", "")
			regularWrite(t, dir, "a/go.mod", "module portable\n")
			regularWrite(t, dir, "go.mod", "module wrong\n")
			testFrozenSymlink(t, dir, "hop", "a/b")
			testFrozenSymlink(t, dir, "relative", "hop/../go.mod")
			testFrozenSymlink(t, dir, "absolute", dir+"/hop/../go.mod")
			testFrozenSymlink(t, dir, "chain", "absolute")
			testFrozenSymlink(t, dir, "dir-link", dir+"/hop/..")
			root := testFrozenRoot(t, dir)
			for _, name := range []string{"a/go.mod", "relative", "absolute", "chain", dir + "/hop/../go.mod"} {
				obs, err := ResolveFrozenRootPath(t.Context(), root, name)
				if err != nil || obs.Path() != "a/go.mod" {
					t.Fatalf("resolve %q = %q, %v", name, obs.Path(), err)
				}
				got, err := obs.Read(t.Context(), int64(len("module portable\n")))
				if string(got) != "module portable\n" || err != nil {
					t.Fatalf("read %q = %q, %v", name, got, err)
				}
			}
			obs, err := ResolveFrozenRootPath(t.Context(), root, "dir-link")
			if err != nil || obs.Path() != "a" {
				t.Fatalf("resolve directory = %q, %v", obs.Path(), err)
			}
			f, err := obs.OpenDir(t.Context())
			if err != nil {
				t.Fatal(err)
			}
			defer f.Close()
			entries, err := f.ReadDir(-1)
			var names []string
			for _, entry := range entries {
				names = append(names, entry.Name())
			}
			slices.Sort(names)
			if err != nil || !slices.Equal(names, []string{"b", "go.mod"}) {
				t.Fatalf("read directory = %v, %v", names, err)
			}
		})
	}
}

func TestFrozenRootPathReadPrefix(t *testing.T) {
	prefix := "#!/bin/sh\n" + strings.Repeat("x", 502)
	dir := t.TempDir()
	regularWrite(t, dir, "large", prefix+strings.Repeat("y", 1024))
	regularWrite(t, dir, "short", "#!")
	regularWrite(t, dir, "empty", "")
	root := testFrozenRoot(t, dir)
	ctx := t.Context()

	for _, tc := range []struct {
		name  string
		limit int64
		want  string
	}{
		{"large", 4, "#!/b"},
		{"short", 512, "#!"},
		{"empty", 512, ""},
	} {
		obs, err := ObserveFrozenLiteralRootPath(ctx, root, tc.name)
		if err != nil {
			t.Fatal(err)
		}
		got, err := obs.ReadPrefix(ctx, tc.limit)
		if err != nil || string(got) != tc.want {
			t.Fatalf("prefix for %s (limit %d) = %q, %v; want %q", tc.name, tc.limit, got, err, tc.want)
		}
	}

	obs, err := ObserveFrozenLiteralRootPath(ctx, root, "large")
	if err != nil {
		t.Fatal(err)
	}
	if got, err := obs.Read(ctx, 512); got != nil || !errors.Is(err, ErrRegularFileTooLarge) {
		t.Fatalf("ordinary full read of oversized file = %q, %v", got, err)
	}

	// A duplicate shares the offset even after ReadPrefix closes its fd.
	// Returning 512 bytes after reading the body would still fail this check.
	var offsetFD int
	got, err := obs.readPrefixWithOpen(ctx, 512, func(root *os.Root, name string) (*os.File, error) {
		f, err := openRootRegularFile(root, name)
		if err != nil {
			return nil, err
		}
		fd, err := unix.Dup(int(f.Fd()))
		if err != nil {
			_ = f.Close()
			t.Fatal(err)
		}
		unix.CloseOnExec(fd)
		t.Cleanup(func() { _ = unix.Close(fd) })
		offsetFD = fd
		return f, nil
	})
	if err != nil || string(got) != prefix {
		t.Fatalf("descriptor prefix = %q, %v", got, err)
	}
	if offset, err := unix.Seek(offsetFD, 0, io.SeekCurrent); err != nil || offset != 512 {
		t.Fatalf("descriptor consumed %d bytes, %v; want 512", offset, err)
	}
}

func TestFrozenRootPathReadPrefixRefusals(t *testing.T) {
	dir := t.TempDir()
	regularWrite(t, dir, "target", "not for a followed observation")
	testFrozenSymlink(t, dir, "link", "target")
	root := testFrozenRoot(t, dir)
	ctx := t.Context()
	link, err := ResolveFrozenRootPath(ctx, root, "link")
	if err != nil || link.Path() != "target" {
		t.Fatalf("linked observation = %q, %v", link.Path(), err)
	}
	directory, err := ObserveFrozenLiteralRootPath(ctx, root, ".")
	if err != nil {
		t.Fatal(err)
	}
	regular, err := ObserveFrozenLiteralRootPath(ctx, root, "target")
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name  string
		obs   FrozenRootPath
		limit int64
		want  error
	}{
		{"followed link", link, 512, ErrFrozenRootPath},
		{"directory", directory, 512, ErrNotRegularFile},
		{"missing observation", FrozenRootPath{}, 512, ErrFrozenRootPath},
		{"negative limit", regular, -1, ErrFrozenRootPath},
		{"exceeded limit", regular, 513, ErrFrozenRootPath},
	} {
		got, err := tc.obs.readPrefixWithOpen(ctx, tc.limit, func(*os.Root, string) (*os.File, error) {
			t.Fatalf("opened %s before refusing it", tc.name)
			return nil, nil
		})
		if got != nil || !errors.Is(err, tc.want) {
			t.Fatalf("prefix for %s = %q, %v; want %v", tc.name, got, err, tc.want)
		}
	}
}

func TestFrozenRootPathReadPrefixDrift(t *testing.T) {
	dir := t.TempDir()
	regularWrite(t, dir, "script", "#!/bin/sh\n")
	root := testFrozenRoot(t, dir)
	ctx := t.Context()
	obs, err := ObserveFrozenLiteralRootPath(ctx, root, "script")
	if err != nil {
		t.Fatal(err)
	}
	got, err := obs.readPrefixWithOpen(ctx, 512, func(root *os.Root, name string) (*os.File, error) {
		f, err := openRootRegularFile(root, name)
		if err != nil {
			return nil, err
		}
		// Keep the opened inode and metadata unchanged, but make the
		// current path reach it through a symlink.
		if err := root.Rename("script", "saved"); err != nil {
			_ = f.Close()
			t.Fatal(err)
		}
		if err := os.Symlink("saved", filepath.Join(dir, "script")); err != nil {
			_ = f.Close()
			t.Fatal(err)
		}
		return f, nil
	})
	if got != nil || !errors.Is(err, ErrFrozenRootChanged) || errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("prefix after path drift = %q, %v", got, err)
	}
}

func TestFrozenRootPathRefusals(t *testing.T) {
	base := t.TempDir()
	dir, outside := filepath.Join(base, "repo"), filepath.Join(base, "outside")
	regularWrite(t, dir, "sub/ok", "inside")
	regularWrite(t, outside, "ok", "outside")
	for name, target := range map[string]string{
		"external":          outside + "/ok",
		"relative-external": "../outside/ok",
		"reenter":           "../outside/../repo/sub/ok",
		"absolute-reenter":  dir + "/../outside/../repo/sub/ok",
		"alias-spelling":    base + "/./repo/sub/ok",
		"cycle-a":           "cycle-b",
		"cycle-b":           "cycle-a",
		"dangling":          "sub/not-there",
		"non-directory":     "sub/ok/../ok",
		"trailing-slash":    "sub/ok/",
		"followed-dir":      "sub",
		"special":           "fifo",
	} {
		testFrozenSymlink(t, dir, name, target)
	}
	if err := unix.Mkfifo(filepath.Join(dir, "fifo"), 0o600); err != nil {
		t.Fatal(err)
	}
	root := testFrozenRoot(t, dir)
	for _, tt := range []struct {
		name string
		want error
	}{
		{"external", ErrOutsideRoot}, {"relative-external", ErrOutsideRoot},
		{"reenter", ErrOutsideRoot}, {"absolute-reenter", ErrOutsideRoot}, {"alias-spelling", ErrOutsideRoot},
		{"cycle-a", ErrFrozenRootPath}, {"dangling", ErrFrozenRootPath},
		{"non-directory", ErrNotRegularFile}, {"trailing-slash", ErrNotRegularFile},
		{strings.Repeat("x", maxFrozenRootPathBytes+1), ErrFrozenRootPath},
	} {
		obs, err := ResolveFrozenRootPath(t.Context(), root, tt.name)
		if obs.Info() != nil || !errors.Is(err, tt.want) || errors.Is(err, fs.ErrNotExist) {
			t.Fatalf("path of %d bytes, want %v: observed %q, %v", len(tt.name), tt.want, obs.Path(), err)
		}
	}
	for _, name := range []string{"missing", "followed-dir/missing"} {
		obs, err := ResolveFrozenRootPath(t.Context(), root, name)
		if obs.Info() != nil || !errors.Is(err, fs.ErrNotExist) {
			t.Fatalf("resolve absent %s = %+v, %v", name, obs, err)
		}
	}
	for _, tt := range []struct {
		name string
		want error
	}{{"special", ErrNotRegularFile}, {"sub/ok", ErrRegularFileTooLarge}} {
		obs, err := ResolveFrozenRootPath(t.Context(), root, tt.name)
		if err != nil {
			t.Fatal(err)
		}
		data, err := obs.readWithOpen(t.Context(), 5, func(*os.Root, string) (*os.File, error) {
			t.Fatal("opened a refused entry")
			return nil, nil
		})
		if data != nil || !errors.Is(err, tt.want) {
			t.Fatalf("read refused %s = %q, %v; want %v", tt.name, data, err, tt.want)
		}
	}
}

func TestFrozenRootPathFileDrift(t *testing.T) {
	for _, change := range []string{"replace before read", "replace before open", "final symlink before open", "retarget link after open"} {
		t.Run(change, func(t *testing.T) {
			dir := t.TempDir()
			regularWrite(t, dir, "target", "old")
			regularWrite(t, dir, "other", "new")
			testFrozenSymlink(t, dir, "manifest", "target")
			root := testFrozenRoot(t, dir)
			obs, err := ResolveFrozenRootPath(t.Context(), root, "manifest")
			if err != nil {
				t.Fatal(err)
			}
			replace := func() {
				if err := root.Rename("target", "saved"); err != nil {
					t.Fatal(err)
				}
				if err := root.WriteFile("target", []byte("new"), obs.Info().Mode().Perm()); err != nil {
					t.Fatal(err)
				}
				mtime := obs.Info().ModTime()
				if err := root.Chtimes("target", mtime, mtime); err != nil {
					t.Fatal(err)
				}
			}
			if change == "replace before read" {
				replace()
			}
			data, err := obs.readWithOpen(t.Context(), 3, func(r *os.Root, name string) (*os.File, error) {
				switch change {
				case "replace before read":
					t.Fatal("opened an entry after its observation changed")
				case "replace before open":
					replace()
				case "final symlink before open":
					if err := root.Rename("target", "saved"); err != nil {
						t.Fatal(err)
					}
					testFrozenSymlink(t, dir, "target", "saved")
				}
				f, err := openRootRegularFile(r, name)
				if err == nil && change == "retarget link after open" {
					if err := root.Rename("manifest", "saved-link"); err != nil {
						_ = f.Close()
						t.Fatal(err)
					}
					testFrozenSymlink(t, dir, "manifest", "other")
				}
				return f, err
			})
			if data != nil || !errors.Is(err, ErrFrozenRootChanged) || errors.Is(err, fs.ErrNotExist) {
				t.Fatalf("read with drift = %q, %v", data, err)
			}
		})
	}
}

func TestFrozenRootPathDirDrift(t *testing.T) {
	dir := t.TempDir()
	regularWrite(t, dir, "include/child.h", "")
	root := testFrozenRoot(t, dir)
	obs, err := ResolveFrozenRootPath(t.Context(), root, "include")
	if err != nil {
		t.Fatal(err)
	}
	f, err := obs.openDirWithOpen(t.Context(), func(r *os.Root, name string) (*os.File, error) {
		if err := root.Rename("include", "saved"); err != nil {
			t.Fatal(err)
		}
		fifo := filepath.Join(dir, "include")
		if err := unix.Mkfifo(fifo, 0o600); err != nil {
			t.Fatal(err)
		}
		// Keep a writer open so even a regressed directory opener cannot hang.
		fd, err := unix.Open(fifo, unix.O_RDWR|unix.O_NONBLOCK|unix.O_CLOEXEC, 0)
		if err != nil {
			t.Fatal(err)
		}
		defer unix.Close(fd)
		file, err := openRootFrozenDir(r, name)
		if file != nil {
			_ = file.Close()
			t.Fatal("directory opener returned a FIFO descriptor")
		}
		return file, err
	})
	if f != nil || !errors.Is(err, ErrFrozenRootChanged) {
		t.Fatalf("open substituted directory = %v, %v", f, err)
	}
}

func TestFrozenRootPathCanceled(t *testing.T) {
	dir := t.TempDir()
	regularWrite(t, dir, "go.mod", "module inside\n")
	root := testFrozenRoot(t, dir)
	obs, err := ResolveFrozenRootPath(t.Context(), root, "go.mod")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	data, err := obs.Read(ctx, 64)
	if data != nil || !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled read = %q, %v", data, err)
	}
}

func TestFrozenRootPathKeepsHeldRoot(t *testing.T) {
	base := t.TempDir()
	dir := filepath.Join(base, "repo")
	regularWrite(t, dir, "go.mod", "module held\n")
	root := testFrozenRoot(t, dir)
	obs, err := ResolveFrozenRootPath(t.Context(), root, "go.mod")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(dir, filepath.Join(base, "moved")); err != nil {
		t.Fatal(err)
	}
	regularWrite(t, dir, "go.mod", "module rebound\n")
	data, err := obs.Read(t.Context(), 64)
	if string(data) != "module held\n" || err != nil {
		t.Fatalf("read from held root = %q, %v", data, err)
	}
}
