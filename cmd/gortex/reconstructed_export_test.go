//go:build acceptance

package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
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
	// Graph equality does not depend on shutdown speed; a forced stop is logged.
	f.forcedStopIsResult = true
	configPath := filepath.Join(f.root, "config", "gortex", "config.yaml")
	config, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatal(err)
	}
	var exports [][]byte
	for _, arm := range []struct{ name, binary string }{{"main", mainBinary}, {"candidate", candidateBinary}} {
		f.binary = arm.binary
		f.start()
		f.awaitSymbolIn(f.primary, "WriteCypher", filepath.Join(f.primary, "internal", "exporter", "cypher.go"), 5*time.Minute)
		f.settle()
		data := reconstructedExportWhenWarm(t, f, 15*time.Minute)
		if err := os.WriteFile(filepath.Join(out, arm.name+".cypher"), data, 0600); err != nil {
			t.Fatal(err)
		}
		exports = append(exports, data)
		if !f.stopAndJoin() || t.Failed() {
			t.Fatal("graph export daemon did not exit")
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

// The daemon writes --out files only inside an indexed repository, so the
// export is read from stdout. Until warm-up ends the daemon wraps a partial
// result in a {"warming": ...} object; retry until the export is the bare graph.
func reconstructedExportWhenWarm(t *testing.T, f *issue767Fixture, timeout time.Duration) []byte {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for {
		ctx, cancel := context.WithTimeout(t.Context(), 5*time.Minute)
		cmd := exec.CommandContext(ctx, f.binary, "export", "--format", "cypher")
		var stderr bytes.Buffer
		cmd.Dir, cmd.Env, cmd.Stderr = f.primary, f.env, &stderr
		out, err := cmd.Output()
		cancel()
		if err == nil && !reconstructedExportWarming(out) {
			return out
		}
		if time.Now().After(deadline) {
			t.Fatalf("graph export not ready after %v: err=%v stdout=%.300q stderr=%.300q", timeout, err, out, stderr.String())
		}
		time.Sleep(5 * time.Second)
	}
}

func reconstructedExportWarming(out []byte) bool {
	var envelope map[string]json.RawMessage
	if json.Unmarshal(out, &envelope) != nil {
		return false
	}
	_, warming := envelope["warming"]
	return warming
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

func TestReconstructedExportWarming(t *testing.T) {
	for _, tc := range []struct {
		name, out string
		want      bool
	}{
		{"cypher", "CREATE (:Function {id: \"a\"});\n", false},
		{"warming envelope", `{"warming":{"warming":true,"percent":57},"results":"CREATE ..."}`, true},
		{"other JSON object", `{"results":"CREATE ..."}`, false},
		{"empty", "", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := reconstructedExportWarming([]byte(tc.out)); got != tc.want {
				t.Fatalf("reconstructedExportWarming(%q) = %v; want %v", tc.out, got, tc.want)
			}
		})
	}
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
