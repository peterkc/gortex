package main

import (
	"context"
	"encoding/json"
	"errors"
	"gxstudy/adapter"
	"gxstudy/conformance"
	"gxstudy/dataset"
	"gxstudy/oracle"
	"gxstudy/runner"
	"gxstudy/workload"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
)

func fixtureConfig(t *testing.T) config {
	t.Helper()
	root := t.TempDir()
	tier := filepath.Join(root, "tier")
	d := conformance.Fixture()
	if _, err := dataset.Write(filepath.Join(tier, d.Repo), d); err != nil {
		t.Fatal(err)
	}
	queries := workload.Generate([]dataset.Dataset{d}, 20261022)
	if err := dataset.WriteJSON(filepath.Join(tier, "queries.json"), queries); err != nil {
		t.Fatal(err)
	}
	view := map[string]adapter.Segment{d.Repo: oracle.New(d)}
	var expected []workload.Expected
	for _, q := range queries {
		a, e := workload.Evaluate(context.Background(), view, adapter.Fanout{Segments: view}, q)
		if e != nil {
			t.Fatal(e)
		}
		expected = append(expected, workload.Expected{QueryID: q.ID, Answer: a})
	}
	expected, e := oracle.ReferenceAnswers([]dataset.Dataset{d}, queries, expected)
	if e != nil {
		t.Fatal(e)
	}
	if err := dataset.WriteJSON(filepath.Join(tier, "expected.json"), expected); err != nil {
		t.Fatal(err)
	}
	if err := dataset.TierManifest(tier, []string{d.Repo}, dataset.Manifest{Version: dataset.GeneratorVersion, SourceRevision: strings.Repeat("b", 40)}); err != nil {
		t.Fatal(err)
	}
	approval := filepath.Join(root, "approval.txt")
	if err := os.WriteFile(approval, []byte("fixture-only; no measurement authority"), 0600); err != nil {
		t.Fatal(err)
	}
	pin, err := dataset.Hash(approval)
	if err != nil {
		t.Fatal(err)
	}
	return config{BaseLayout: "c-parquet", Schema: "linkdelta-measure/v1", Data: tier, Out: filepath.Join(root, "out"), Launcher: "/fixture/limited-command", CgroupRoot: "/sys/fs/cgroup/fixture", SourceRevision: strings.Repeat("a", 40), Approval: approval, ApprovalSHA256: pin, DeltaRepo: d.Repo, Sizes: []int{1, 2}, Repeats: 1, WallSeconds: 30, Limits: runner.MemoryLimits{Runner: 268435456, Checker: 268435456, Engine: 268435456}, LinkLimit: linkCap}
}
func TestMeasurementWorkersTinyCompleteQueries(t *testing.T) {
	ctx := context.Background()
	c := fixtureConfig(t)
	if err := os.Mkdir(c.Out, 0700); err != nil {
		t.Fatal(err)
	}
	before, err := treePins(c.Data)
	if err != nil {
		t.Fatal(err)
	}
	if err = prepare(ctx, c); err != nil {
		t.Fatal(err)
	}
	if err = prepareReferences(ctx, c); err != nil {
		t.Fatal(err)
	}
	for _, format := range []string{"arrow", "parquet"} {
		if err = work(ctx, c, "delta-"+format); err != nil {
			t.Fatal(err)
		}
		if err = work(ctx, c, "verify-"+format); err != nil {
			t.Fatal(err)
		}
		var metrics deltaReport
		if err = dataset.ReadJSON(filepath.Join(c.Out, "delta-"+format, "metrics.json"), &metrics); err != nil {
			t.Fatal(err)
		}
		if metrics.Edits != 2 || len(metrics.Checkpoints) != 2 || metrics.Visibility.Count != 2 || metrics.MaxEditCPU <= 0 {
			t.Fatal(metrics)
		}

		raw := map[int]map[string]map[string][]int64{}
		if e := eachJSON(filepath.Join(c.Out, "delta-"+format, "answers.jsonl"), func(s sample) error {
			if s.ElapsedNS <= 0 {
				t.Fatal("missing raw timing")
			}
			if s.Repeat < 0 {
				return nil
			}
			if raw[s.Size] == nil {
				raw[s.Size] = map[string]map[string][]int64{"base": {}, "wrapper": {}, "delta": {}}
			}
			raw[s.Size][s.Form][s.Query.Op] = append(raw[s.Size][s.Form][s.Query.Op], s.ElapsedNS)
			return nil
		}); e != nil {
			t.Fatal(e)
		}
		for _, cp := range metrics.Checkpoints {
			empty := func() checkpoint {
				return checkpoint{BaseP95: map[string]float64{}, WrapperP95: map[string]float64{}, DeltaP95: map[string]float64{}, Ratio: map[string]float64{}, AARatio: map[string]float64{}}
			}
			got, e := summarizeTimings(empty(), raw[cp.Size])
			if e != nil || !reflect.DeepEqual(got.BaseP95, cp.BaseP95) || !reflect.DeepEqual(got.DeltaP95, cp.DeltaP95) || !reflect.DeepEqual(got.WrapperP95, cp.WrapperP95) || !reflect.DeepEqual(got.Ratio, cp.Ratio) || !reflect.DeepEqual(got.AARatio, cp.AARatio) {
				t.Fatal("raw samples do not reproduce metrics", got, cp, e)
			}
			for op, values := range raw[cp.Size]["wrapper"] {
				for i := range values {
					values[i] *= 2
				}
				raw[cp.Size]["wrapper"][op] = values
			}
			changed, e := summarizeTimings(empty(), raw[cp.Size])
			if e != nil || changed.AAWorstRatio == got.AAWorstRatio {
				t.Fatal("raw timing changes did not affect control", changed, e)
			}
		}
	}
	after, err := treePins(c.Data)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(before, after) {
		t.Fatal("protected input mutated")
	}
	// Corrupt one real serialized answer and require the checker caller to fail.
	path := filepath.Join(c.Out, "delta-arrow", "answers.jsonl")
	var rows []sample
	if err = eachJSON(path, func(s sample) error { rows = append(rows, s); return nil }); err != nil {
		t.Fatal(err)
	}
	changed := false
	for i := range rows {
		a, e := loadAnswer(filepath.Join(c.Out, "answer-blobs"), rows[i].AnswerSHA256)
		if e != nil {
			t.Fatal(e)
		}
		if len(a.Edges) > 0 {
			a.Edges = a.Edges[1:]
			pin, e := storeAnswer(filepath.Join(c.Out, "answer-blobs"), a)
			if e != nil {
				t.Fatal(e)
			}
			rows[i].AnswerSHA256 = pin
			changed = true
			break
		}
	}
	if !changed {
		t.Fatal("fixture lacks edge answer")
	}
	file, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range rows {
		raw, e := json.Marshal(s)
		if e != nil {
			t.Fatal(e)
		}
		if _, e = file.Write(append(raw, '\n')); e != nil {
			t.Fatal(e)
		}
	}
	if err = file.Close(); err != nil {
		t.Fatal(err)
	}
	if err = verifyDeltas(ctx, c, "arrow"); err == nil {
		t.Fatal("dropped edge accepted by checker caller")
	}
}
func TestConfigRefusesMissingDriftAndWrongLimits(t *testing.T) {
	c := fixtureConfig(t)
	path := filepath.Join(t.TempDir(), "config.json")
	write := func(c config) string {
		if err := dataset.WriteJSON(path, c); err != nil {
			t.Fatal(err)
		}
		pin, err := dataset.Hash(path)
		if err != nil {
			t.Fatal(err)
		}
		return pin
	}
	pin := write(c)
	if _, err := readConfig(path, pin); err != nil {
		t.Fatal(err)
	}
	if _, err := readConfig(path, strings.Repeat("0", 64)); err == nil {
		t.Fatal("wrong config pin accepted")
	}
	for _, tc := range []struct {
		name   string
		change func(*config)
	}{{"missing-data", func(c *config) { c.Data = "" }}, {"missing-engine-limit", func(c *config) { c.Limits.Engine = 0 }}, {"wrong-link-cap", func(c *config) { c.LinkLimit = linkCap * 2 }}, {"missing-wall", func(c *config) { c.WallSeconds = 0 }}, {"reordered-sizes", func(c *config) { c.Sizes = []int{2, 1} }}, {"approval-drift", func(c *config) { c.ApprovalSHA256 = strings.Repeat("0", 64) }}} {
		t.Run(tc.name, func(t *testing.T) {
			bad := c
			tc.change(&bad)
			if _, err := readConfig(path, write(bad)); err == nil {
				t.Fatal("invalid config accepted")
			}
		})
	}
	if err := dispatch([]string{"measure"}); err == nil {
		t.Fatal("missing explicit arguments accepted")
	}
	if err := dispatch([]string{"worker", "-task", "prepare"}); err == nil {
		t.Fatal("unbound worker accepted")
	}
}
func TestThresholdBranchesKeepTargets(t *testing.T) {
	for _, tc := range []struct {
		name             string
		points           []checkpoint
		branch           string
		threshold, lower *int
	}{{"crossing", []checkpoint{{Size: 1, WorstRatio: 1.01}, {Size: 4, WorstRatio: 1.10}, {Size: 16, WorstRatio: 1.11}}, "observed-10-percent-crossing", intPtr(4), nil}, {"no-crossing", []checkpoint{{Size: 1, WorstRatio: 1}, {Size: 4, WorstRatio: 1.1}}, "no-crossing-lower-bound-only", nil, intPtr(4)}, {"first-size-misses", []checkpoint{{Size: 1, WorstRatio: 1.11}}, "crossing-at-smallest-size-threshold-unresolved", nil, nil}} {
		t.Run(tc.name, func(t *testing.T) {
			r := threshold(deltaReport{Checkpoints: tc.points})
			if r.CompactionBranch != tc.branch || !reflect.DeepEqual(r.Threshold, tc.threshold) || !reflect.DeepEqual(r.LowerBound, tc.lower) {
				t.Fatal(r)
			}
		})
	}
	// Each rejection starts with a genuinely eligible regime. Changing one
	// bound must fail; later diagnostics above the served threshold must not.
	for _, tc := range []struct {
		name   string
		change func(*deltaReport)
	}{
		{"cpu", func(r *deltaReport) { r.Checkpoints[0].MaxEditCPU = 1000000001 }},
		{"visibility", func(r *deltaReport) { r.Checkpoints[0].Visibility.P95 = 5000000001 }},
		{"disk", func(r *deltaReport) { r.Checkpoints[0].ByteRatio = 2.01 }},
		{"read", func(r *deltaReport) { r.Checkpoints[0].WorstRatio = 1.1001 }},
		{"missing-samples", func(r *deltaReport) { r.Checkpoints[0].Visibility.Count = 0 }},
		{"no-threshold", func(r *deltaReport) { r.LowerBound = nil }},
		{"absent-checkpoint", func(r *deltaReport) { r.LowerBound = intPtr(32) }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := passingReport()
			if !deltaPass(r) {
				t.Fatal("positive fixture does not pass")
			}
			tc.change(&r)
			if deltaPass(r) {
				t.Fatal("guard did not reject")
			}
		})
	}
	r := passingReport()
	r.MaxEditCPU = 9e9
	r.Visibility.P95 = 9e9
	r.Checkpoints = append(r.Checkpoints, checkpoint{Size: 32, MaxEditCPU: 9e9, Visibility: runner.Distribution{Count: 32, P95: 9e9}, WorstRatio: 1.2, ByteRatio: 1})
	r.Threshold = intPtr(1)
	r.LowerBound = nil
	if !deltaPass(r) {
		t.Fatal("unserved diagnostics invalidated earlier regime")
	}

}
func intPtr(n int) *int { return &n }

