//go:build acceptance

package main

import (
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

// Both arms use the same repository path and Git identity. Reset only the
// fixture's private daemon state, not the corpus, between cold indexes.
func TestReconstructedExport(t *testing.T) {
	mainBinary, candidateBinary := os.Getenv("GORTEX_RECON_EXPORT_MAIN_BINARY"), os.Getenv("GORTEX_RECON_EXPORT_CANDIDATE_BINARY")
	if mainBinary == "" || candidateBinary == "" {
		t.Skip("set GORTEX_RECON_EXPORT_MAIN_BINARY and GORTEX_RECON_EXPORT_CANDIDATE_BINARY")
	}
	corpus, out := os.Getenv("GORTEX_RECON_CORPUS"), os.Getenv("GORTEX_RECON_EXPORT_OUT_DIR")
	if corpus == "" || out == "" {
		t.Fatal("graph export needs GORTEX_RECON_CORPUS and GORTEX_RECON_EXPORT_OUT_DIR")
	}
	f := newIssue767FixtureWithCorpus(t, mainBinary, func(f *issue767Fixture) {
		err := filepath.WalkDir(corpus, func(path string, entry fs.DirEntry, walkErr error) error {
			if walkErr != nil {
				return walkErr
			}
			if entry.IsDir() {
				if entry.Name() == ".git" {
					return filepath.SkipDir
				}
				return nil
			}
			rel, err := filepath.Rel(corpus, path)
			if err != nil {
				return err
			}
			data, err := os.ReadFile(path)
			if err != nil {
				return err
			}
			f.write(filepath.Join(f.primary, rel), string(data))
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
	})
	configPath := filepath.Join(f.root, "config", "gortex", "config.yaml")
	config, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatal(err)
	}
	var exports [][]byte
	for _, arm := range []struct{ name, binary string }{{"main", mainBinary}, {"candidate", candidateBinary}} {
		f.binary = arm.binary
		f.start()
		f.awaitSymbolIn(f.primary, "WriteCypher", "internal/exporter/cypher.go", 5*time.Minute)
		f.settle()
		path := filepath.Join(out, arm.name+".cypher")
		f.command(5*time.Minute, f.primary, "export", "--format", "cypher", "--out", path)
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		exports = append(exports, data)
		if !f.stopAndJoin() || t.Failed() {
			t.Fatal("graph export daemon did not stop cleanly")
		}
		if arm.name == "candidate" {
			break
		}
		entries, err := os.ReadDir(f.root)
		if err != nil {
			t.Fatal(err)
		}
		for _, entry := range entries {
			if entry.Name() == "repo" || entry.Name() == "gitconfig" || (strings.HasPrefix(entry.Name(), "daemon-") && strings.HasSuffix(entry.Name(), ".log")) {
				continue
			}
			if err := os.RemoveAll(filepath.Join(f.root, entry.Name())); err != nil {
				t.Fatal(err)
			}
		}
		f.write(configPath, string(config))
	}
	if err := compareReconstructedExports(exports[0], exports[1]); err != nil {
		t.Fatal(err)
	}
}

func compareReconstructedExports(main, candidate []byte) error {
	left, right := strings.Split(strings.TrimSuffix(string(main), "\n"), "\n"), strings.Split(strings.TrimSuffix(string(candidate), "\n"), "\n")
	slices.Sort(left)
	slices.Sort(right)
	if len(main) == 0 || len(candidate) == 0 {
		return fmt.Errorf("graph export is empty: main=%d bytes candidate=%d bytes", len(main), len(candidate))
	}
	for i := range min(len(left), len(right)) {
		if left[i] != right[i] {
			return fmt.Errorf("graph exports differ at sorted line %d (main=%d lines candidate=%d lines): main %.200q; candidate %.200q", i+1, len(left), len(right), left[i], right[i])
		}
	}
	if len(left) != len(right) {
		return fmt.Errorf("graph export line counts differ: main=%d candidate=%d", len(left), len(right))
	}
	return nil
}

func TestReconstructedExportComparison(t *testing.T) {
	for _, tc := range []struct {
		name, main, candidate string
		wantError             bool
	}{
		{"reordered", "node A\nnode B\n", "node B\nnode A\n", false},
		{"changed", "node A\n", "node B\n", true},
		{"missing", "node A\nnode B\n", "node A\n", true},
		{"duplicate", "node A\n", "node A\nnode A\n", true},
		{"empty", "", "", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := compareReconstructedExports([]byte(tc.main), []byte(tc.candidate))
			if (err != nil) != tc.wantError {
				t.Fatalf("compareReconstructedExports() = %v; want error %v", err, tc.wantError)
			}
		})
	}
}
