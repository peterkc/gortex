package search

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

// TestProjectNameDeWeight proves project-name detection (go.mod / package.json /
// dir, with the length floor) and that the detected name is stripped from the
// INDEXED path so it no longer contributes a uniform path-field boost — the
// de-weighting that composes with the rerank rather than flattening it.
func TestProjectNameDeWeight(t *testing.T) {
	t.Run("detect_from_go_mod", func(t *testing.T) {
		dir := t.TempDir()
		mustWrite(t, filepath.Join(dir, "go.mod"), "module github.com/acme/gortex\n\ngo 1.24\n")
		if got := DetectProjectName(dir); got != "gortex" {
			t.Errorf("DetectProjectName=%q, want gortex", got)
		}
	})

	t.Run("detect_from_package_json", func(t *testing.T) {
		dir := t.TempDir()
		mustWrite(t, filepath.Join(dir, "package.json"), `{"name": "@acme/widgets", "version": "1.0.0"}`)
		if got := DetectProjectName(dir); got != "widgets" {
			t.Errorf("DetectProjectName=%q, want widgets (scoped name de-scoped)", got)
		}
	})

	t.Run("short_name_disabled", func(t *testing.T) {
		dir := t.TempDir()
		mustWrite(t, filepath.Join(dir, "go.mod"), "module github.com/x/cli\n")
		if got := DetectProjectName(dir); got != "" {
			t.Errorf("DetectProjectName=%q, want \"\" (below the %d-char floor)", got, projectNameMinLen)
		}
	})

	t.Run("strip_project_name_segment", func(t *testing.T) {
		// The repo-root segment is dropped; the rest of the path is intact.
		if got := StripProjectNameFromPath("gortex/internal/auth/token.go", "gortex"); got != "internal/auth/token.go" {
			t.Errorf("StripProjectNameFromPath=%q, want internal/auth/token.go", got)
		}
		// A path that does not contain the project name is unchanged.
		if got := StripProjectNameFromPath("internal/auth/token.go", "gortex"); got != "internal/auth/token.go" {
			t.Errorf("unchanged path mutated: %q", got)
		}
		// A short / empty project name never strips.
		if got := StripProjectNameFromPath("cli/main.go", "cli"); got != "cli/main.go" {
			t.Errorf("short project name should not strip: got %q", got)
		}
	})
}

func mustWrite(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

// DetectProjectNameWith takes every root manifest it reads through the reader
// it is given — go.mod, and package.json only when go.mod names nothing — so
// a caller that accounts for its working-copy reads sees each one.
func TestDetectProjectNameWithReadsThroughTheReader(t *testing.T) {
	dir := t.TempDir()
	mustWrite(t, filepath.Join(dir, "package.json"), `{"name": "@acme/widgets"}`)
	var read []string
	got := DetectProjectNameWith(dir, func(name string) ([]byte, error) {
		read = append(read, filepath.Base(name))
		return os.ReadFile(name)
	})
	if got != "widgets" || len(read) != 2 || read[0] != "go.mod" || read[1] != "package.json" {
		t.Fatalf("DetectProjectNameWith = %q reading %v, want widgets reading go.mod then package.json", got, read)
	}
}

func TestPortableProjectNameRelocation(t *testing.T) {
	const indexed = "widgetalpha/run.py"
	for _, dir := range []string{"widgetalpha", "widgetbeta"} {
		t.Run(dir, func(t *testing.T) {
			root := filepath.Join(t.TempDir(), dir)
			legacy := DetectProjectNameWith(root, func(string) ([]byte, error) { return nil, os.ErrNotExist })
			want := indexed
			if dir == "widgetalpha" {
				want = "run.py"
			}
			if got := StripProjectNameFromPath(indexed, legacy); got != want {
				t.Fatalf("legacy path = %q, want %q", got, want)
			}
			portable, err := PortableProjectName(func(string) ([]byte, error) { return nil, nil })
			if err != nil || portable != "" || StripProjectNameFromPath(indexed, portable) != indexed {
				t.Fatalf("portable name = %q, err = %v; must keep %q", portable, err, indexed)
			}
		})
	}
}

func TestPortableProjectNameFrozenSelection(t *testing.T) {
	changed := errors.New("frozen manifest changed")
	for _, tt := range []struct {
		name, goMod, pkg, want string
		readErr                error
		reads                  int
	}{
		{name: "module first", goMod: "module example.org/widgetalpha\n", want: "widgetalpha", reads: 1},
		{name: "short module stops", goMod: "module example.org/cli\n", pkg: `{"name":"widgetbeta"}`, reads: 1},
		{name: "scoped package", pkg: `{"name":"@example/widgetbeta"}`, want: "widgetbeta", reads: 2},
		{name: "bad package", pkg: `{"name":`, reads: 2},
		{name: "changed input is not absence", readErr: changed, pkg: `{"name":"widgetbeta"}`, reads: 1},
	} {
		t.Run(tt.name, func(t *testing.T) {
			reads := 0
			got, err := PortableProjectName(func(path string) ([]byte, error) {
				reads++
				switch path {
				case "go.mod":
					return []byte(tt.goMod), tt.readErr
				case "package.json":
					return []byte(tt.pkg), nil
				default:
					t.Fatalf("unexpected input %q", path)
					return nil, nil
				}
			})
			if got != tt.want || !errors.Is(err, tt.readErr) || reads != tt.reads {
				t.Fatalf("name = %q, err = %v, reads = %d; want %q, %v, %d", got, err, reads, tt.want, tt.readErr, tt.reads)
			}
		})
	}
}