func TestMeasurementRefusesExistingOutputBeforeLaunch(t *testing.T) {
	c := fixtureConfig(t)
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	c.Launcher, err = filepath.EvalSymlinks(exe)
	if err != nil {
		t.Fatal(err)
	}
	if err = os.Mkdir(c.Out, 0700); err != nil {
		t.Fatal(err)
	}
	marker := filepath.Join(c.Out, "keep")
	if err = os.WriteFile(marker, []byte("unchanged"), 0600); err != nil {
		t.Fatal(err)
	}
	cfg := filepath.Join(t.TempDir(), "config.json")
	if err = dataset.WriteJSON(cfg, c); err != nil {
		t.Fatal(err)
	}
	pin, err := dataset.Hash(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if err = measure([]string{"-config", cfg, "-config-pin", pin}); err == nil {
		t.Fatal("existing output accepted")
	}
	raw, err := os.ReadFile(marker)
	if err != nil || string(raw) != "unchanged" {
		t.Fatal("existing evidence changed", err)
	}
	if _, err = os.Stat(filepath.Join(c.Out, "controller-binding.json")); !os.IsNotExist(err) {
		t.Fatal("child was prepared before refusal", err)
	}
}

func passingReport() deltaReport {
	return deltaReport{MaxEditCPU: 1, Visibility: runner.Distribution{Count: 1, P95: 1}, Checkpoints: []checkpoint{{Size: 1, MaxEditCPU: 1, Visibility: runner.Distribution{Count: 1, P95: 1}, WorstRatio: 1, ByteRatio: 1}}, LowerBound: intPtr(1)}
}
func completedReceipt(limit int64) externalResult {
	return externalResult{State: "completed", Removed: true, Memory: runner.ComponentMemory{PID: 1, LimitBytes: limit, PeakBytes: 1, PeakRSSBytes: 1, Status: "completed", ExitCode: 0}}
}
func TestSelectionAndLinkReceiptDecisions(t *testing.T) {
	for _, tc := range []struct {
		name           string
		reports        map[string]deltaReport
		branch, status string
	}{
		{"arrow", map[string]deltaReport{"arrow": passingReport()}, "arrow", "pass"},
		{"parquet", map[string]deltaReport{"parquet": passingReport()}, "parquet", "pass"},
		{"both", map[string]deltaReport{"arrow": passingReport(), "parquet": passingReport()}, "both-pass-lead-selects-simplest", "partial"},
		{"none", map[string]deltaReport{"arrow": {}, "parquet": {}}, "no-passing-form", "miss"},
		{"missing", map[string]deltaReport{}, "no-complete-form-evidence", "blocked"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := selectDelta(tc.reports)
			if r.Branch != tc.branch || r.Status != tc.status {
				t.Fatal(r)
			}
		})
	}
	a, p := passingReport(), passingReport()
	a.Checkpoints[0].MaxEditCPU = 2e9
	p.Checkpoints[0].MaxEditCPU = 2e9
	if r := selectDelta(map[string]deltaReport{"arrow": a, "parquet": p}); r.Branch != "stop-ask-user-both-edit-bound-misses" {
		t.Fatal(r)
	}
	a, p = passingReport(), passingReport()
	a.Checkpoints[0].Visibility.P95 = 6e9
	p.Checkpoints[0].Visibility.P95 = 6e9
	if r := selectDelta(map[string]deltaReport{"arrow": a, "parquet": p}); r.Branch != "stop-ask-user-both-visibility-bound-misses" {
		t.Fatal(r)
	}
	rec := completedReceipt(linkCap)
	l := linkResult{Status: "pass"}
	parity := parityResult{Status: "pass"}
	if r := linkTarget("global", rec, l, parity); r.Status != "pass" {
		t.Fatal(r)
	}
	for _, tc := range []struct {
		name   string
		change func(*externalResult, *linkResult, *parityResult)
		status string
	}{
		{"partial", func(_ *externalResult, l *linkResult, _ *parityResult) { l.Status = "partial" }, "partial"},
		{"parity", func(_ *externalResult, _ *linkResult, p *parityResult) { p.Status = "miss" }, "miss"},
		{"memory", func(r *externalResult, _ *linkResult, _ *parityResult) { r.Memory.PeakBytes = linkCap + 1 }, "miss"},
		{"missing-accounting", func(r *externalResult, _ *linkResult, _ *parityResult) { r.Memory.PID = 0 }, "miss"},
		{"cleanup", func(r *externalResult, _ *linkResult, _ *parityResult) { r.Removed = false }, "miss"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r, l, p := rec, l, parity
			tc.change(&r, &l, &p)
			if got := linkTarget("global", r, l, p); got.Status != tc.status {
				t.Fatal(got)
			}
		})
	}
	if memoryFallback(rec) {
		t.Fatal("semantic failure triggered package fallback")
	}
	oom := rec
	oom.State = "oom"
	oom.Memory.Status = "failed"
	oom.Memory.OOMKills = 1
	if !memoryFallback(oom) {
		t.Fatal("contained OOM did not trigger fallback")
	}
	oom.Removed = false
	if memoryFallback(oom) {
		t.Fatal("unsafe cleanup triggered fallback")
	}
	for _, state := range []string{"cancelled", "deadline", "refused"} {
		r := rec
		r.State = state
		if safeReceipt(r, linkCap) {
			t.Fatal("unsafe launch continuation", state)
		}
	}
}
func TestIndependentControllerPaths(t *testing.T) {
	for _, scenario := range []string{"partial", "parity-miss", "oom-arrow-failure", "unsafe-cleanup", "cancelled", "references-failure", "verify-arrow-failure"} {
		t.Run(scenario, func(t *testing.T) {
			c := fixtureConfig(t)
			if e := os.Mkdir(c.Out, 0700); e != nil {
				t.Fatal(e)
			}
			var calls []string
			write := func(path string, v any) {
				t.Helper()
				if e := os.MkdirAll(filepath.Dir(path), 0700); e != nil {
					t.Fatal(e)
				}
				if e := dataset.WriteJSON(path, v); e != nil {
					t.Fatal(e)
				}
			}
			failed := func(limit int64) externalResult {
				r := completedReceipt(limit)
				r.State = "failed"
				r.Memory.Status = "failed"
				r.Memory.ExitCode = 1
				return r
			}
			run := func(task string, limit int64) (externalResult, error) {
				calls = append(calls, task)
				r := completedReceipt(limit)
				if task == "link-global" {
					switch scenario {
					case "oom-arrow-failure":
						r = failed(limit)
						r.State = "oom"
						r.Memory.OOMKills = 1
						return r, errors.New("contained OOM")
					case "unsafe-cleanup":
						r = failed(limit)
						r.Removed = false
						return r, errors.New("cleanup uncertain")
					case "cancelled":
						r = failed(limit)
						r.State = "cancelled"
						return r, errors.New("cancelled")
					}
				}
				if (task == "delta-arrow" && scenario == "oom-arrow-failure") || (task == "verify-arrow" && scenario == "verify-arrow-failure") || (task == "references" && scenario == "references-failure") {
					return failed(limit), errors.New("contained failure")
				}
				switch task {
				case "link-global", "link-package":
					status := "pass"
					if scenario == "partial" && task == "link-global" {
						status = "partial"
					}
					write(filepath.Join(c.Out, task, "result.json"), linkResult{Status: status})
				case "check-link-global", "check-link-package":
					status := "pass"
					if scenario == "parity-miss" && task == "check-link-global" {
						status = "miss"
					}
					write(filepath.Join(c.Out, task+".json"), parityResult{Status: status})
				case "delta-arrow", "delta-parquet":
					write(filepath.Join(c.Out, task, "metrics.json"), passingReport())
				case "verify-arrow", "verify-parquet":
					write(filepath.Join(c.Out, task+".json"), verifyReport{Status: "pass", Checked: 1, NativeTopKComparisons: 1, NativeTopKEqual: 1})
				}
				return r, nil
			}
			targets, e := collectTargets(c, run)
			if scenario == "unsafe-cleanup" || scenario == "cancelled" {
				if e == nil || slices.Contains(calls, "references") || slices.Contains(calls, "link-package") {
					t.Fatal(calls, targets, e)
				}
				return
			}
			if e != nil {
				t.Fatal(e)
			}
			if scenario == "references-failure" {
				if slices.Contains(calls, "delta-arrow") || slices.Contains(calls, "delta-parquet") {
					t.Fatal(calls)
				}
				return
			}
			if !slices.Contains(calls, "delta-parquet") || !slices.Contains(calls, "verify-parquet") {
				t.Fatal("independent Check 4 suppressed", calls)
			}
			if (scenario == "oom-arrow-failure") != slices.Contains(calls, "link-package") {
				t.Fatal("wrong fallback decision", calls)
			}
			if scenario == "partial" || scenario == "parity-miss" {
				for _, target := range targets {
					if strings.HasPrefix(target.Target, "check4-") && target.Target != "check4-delta-selection" && target.Status != "pass" {
						t.Fatal(target)
					}
				}
			}
		})
	}
}
func TestReferenceBindingRejectsChangedInputs(t *testing.T) {
	ctx := context.Background()
	c := fixtureConfig(t)
	if e := os.Mkdir(c.Out, 0700); e != nil {
		t.Fatal(e)
	}
	if e := prepare(ctx, c); e != nil {
		t.Fatal(e)
	}
	if e := prepareReferences(ctx, c); e != nil {
		t.Fatal(e)
	}
	before, e := referenceBinding(c)
	if e != nil {
		t.Fatal(e)
	}
	if e = os.WriteFile(filepath.Join(c.Out, "changes.json"), []byte("changed binding"), 0600); e != nil {
		t.Fatal(e)
	}
	after, e := referenceBinding(c)
	if e != nil || before == after {
		t.Fatal(before, after, e)
	}
	var qs []workload.Query
	if e = dataset.ReadJSON(filepath.Join(c.Out, "queries.json"), &qs); e != nil {
		t.Fatal(e)
	}
	var ref referenceRecord
	if e = dataset.ReadJSON(filepath.Join(c.Out, "references", referenceKey(false, qs[0])+".json"), &ref); e != nil || ref.Binding == after {
		t.Fatal(ref.Binding, after, e)
	}
}

