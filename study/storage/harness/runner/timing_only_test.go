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
	"path/filepath"
	"slices"
	"testing"
)

func TestTimingOnlyFlagReachesWrittenRequest(t *testing.T) {
	for _, timingOnly := range []bool{false, true} {
		t.Run(map[bool]string{false: "default", true: "timing"}[timingOnly], func(t *testing.T) {
			var args []string
			if timingOnly {
				args = append(args, "-timing-only")
			}
			req := cliFixtureRequest(t, args...)
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

func TestBuildOnlyHasNoServingOrTimingPhases(t *testing.T) {
	d := conformance.Fixture()
	b := &trackedBackend{Backend: oracle.Backend{}}
	result := Result{BuildOnly: true, Metrics: map[string]Distribution{}, Capabilities: b.Capabilities()}
	query := workload.Query{ID: "must-not-run", Op: "unknown", Repo: d.Repo}
	if err := runOnce(context.Background(), b, nil, []dataset.Dataset{d}, []workload.Query{query}, nil, t.TempDir(), 1, defaultSafetyTimeout, &result); err != nil {
		t.Fatal(err)
	}
	if len(result.Builds) != 1 || len(result.Checks) != 0 || len(result.Metrics) != 0 || result.Delta.Ref.Path != "" {
		t.Fatal("build-only performed unselected work", result)
	}
	raw, err := json.Marshal(result)
	if err != nil {
		t.Fatal(err)
	}
	var wire map[string]json.RawMessage
	if err = json.Unmarshal(raw, &wire); err != nil {
		t.Fatal(err)
	}
	for _, field := range []string{"metrics", "checks", "delta", "concurrent", "crash", "query_latency_sha256", "started", "uptime"} {
		if _, ok := wire[field]; ok {
			t.Fatal("unselected timing or phase emitted", field)
		}
	}
	for _, field := range []string{"cpu_ns", "duration_ns", "p95_ns"} {
		if bytes.Contains(raw, []byte(field)) {
			t.Fatal("timing field emitted", string(raw))
		}
	}
}

func TestCheckOnlyQueriesOnceAndEmitsOnlyBuildCPU(t *testing.T) {
	d := conformance.Fixture()
	b := &trackedBackend{Backend: oracle.Backend{}}
	q := workload.Query{ID: "point", Op: "id", Repo: d.Repo, Key: "a"}
	want, err := workload.Evaluate(context.Background(), map[string]adapter.Segment{d.Repo: oracle.New(d)}, nil, q)
	if err != nil {
		t.Fatal(err)
	}
	result := Result{CheckOnly: true, Metrics: map[string]Distribution{}, Capabilities: b.Capabilities()}
	if err = runOnce(context.Background(), b, nil, []dataset.Dataset{d}, []workload.Query{q}, []workload.Expected{{QueryID: q.ID, Answer: want}}, t.TempDir(), 1, defaultSafetyTimeout, &result); err != nil {
		t.Fatal(err)
	}
	if len(result.Checks) != 1 || result.Checks[0].Mismatch || len(result.Metrics) != 0 || len(result.QueryLatencies) != 0 || len(b.refs) != 1 {
		t.Fatal("check-only ran unselected work", result)
	}
	raw, err := json.Marshal(result)
	if err != nil {
		t.Fatal(err)
	}
	for _, field := range []string{"duration_ns", "metrics", "query_latency_sha256", "p95_ns", "concurrent", "delta"} {
		if bytes.Contains(raw, []byte(field)) {
			t.Fatal("timing or update field emitted", field)
		}
	}
	if !bytes.Contains(raw, []byte("cpu_ns")) {
		t.Fatal("reported storage-build CPU omitted")
	}
}

func TestSelectedCheckOperationsKeepPairedAnswers(t *testing.T) {
	var qs []workload.Query
	var want []workload.Expected
	for _, op := range []string{"id", "name", "file", "out", "in", "traverse2", "symbols", "content", "vector", "multi_name", "multi_in", "multi_traverse3", "multi_symbols", "multi_vector"} {
		qs = append(qs, workload.Query{ID: op, Op: op})
		want = append(want, workload.Expected{QueryID: op})
	}
	for _, tc := range []struct {
		lookup, base bool
		expected     []string
	}{
		{true, false, []string{"id", "name", "file"}},
		{false, true, []string{"name", "in", "traverse2", "symbols", "multi_name", "multi_in", "multi_traverse3", "multi_symbols"}},
	} {
		selected, answers := selectedQueries(qs, want, tc.lookup, tc.base)
		var got []string
		for i, q := range selected {
			got = append(got, q.ID)
			if answers[i].QueryID != q.ID {
				t.Fatal("answer pairing changed")
			}
		}
		if !slices.Equal(got, tc.expected) {
			t.Fatal("unselected measurement", got, tc.expected)
		}
	}
	if len(qs) != 14 || len(want) != 14 {
		t.Fatal("caller inputs changed")
	}
}

func TestSelectedModeFlagsReachRequest(t *testing.T) {
	for _, mode := range []string{"build-only", "check-only", "lookup-only", "base-only"} {
		t.Run(mode, func(t *testing.T) {
			args := []string{"-" + mode}
			if mode == "lookup-only" || mode == "base-only" {
				args = append(args, "-timing-only")
			}
			req := cliFixtureRequest(t, args...)
			if req.BuildOnly != (mode == "build-only") || req.CheckOnly != (mode == "check-only") || req.LookupOnly != (mode == "lookup-only") || req.BaseOnly != (mode == "base-only") {
				t.Fatal("selected mode lost", req)
			}
		})
	}
}
