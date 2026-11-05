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
	"os/exec"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"
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
	c.Repeats = 12
	var generated []map[string]json.RawMessage
	if err := dataset.ReadJSON(filepath.Join(c.Data, "expected.json"), &generated); err != nil {
		t.Fatal(err)
	}
	for _, row := range generated {
		if len(row) != 2 || row["query_id"] == nil || row["answer"] == nil {
			t.Fatal("fixture differs from cmd/gen's query_id/answer format", row)
		}
	}
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
	var queries []workload.Query
	if err = dataset.ReadJSON(filepath.Join(c.Out, "queries.json"), &queries); err != nil {
		t.Fatal(err)
	}
	queryCounts := map[string]int{}
	for _, q := range queries {
		queryCounts[q.Op]++
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
		for _, edit := range metrics.EditSamples {
			if edit.TextRebuildCPU <= 0 || edit.CPU < edit.TextRebuildCPU {
				t.Fatal("D text collection/build CPU missing or outside total edit CPU", edit)
			}
		}
		for _, cp := range metrics.Checkpoints {
			if cp.HiddenBaseTextRows["content"] != len(conformance.Fixture().Content) || cp.HiddenBaseTextRows["symbols"] != len(conformance.Fixture().Symbols) {
				t.Fatal("hidden base text count missing", cp)
			}
		}
		var edits []editSample
		if err = eachJSON(filepath.Join(c.Out, "delta-"+format, "edits.jsonl"), func(s editSample) error { edits = append(edits, s); return nil }); err != nil || !slices.Equal(edits, metrics.EditSamples) {
			t.Fatal("durable edits differ from final report", edits, metrics.EditSamples, err)
		}
		var checkpoints []checkpoint
		if err = eachJSON(filepath.Join(c.Out, "delta-"+format, "checkpoints.jsonl"), func(p checkpoint) error { checkpoints = append(checkpoints, p); return nil }); err != nil || !reflect.DeepEqual(checkpoints, metrics.Checkpoints) {
			t.Fatal("durable checkpoints differ from final report", checkpoints, metrics.Checkpoints, err)
		}

		var triple []sample
		raw := map[int]map[string]map[string][]int64{}
		warmups := map[int]map[string]int{}
		// Orders the loop actually ran, per size and query ID of measured triples.
		type sizeQuery struct {
			size int
			id   string
		}
		orders := map[sizeQuery]map[[3]string]int{}
		if e := eachJSON(filepath.Join(c.Out, "delta-"+format, "answers.jsonl"), func(s sample) error {
			triple = append(triple, s)
			if len(triple) == 3 {
				if triple[0].Form != "base" {
					t.Fatal("dependent form precedes clean baseline", triple)
				}
				positions := map[int]bool{}
				for _, v := range triple {
					positions[v.TimingPosition] = true
				}
				if len(positions) != 3 || !positions[0] || !positions[1] || !positions[2] {
					t.Fatal("timing positions not retained", triple)
				}
				forms := map[string]bool{}
				for _, v := range triple {
					if v.Size != triple[0].Size || v.Repeat != triple[0].Repeat || !reflect.DeepEqual(v.Query, triple[0].Query) {
						t.Fatal("A/A samples are not query-paired", triple)
					}
					forms[v.Form] = true
				}
				if len(forms) != 3 {
					t.Fatal("missing paired form", triple)
				}
				if triple[0].Repeat >= 0 {
					var order [3]string
					for _, v := range triple {
						order[v.TimingPosition] = v.Form
					}
					key := sizeQuery{triple[0].Size, triple[0].Query.ID}
					if orders[key] == nil {
						orders[key] = map[[3]string]int{}
					}
					orders[key][order]++
				}
				triple = nil
			}
			if s.ElapsedNS <= 0 {
				t.Fatal("missing raw timing")
			}
			if s.Repeat < 0 {
				if warmups[s.Size] == nil {
					warmups[s.Size] = map[string]int{}
				}
				warmups[s.Size][s.Form]++
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
		if len(triple) != 0 {
			t.Fatal("incomplete timing triple", triple)
		}
		if len(orders) != len(queries)*len(metrics.Checkpoints) {
			t.Fatal("measured order evidence missing", format, len(orders))
		}
		for key, counts := range orders {
			if len(counts) != 6 {
				t.Fatal("query not timed in all six orders", format, key, counts)
			}
			for order, n := range counts {
				if n != 2 {
					t.Fatal("query order not timed exactly twice", format, key, order, n)
				}
			}
		}
		for _, cp := range metrics.Checkpoints {
			for _, form := range []string{"base", "wrapper", "delta"} {
				if warmups[cp.Size][form] != len(queries) {
					t.Fatal("warm-up evidence count differs", format, cp.Size, form, warmups)
				}
				for op, count := range queryCounts {
					if got := len(raw[cp.Size][form][op]); got != count*12 {
						t.Fatal("timed sample count differs", format, cp.Size, form, op, got, count*12)
					}
				}
			}
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
	// An absent reference match set is incomplete evidence, not a candidate miss.
	var qs []workload.Query
	if err = dataset.ReadJSON(filepath.Join(c.Out, "queries.json"), &qs); err != nil {
		t.Fatal(err)
	}
	lexical := false
	for _, q := range qs {
		if !workload.Lexical(q.Op) {
			continue
		}
		lexical = true
		refPath := filepath.Join(c.Out, "references", referenceKey(false, q)+".json")
		raw, e := os.ReadFile(refPath)
		if e != nil {
			t.Fatal(e)
		}
		var ref referenceRecord
		if e = json.Unmarshal(raw, &ref); e != nil || ref.MatchRepos == nil {
			t.Fatal("source-derived map missing", ref, e)
		}
		ref.MatchRepos = nil
		if e = dataset.WriteJSON(refPath, ref); e != nil {
			t.Fatal(e)
		}
		if e = verifyDeltas(ctx, c, "arrow"); e == nil || errors.Is(e, errAnswerMismatch) {
			t.Fatal("missing match map must block, not report a semantic miss", e)
		}
		var blocked verifyReport
		if e = dataset.ReadJSON(filepath.Join(c.Out, "verify-arrow.json"), &blocked); e != nil || blocked.Status != "blocked" || blocked.Error == "" {
			t.Fatal(blocked, e)
		}
		if e = os.WriteFile(refPath, raw, 0600); e != nil {
			t.Fatal(e)
		}
		break
	}
	if !lexical {
		t.Fatal("fixture has no lexical query")
	}
	// Corrupt one real serialized answer and require the checker caller to fail.
	path := filepath.Join(c.Out, "delta-arrow", "answers.jsonl")
	var rows []sample
	if err = eachJSON(path, func(s sample) error { rows = append(rows, s); return nil }); err != nil {
		t.Fatal(err)
	}
	cleanRows := slices.Clone(rows)
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
	writeRows := func(rows []sample) {
		t.Helper()
		file, err := os.Create(path)
		if err != nil {
			t.Fatal(err)
		}
		enc := json.NewEncoder(file)
		for _, s := range rows {
			if err := enc.Encode(s); err != nil {
				_ = file.Close()
				t.Fatal(err)
			}
		}
		if err = file.Close(); err != nil {
			t.Fatal(err)
		}
	}
	writeRows(rows)
	if err = verifyDeltas(ctx, c, "arrow"); !errors.Is(err, errAnswerMismatch) {
		t.Fatal("dropped edge did not produce a semantic mismatch", err)
	}
	var failed verifyReport
	if err = dataset.ReadJSON(filepath.Join(c.Out, "verify-arrow.json"), &failed); err != nil || failed.Status != "miss" || failed.Error == "" {
		t.Fatal("missing structured mismatch report", failed, err)
	}
	// A wrapper hash difference cannot be a semantic miss before its blobs verify.
	missingBlob := false
	for i := range cleanRows {
		if cleanRows[i].Form == "wrapper" {
			cleanRows[i].AnswerSHA256 = strings.Repeat("0", 64)
			missingBlob = true
			break
		}
	}
	if !missingBlob {
		t.Fatal("fixture has no wrapper observation")
	}
	writeRows(cleanRows)
	if err = verifyDeltas(ctx, c, "arrow"); err == nil || errors.Is(err, errAnswerMismatch) {
		t.Fatal("missing wrapper blob became semantic inequality", err)
	}
	if err = dataset.ReadJSON(filepath.Join(c.Out, "verify-arrow.json"), &failed); err != nil || failed.Status != "blocked" {
		t.Fatal("missing blob did not produce a blocked report", failed, err)
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
	}{{"unknown-step", func(c *config) { c.Step = "check3" }}, {"missing-data", func(c *config) { c.Data = "" }}, {"missing-engine-limit", func(c *config) { c.Limits.Engine = 0 }}, {"wrong-link-cap", func(c *config) { c.LinkLimit = linkCap * 2 }}, {"missing-wall", func(c *config) { c.WallSeconds = 0 }}, {"reordered-sizes", func(c *config) { c.Sizes = []int{2, 1} }}, {"approval-drift", func(c *config) { c.ApprovalSHA256 = strings.Repeat("0", 64) }}} {
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
	}{{"crossing", []checkpoint{{Size: 1, ReadVerdict: map[string]string{"op": "pass"}}, {Size: 4, ReadVerdict: map[string]string{"op": "pass"}}, {Size: 16, ReadVerdict: map[string]string{"op": "miss"}}}, "observed-read-bound-crossing", intPtr(4), nil}, {"no-crossing", []checkpoint{{Size: 1, ReadVerdict: map[string]string{"op": "pass"}}, {Size: 4, ReadVerdict: map[string]string{"op": "pass"}}}, "no-crossing-lower-bound-only", nil, intPtr(4)}, {"first-size-misses", []checkpoint{{Size: 1, ReadVerdict: map[string]string{"op": "miss"}}}, "crossing-at-smallest-size-threshold-unresolved", nil, nil}} {
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
		{"missing-disk", func(r *deltaReport) { r.Checkpoints[0].ByteRatio = 0 }},
		{"read", func(r *deltaReport) { r.Checkpoints[0].ReadVerdict["op"] = "miss" }},
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
	r.Checkpoints[0].ByteRatio = 2
	if !deltaPass(r) {
		t.Fatal("inclusive 2x byte bound refused")
	}
	r.MaxEditCPU = 9e9
	r.Visibility.P95 = 9e9
	r.Checkpoints = append(r.Checkpoints, checkpoint{Size: 32, MaxEditCPU: 9e9, Visibility: runner.Distribution{Count: 32, P95: 9e9}, WorstRatio: 1.2, ReadVerdict: map[string]string{"op": "miss"}, ByteRatio: 1})
	r.Threshold = intPtr(1)
	r.LowerBound = nil
	if !deltaPass(r) {
		t.Fatal("unserved diagnostics invalidated earlier regime")
	}
	if selected := selectDelta(map[string]formResult{"arrow": {Status: "pass", Report: r}, "parquet": {Status: "pass", Report: r}}); selected.Branch != "both-pass-lead-selects-simplest" {
		t.Fatal("later diagnostics triggered an edit-bound stop", selected)
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
	return deltaReport{MaxEditCPU: 1, Visibility: runner.Distribution{Count: 1, P95: 1}, Checkpoints: []checkpoint{{Size: 1, MaxEditCPU: 1, Visibility: runner.Distribution{Count: 1, P95: 1}, WorstRatio: 1, ReadVerdict: map[string]string{"op": "pass"}, ByteRatio: 1}}, LowerBound: intPtr(1)}
}
func completedReceipt(limit int64) externalResult {
	return externalResult{State: "completed", Removed: true, Memory: runner.ComponentMemory{PID: 1, LimitBytes: limit, PeakBytes: 1, PeakRSSBytes: 1, Status: "completed", ExitCode: 0}}
}
func TestSelectionAndLinkReceiptDecisions(t *testing.T) {
	pass := formResult{Status: "pass", Report: passingReport()}
	miss := formResult{Status: "miss"}
	blocked := formResult{Status: "blocked"}
	for _, tc := range []struct {
		name           string
		reports        map[string]formResult
		branch, status string
	}{
		{"arrow-other-measured-miss", map[string]formResult{"arrow": pass, "parquet": miss}, "arrow", "pass"},
		{"parquet-other-measured-miss", map[string]formResult{"parquet": pass, "arrow": miss}, "parquet", "pass"},
		{"arrow-other-blocked", map[string]formResult{"arrow": pass, "parquet": blocked}, "arrow-passes-parquet-blocked", "partial"},
		{"parquet-other-blocked", map[string]formResult{"parquet": pass, "arrow": blocked}, "parquet-passes-arrow-blocked", "partial"},
		{"absent-other-is-blocked", map[string]formResult{"arrow": pass}, "arrow-passes-parquet-blocked", "partial"},
		{"both", map[string]formResult{"arrow": pass, "parquet": pass}, "both-pass-lead-selects-simplest", "partial"},
		{"none", map[string]formResult{"arrow": miss, "parquet": miss}, "no-passing-form", "miss"},
		{"missing", map[string]formResult{}, "no-complete-form-evidence", "blocked"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := selectDelta(tc.reports)
			if r.Branch != tc.branch || r.Status != tc.status {
				t.Fatal(r)
			}
		})
	}
	a, p := passingReport(), passingReport()
	a.Checkpoints[0].MaxEditCPU = 3e9
	p.Checkpoints[0].MaxEditCPU = 2e9
	if r := selectDelta(map[string]formResult{"arrow": {Status: "miss", Report: a}, "parquet": {Status: "miss", Report: p}}); r.Branch != "stop-ask-user-both-edit-bound-misses" {
		t.Fatal(r)
	}
	// No read regime passes for Arrow. Its shortest completed edit still counts.
	a.Checkpoints[0].ReadVerdict["op"] = "miss"
	a = threshold(a)
	p.Checkpoints[0].Size = 4
	p.Checkpoints[0].Visibility.Count = 4
	p.LowerBound = intPtr(4)
	if r := selectDelta(map[string]formResult{"arrow": {Status: "miss", Report: a}, "parquet": {Status: "miss", Report: p}}); r.Branch != "stop-ask-user-both-edit-bound-misses" {
		t.Fatal("first-size crossing hid edit-bound user stop", r)
	}
	a, p = passingReport(), passingReport()
	a.Checkpoints[0].Visibility.P95 = 6e9
	p.Checkpoints[0].Visibility.P95 = 6e9
	if r := selectDelta(map[string]formResult{"arrow": {Status: "miss", Report: a}, "parquet": {Status: "miss", Report: p}}); r.Branch != "no-passing-form" || r.Status != "miss" {
		t.Fatal("unapproved visibility-specific stop", r)
	}
	rec := completedReceipt(linkCap)
	l := linkResult{Status: "pass"}
	parity := parityResult{Status: "pass"}
	if r := linkTarget("global", rec, l, parity, collisionStats{}); r.Status != "pass" {
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
			if got := linkTarget("global", r, l, p, collisionStats{}); got.Status != tc.status {
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
	for _, scenario := range []string{"partial", "parity-miss", "oom-arrow-failure", "link-crash", "missing-collisions", "unsafe-cleanup", "cancelled", "deadline-parquet", "references-failure", "verify-arrow-failure", "verify-arrow-oom", "verify-arrow-mismatch"} {
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
					case "link-crash":
						return failed(limit), errors.New("contained link crash")
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
				if task == "delta-parquet" && scenario == "deadline-parquet" {
					r = failed(limit)
					r.State = "deadline"
					return r, errors.New("worker deadline expired")
				}
				if task == "verify-arrow" && (scenario == "verify-arrow-oom" || scenario == "verify-arrow-mismatch") {
					r = failed(limit)
					if scenario == "verify-arrow-oom" {
						r.State = "oom"
						r.Memory.OOMKills = 1
					} else {
						write(filepath.Join(c.Out, task+".json"), verifyReport{Status: "miss", Error: "answer differs: fixture wrong edge"})
					}
					return r, errors.New("checker did not pass")
				}
				if (task == "delta-arrow" && scenario == "oom-arrow-failure") || (task == "verify-arrow" && scenario == "verify-arrow-failure") || (task == "references" && scenario == "references-failure") {
					return failed(limit), errors.New("contained failure")
				}
				switch task {
				case "prepare-link":
					if scenario != "missing-collisions" {
						write(filepath.Join(c.Out, "facts", "collisions.json"), collisionStats{})
					}
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
			byTarget := map[string]targetResult{}
			for _, target := range targets {
				byTarget[target.Target] = target
			}
			if scenario == "deadline-parquet" {
				if e == nil || byTarget["check4-arrow"].Status != "pass" || byTarget["check4-parquet"].Status != "blocked" || slices.Contains(calls, "verify-parquet") {
					t.Fatal("deadline lost earlier result or allowed another launch", calls, targets, e)
				}
				return
			}
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
			if scenario == "link-crash" || scenario == "missing-collisions" {
				if byTarget["check3-link-peak-and-parity"].Status != "blocked" || slices.Contains(calls, "link-package") {
					t.Fatal("unmeasured link failure became miss/retry", calls, targets)
				}
			}
			if scenario == "oom-arrow-failure" || scenario == "verify-arrow-failure" || scenario == "verify-arrow-oom" {
				if byTarget["check4-arrow"].Status != "blocked" || byTarget["check4-delta-selection"].Status != "partial" || byTarget["check4-delta-selection"].Branch != "parquet-passes-arrow-blocked" {
					t.Fatal("unknown Arrow result became a format choice", targets)
				}
			}
			if scenario == "verify-arrow-mismatch" {
				if byTarget["check4-arrow"].Status != "miss" || byTarget["check4-delta-selection"].Status != "pass" || byTarget["check4-delta-selection"].Branch != "parquet" {
					t.Fatal("measured mismatch lost its distinct status", targets)
				}
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

func TestLexicalReferenceRequiresExplicitMatchSet(t *testing.T) {
	ctx := context.Background()
	d := conformance.Fixture()
	q := workload.Query{ID: "absent-lexical", Op: "symbols", Repo: d.Repo, Key: "absentlexicaltoken987", K: -1}
	matches, err := matchRepositories(ctx, []dataset.Dataset{d}, q)
	if err != nil || matches == nil || len(matches) != 0 {
		t.Fatal(matches, err)
	}
	ref := referenceRecord{Query: q, Expected: workload.Expected{QueryID: q.ID}, MatchRepos: matches}
	raw, err := json.Marshal(ref)
	if err != nil {
		t.Fatal(err)
	}
	var read referenceRecord
	if err = json.Unmarshal(raw, &read); err != nil || read.MatchRepos == nil {
		t.Fatal("empty membership was erased by serialization", string(raw), err)
	}
	if err = compareReference(q, read.Expected, read.MatchRepos, workload.Answer{}); err != nil {
		t.Fatal("empty native answer refused with a valid empty set", err)
	}
	if err = compareReference(q, read.Expected, nil, workload.Answer{}); err == nil || errors.Is(err, errAnswerMismatch) {
		t.Fatal("missing reference membership must block", err)
	}
	if err = compareReference(q, read.Expected, read.MatchRepos, workload.Answer{Hits: []adapter.Hit{{ID: "non-member", Score: 1}}}); !errors.Is(err, errAnswerMismatch) {
		t.Fatal("non-member hit did not fail semantic validation", err)
	}
}

func TestLinkTargetShowsCollisionFactShares(t *testing.T) {
	collisions := collisionStats{NameScopeGroups: 2, ExtendedGroups: 1, NameScopeFacts: 6, ExtendedFacts: 3}
	r := linkTarget("global", completedReceipt(linkCap), linkResult{Status: "pass", Facts: 12}, parityResult{Status: "pass"}, collisions)
	m, ok := r.Measured.(linkMeasurement)
	if !ok || r.Status != "pass" || m.Collisions != collisions || m.NameScopeCollisionFactShare != .5 || m.OrdinalFactShare != .25 {
		t.Fatal(r)
	}
}

func TestWorkerDeadlineReservesControllerFinalization(t *testing.T) {
	c := fixtureConfig(t)
	if err := os.Mkdir(c.Out, 0700); err != nil {
		t.Fatal(err)
	}
	c.WallSeconds = 9000
	now := time.Date(2026, 10, 30, 12, 0, 0, 0, time.UTC)
	if err := writePolicies(c, now); err != nil {
		t.Fatal(err)
	}
	for _, task := range []string{"controller", "prepare", "references", "link-global", "delta-arrow", "verify-parquet"} {
		var policy struct {
			Schema   string
			Deadline time.Time
		}
		if err := dataset.ReadJSON(launchPolicy(c, task), &policy); err != nil {
			t.Fatal(err)
		}
		want := now.Add(9000 * time.Second)
		if task != "controller" {
			want = want.Add(-60 * time.Second)
		}
		if policy.Schema != "external-limits/v1" || !policy.Deadline.Equal(want) {
			t.Fatal(task, policy, want)
		}
	}
	for _, seconds := range []int{0, 30, 60} {
		bad := c
		bad.Out = t.TempDir()
		bad.WallSeconds = seconds
		if err := writePolicies(bad, now); err == nil {
			t.Fatal("budget without a finalization reserve accepted", seconds)
		}
		entries, err := os.ReadDir(bad.Out)
		if err != nil || len(entries) != 0 {
			t.Fatal("rejected budget created policies", entries, err)
		}
	}
}

func TestExpiredWorkerPolicyStillWritesBlockedSummary(t *testing.T) {
	c := fixtureConfig(t)
	c.WallSeconds = 9000
	if err := os.Mkdir(c.Out, 0700); err != nil {
		t.Fatal(err)
	}
	if err := prepare(context.Background(), c); err != nil {
		t.Fatal(err)
	}
	if err := dataset.WriteJSON(filepath.Join(c.Out, "config.json"), c); err != nil {
		t.Fatal(err)
	}
	// Expired workers, but 59 seconds remain for the controller. No sleep or launch.
	if err := writePolicies(c, time.Now().Add(-8941*time.Second)); err != nil {
		t.Fatal(err)
	}
	if err := control(c); err == nil || !strings.Contains(err.Error(), "worker deadline expired") {
		t.Fatal(err)
	}
	var summary runSummary
	if err := dataset.ReadJSON(filepath.Join(c.Out, "result.json"), &summary); err != nil {
		t.Fatal(err)
	}
	if summary.Complete || len(summary.Targets) != 4 || summary.Protected != "unchanged" || summary.Error == "" {
		t.Fatal(summary)
	}
	for _, target := range summary.Targets {
		if target.Status != "blocked" || !strings.Contains(target.Reason, "worker deadline expired") {
			t.Fatal(target)
		}
	}
	if reason := unfinishedReason(nil); reason == "" || strings.Contains(reason, "<nil>") {
		t.Fatal(reason)
	}
	if _, err := os.Stat(filepath.Join(c.Out, "prepare-binding.json")); !os.IsNotExist(err) {
		t.Fatal("expired worker was prepared for launch", err)
	}
}

func TestDeltaProgressCrashHelper(t *testing.T) {
	configPath := os.Getenv("LINKDELTA_PROGRESS_CONFIG")
	if configPath == "" {
		return
	}
	var c config
	if err := dataset.ReadJSON(configPath, &c); err != nil {
		t.Fatal(err)
	}
	stage := os.Getenv("LINKDELTA_PROGRESS_STAGE")
	adapter.Checkpoint = func(s string) {
		if s == stage {
			os.Exit(99)
		}
	}
	if err := measureDeltas(context.Background(), c, "arrow"); err != nil {
		t.Fatal(err)
	}
	t.Fatal("progress checkpoint was unreachable")
}

func TestCompletedDeltaProgressSurvivesWorkerExit(t *testing.T) {
	for _, stage := range []string{"linkdelta-after-edit", "linkdelta-after-checkpoint"} {
		t.Run(stage, func(t *testing.T) {
			c := fixtureConfig(t)
			if err := os.Mkdir(c.Out, 0700); err != nil {
				t.Fatal(err)
			}
			if err := prepare(context.Background(), c); err != nil {
				t.Fatal(err)
			}
			if err := os.Mkdir(filepath.Join(c.Out, "answer-blobs"), 0700); err != nil {
				t.Fatal(err)
			}
			configPath := filepath.Join(c.Out, "progress-fixture.json")
			if err := dataset.WriteJSON(configPath, c); err != nil {
				t.Fatal(err)
			}
			cmd := exec.Command(os.Args[0], "-test.run=^TestDeltaProgressCrashHelper$")
			cmd.Env = append(os.Environ(), "LINKDELTA_PROGRESS_CONFIG="+configPath, "LINKDELTA_PROGRESS_STAGE="+stage)
			output, err := cmd.CombinedOutput()
			var exit *exec.ExitError
			if !errors.As(err, &exit) || exit.ExitCode() != 99 {
				t.Fatal("unexpected helper outcome", err, string(output))
			}
			var edits []editSample
			if err = eachJSON(filepath.Join(c.Out, "delta-arrow", "edits.jsonl"), func(s editSample) error { edits = append(edits, s); return nil }); err != nil || len(edits) != 1 || edits[0].Size != 1 || edits[0].CPU <= 0 || edits[0].Visibility <= 0 {
				t.Fatal("completed edit lost", edits, err)
			}
			var points []checkpoint
			if err = eachJSON(filepath.Join(c.Out, "delta-arrow", "checkpoints.jsonl"), func(p checkpoint) error { points = append(points, p); return nil }); err != nil {
				t.Fatal(err)
			}
			want := 0
			if stage == "linkdelta-after-checkpoint" {
				want = 1
			}
			if len(points) != want || (want == 1 && (points[0].Size != 1 || points[0].Visibility.Count != 1 || len(points[0].BaseP95) == 0)) {
				t.Fatal("completed checkpoint lost", points)
			}
			if _, err = os.Stat(filepath.Join(c.Out, "delta-arrow", "metrics.json")); !os.IsNotExist(err) {
				t.Fatal("aborted worker produced final metrics", err)
			}
			pins, err := outputPins(c.Out)
			if err != nil || !digest(pins["delta-arrow/edits.jsonl"]) || !digest(pins["delta-arrow/checkpoints.jsonl"]) {
				t.Fatal("terminal hashing omitted progress", pins, err)
			}
		})
	}
}

func TestCleanReferenceSourceValidationIsNotRepeated(t *testing.T) {
	for _, scenario := range []string{"cached", "invalid-first-source"} {
		t.Run(scenario, func(t *testing.T) {
			invalidFirst := scenario == "invalid-first-source"
			c := fixtureConfig(t)
			c.Sizes = []int{1, 2, 3}
			if err := os.Mkdir(c.Out, 0700); err != nil {
				t.Fatal(err)
			}
			if err := prepare(context.Background(), c); err != nil {
				t.Fatal(err)
			}
			if err := os.Mkdir(filepath.Join(c.Out, "answer-blobs"), 0700); err != nil {
				t.Fatal(err)
			}
			source, hidden := filepath.Join(c.Out, "changed-source"), filepath.Join(c.Out, "hidden-source")
			moved := false
			defer func() {
				if moved {
					if err := os.Rename(hidden, source); err != nil {
						t.Error(err)
					}
				}
			}()
			previous := adapter.Checkpoint
			defer func() { adapter.Checkpoint = previous }()
			count := 0
			var hookErr error
			if invalidFirst {
				if err := os.Rename(source, hidden); err != nil {
					t.Fatal(err)
				}
				moved = true
			} else {
				adapter.Checkpoint = func(stage string) {
					if stage == "linkdelta-after-checkpoint" {
						count++
						if count == 1 {
							hookErr = os.Rename(source, hidden)
							moved = hookErr == nil
						}
					}
				}
			}
			err := measureDeltas(context.Background(), c, "arrow")
			if invalidFirst {
				if err == nil {
					t.Fatal("source missing before its first build was accepted")
				}
				return
			}
			if err != nil || hookErr != nil || count != 3 || !moved {
				t.Fatal("later checkpoints reopened the already-built source", count, err, hookErr)
			}
		})
	}
}

func TestNativeLexicalRequiresFullCountWithoutRankEquivalence(t *testing.T) {
	wanted := []adapter.Hit{{ID: "a", Score: 4}, {ID: "b", Score: 3}, {ID: "c", Score: 2}, {ID: "d", Score: 1}}
	native := []adapter.Hit{{ID: "d", Score: 4}, {ID: "c", Score: 3}, {ID: "b", Score: 2}, {ID: "a", Score: 1}}
	matches := map[string]string{"a": "r", "b": "r", "c": "r", "d": "r"}
	for _, k := range []int{-1, 0, 1, 2, 8} {
		q := workload.Query{ID: "native-cardinality", Op: "symbols", Repo: "r", K: k, ExactRank: true}
		count := len(wanted)
		if k >= 0 {
			count = min(k, count)
		}
		want := workload.Expected{QueryID: q.ID, Answer: workload.Answer{Hits: wanted[:count]}}
		full := workload.Answer{Hits: native[:count]}
		if err := compareReference(q, want, matches, full); err != nil {
			t.Fatal("full native result incorrectly required rank equivalence", k, err)
		}
		if count > 0 {
			invalid := slices.Clone(native[:count])
			invalid[0].ID = "non-member"
			if err := compareReference(q, want, matches, workload.Answer{Hits: invalid}); !errors.Is(err, errAnswerMismatch) {
				t.Fatal("full count bypassed native membership", k, err)
			}
		}
		if count > 1 {
			duplicate := slices.Clone(native[:count])
			duplicate[1] = duplicate[0]
			if err := compareReference(q, want, matches, workload.Answer{Hits: duplicate}); !errors.Is(err, errAnswerMismatch) {
				t.Fatal("full count bypassed duplicate rejection", k, err)
			}
			underfilled := workload.Answer{Hits: native[:count-1]}
			if err := compareReference(q, want, matches, underfilled); !errors.Is(err, errAnswerMismatch) {
				t.Fatal("valid but underfilled nonempty result accepted", k, err)
			}
		}
	}
}

func TestMatchRepositoriesMatchesOracleAcrossRepositories(t *testing.T) {
	ctx := context.Background()
	var memory, disk []dataset.Dataset
	view := map[string]adapter.Segment{}
	for _, repo := range []string{"one", "two"} {
		d := conformance.Fixture()
		d.Repo = repo
		for i := range d.Nodes {
			d.Nodes[i].ID = repo + "/" + d.Nodes[i].ID
		}
		for i := range d.Edges {
			d.Edges[i].From = repo + "/" + d.Edges[i].From
			d.Edges[i].To = repo + "/" + d.Edges[i].To
		}
		for i := range d.Symbols {
			d.Symbols[i].NodeID = repo + "/" + d.Symbols[i].NodeID
		}
		for i := range d.Content {
			d.Content[i].NodeID = repo + "/" + d.Content[i].NodeID
			d.Content[i].ChunkID = repo + "/" + d.Content[i].ChunkID
		}
		for i := range d.Vectors {
			d.Vectors[i].NodeID = repo + "/" + d.Vectors[i].NodeID
		}
		d.Symbols[1].Tokens += " symbolonly"
		d.Content[0].Body += " textonly"
		memory = append(memory, d)
		view[repo] = oracle.New(d)
		dir := filepath.Join(t.TempDir(), repo)
		if _, err := dataset.Write(dir, d); err != nil {
			t.Fatal(err)
		}
		source, err := dataset.OpenSource(dir)
		if err != nil {
			t.Fatal(err)
		}
		disk = append(disk, source)
	}
	queries := []workload.Query{
		{Op: "symbols", Repo: "one", Key: "graph node", K: 10},
		{Op: "symbols", Repo: "two", Key: "symbolonly", K: 10},
		{Op: "content", Repo: "one", Key: "textonly", K: 10},
		{Op: "content", Repo: "two", Key: "graph node", K: 10},
		{Op: "multi_symbols", Key: "symbolonly", K: 10},
		{Op: "multi_symbols", Key: "graph node", K: 10},
		{Op: "symbols", Repo: "two", Key: "absentterm987", K: 10},
		{Op: "content", Repo: "one", Key: "absentterm987", K: 10},
		{Op: "multi_symbols", Key: "absentterm987", K: 10},
	}
	var expected []workload.Expected
	for i := range queries {
		q := &queries[i]
		q.ID = q.Op + ":" + q.Repo + ":" + q.Key
		answer, err := workload.Evaluate(ctx, view, adapter.Fanout{Segments: view}, *q)
		if err != nil {
			t.Fatal(err)
		}
		expected = append(expected, workload.Expected{QueryID: q.ID, Answer: answer})
	}
	references, err := oracle.ReferenceAnswers(memory, queries, expected)
	if err != nil {
		t.Fatal(err)
	}
	for i, q := range queries {
		t.Run(q.ID, func(t *testing.T) {
			matches, err := matchRepositories(ctx, disk, q)
			if err != nil || matches == nil || !reflect.DeepEqual(matches, references[i].MatchRepos) {
				t.Fatal("streamed match set differs from oracle", matches, references[i].MatchRepos, err)
			}
			for _, repo := range matches {
				if q.Repo != "" && repo != q.Repo {
					t.Fatal("local query included another repository", q, matches)
				}
			}
		})
	}
	cancelled, cancel := context.WithCancel(ctx)
	cancel()
	if _, err := matchRepositories(cancelled, disk, queries[0]); !errors.Is(err, context.Canceled) {
		t.Fatal("cancelled derivation was not refused", err)
	}
}
