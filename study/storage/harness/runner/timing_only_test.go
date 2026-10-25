package runner

import (
	"bytes"
	"context"
	"encoding/json"
	"gxstudy/adapter"
	"gxstudy/conformance"
	"gxstudy/dataset"
	"gxstudy/oracle"
	"gxstudy/workload"
	"os"
	"path/filepath"
	"slices"
	"testing"
)

func TestTimingOnlyCLISerialization(t *testing.T) {
	for _, timingOnly := range []bool{false, true} {
		t.Run(map[bool]string{false: "default", true: "timing"}[timingOnly], func(t *testing.T) {
			args := os.Args
			t.Cleanup(func() { os.Args = args })
			out := filepath.Join(t.TempDir(), "out")
			os.Args = []string{args[0], "-data", "unused", "-out", out, "-runs", "1", "-warmup=false"}
			if timingOnly {
				os.Args = append(os.Args, "-timing-only")
			}
			if err := RunCLI(oracle.Backend{}, oracle.NewCatalog); err != nil {
				t.Fatal(err)
			}
			var req runRequest
			if err := dataset.ReadJSON(filepath.Join(out, "request-01.json"), &req); err != nil {
				t.Fatal(err)
			}
			if req.TimingOnly != timingOnly || req.SafetyTimeout != defaultSafetyTimeout {
				t.Fatal("request mode or default timeout changed", req)
			}
		})
	}
}

func TestTimingOnlyRunPhases(t *testing.T) {
	for _, timingOnly := range []bool{false, true} {
		t.Run(map[bool]string{false: "default", true: "timing"}[timingOnly], func(t *testing.T) {
			dir := t.TempDir()
			data := filepath.Join(dir, "tier")
			d := conformance.Fixture()
			if _, err := dataset.Write(filepath.Join(data, d.Repo), d); err != nil {
				t.Fatal(err)
			}
			q := workload.Query{ID: "point", Op: "id", Repo: d.Repo, Key: d.Nodes[0].ID}
			a, err := workload.Evaluate(context.Background(), map[string]adapter.Segment{d.Repo: oracle.New(d)}, nil, q)
			if err != nil {
				t.Fatal(err)
			}
			if err := dataset.WriteJSON(filepath.Join(data, "queries.json"), []workload.Query{q}); err != nil {
				t.Fatal(err)
			}
			if err := dataset.WriteJSON(filepath.Join(data, "expected.json"), []workload.Expected{{QueryID: q.ID, Answer: a}}); err != nil {
				t.Fatal(err)
			}
			if err := dataset.TierManifest(data, []string{d.Repo}, dataset.Manifest{Version: dataset.GeneratorVersion}); err != nil {
				t.Fatal(err)
			}
			b := &hooksBackend{}
			out := filepath.Join(dir, "out")
			if err := runChild(b, oracle.NewCatalog, runRequest{Data: data, Out: out, Run: 1, Trials: 1, TimingOnly: timingOnly}); err != nil {
				t.Fatal(err)
			}
			path := filepath.Join(out, "oracle-tier-01.json")
			var report Result
			var wire map[string]json.RawMessage
			if err := dataset.ReadJSON(path, &report); err != nil {
				t.Fatal(err)
			}
			if err := dataset.ReadJSON(path, &wire); err != nil {
				t.Fatal(err)
			}
			if !report.Success || report.TimingOnly != timingOnly || len(report.Checks) != 1 || report.Checks[0].QueryID != q.ID {
				t.Fatal("base workload or result mode missing", report)
			}
			for _, key := range []string{"build_seal", "open_first_no_cache_flush", "open_warm", "id"} {
				if report.Metrics[key].Count == 0 {
					t.Fatal("missing base timing", key)
				}
			}
			for _, key := range []string{"delta", "delta_checks", "concurrent", "crash"} {
				if _, present := wire[key]; present == timingOnly {
					t.Fatal("wrong phase presence", key, timingOnly)
				}
			}
			if timingOnly {
				var skipped []string
				if err := json.Unmarshal(wire["skipped_phases"], &skipped); err != nil {
					t.Fatal(err)
				}
				if !slices.Equal(skipped, []string{"delta", "concurrent_readers", "commit_race", "crash_trials"}) {
					t.Fatal("missing explicit skipped phases", skipped)
				}
				for _, key := range []string{"delta", "concurrent", "crash"} {
					if _, present := report.ServerUsage[key]; present {
						t.Fatal("skipped phase executed", key)
					}
				}
				if report.Metrics["delta_build"].Count != 0 || b.releases != 1 || b.resets != 2 {
					t.Fatal("timing mode ran an update or missed cleanup", b, report.Metrics)
				}
				return
			}
			if len(report.DeltaChecks) == 0 || len(report.Crash) != 2 || len(report.Concurrent.CommitRaceWindows) != len(stages) || b.releases != 5 {
				t.Fatal("default full correctness phases changed", report)
			}
			if _, present := wire["skipped_phases"]; present {
				t.Fatal("default mode marked phases skipped")
			}
			if _, present := wire["timing_only"]; present {
				t.Fatal("default JSON schema changed")
			}
		})
	}
}

func TestTimingOnlyDefaultJSONUnchanged(t *testing.T) {
	r := Result{Success: true, Delta: BuildRecord{Repo: "fixture"}, Crash: []CrashTrial{{OK: true}}}
	type plain Result
	want, err := json.Marshal(plain(r))
	if err != nil {
		t.Fatal(err)
	}
	got, err := json.Marshal(r)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, want) {
		t.Fatal("default result serialization changed", string(got), string(want))
	}
}