func TestFinalizedLogAndProtectedFailureEvidence(t *testing.T) {
	root := t.TempDir()
	for _, d := range []string{"controller-scope", "delta-arrow-scope"} {
		if e := os.Mkdir(filepath.Join(root, d), 0700); e != nil {
			t.Fatal(e)
		}
	}
	live, e := os.Create(filepath.Join(root, "controller-scope", "command.log"))
	if e != nil {
		t.Fatal(e)
	}
	if _, e = live.WriteString("before summary\n"); e != nil {
		t.Fatal(e)
	}
	component := filepath.Join(root, "delta-arrow-scope", "command.log")
	if e = os.WriteFile(component, []byte("closed component\n"), 0600); e != nil {
		t.Fatal(e)
	}
	pins, e := outputPins(root)
	if e != nil {
		t.Fatal(e)
	}
	if _, ok := pins["controller-scope/command.log"]; ok {
		t.Fatal("summary hashed its own live log")
	}
	if !digest(pins["delta-arrow-scope/command.log"]) {
		t.Fatal("closed component log omitted")
	}
	if _, e = live.WriteString("after summary\n"); e != nil {
		t.Fatal(e)
	}
	if e = live.Close(); e != nil {
		t.Fatal(e)
	}
	final, e := dataset.Hash(filepath.Join(root, "controller-scope", "command.log"))
	if e != nil || !digest(final) {
		t.Fatal(final, e)
	}
	c := fixtureConfig(t)
	if e = os.Mkdir(c.Out, 0700); e != nil {
		t.Fatal(e)
	}
	if e = prepare(context.Background(), c); e != nil {
		t.Fatal(e)
	}
	if status, e := protectedInputs(c); e != nil || status != "unchanged" {
		t.Fatal(status, e)
	}
	if e = os.WriteFile(filepath.Join(c.Data, "unexpected"), []byte("mutation after failed worker"), 0600); e != nil {
		t.Fatal(e)
	}
	if status, e := protectedInputs(c); e == nil || status != "changed" {
		t.Fatal(status, e)
	}
}
func TestDeltaRepositoryMustMatchPinnedQueryRepository(t *testing.T) {
	c := fixtureConfig(t)
	c.DeltaRepo = "another-repo"
	if e := os.Mkdir(c.Out, 0700); e != nil {
		t.Fatal(e)
	}
	if e := prepare(context.Background(), c); e == nil {
		t.Fatal("unexercised delta repository accepted")
	}
}
