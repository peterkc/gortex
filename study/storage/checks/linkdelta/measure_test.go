package main

import (
	"context"
	"encoding/json"
	"gxstudy/conformance"
	"gxstudy/dataset"
	"gxstudy/runner"
	"gxstudy/workload"
	"os"
	"path/filepath"
	"reflect"
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
	if err := dataset.WriteJSON(filepath.Join(tier, "expected.json"), []workload.Expected{}); err != nil {
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
		if len(rows[i].Answer.Edges) > 0 {
			rows[i].Answer.Edges = rows[i].Answer.Edges[1:]
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
	r := deltaReport{MaxEditCPU: 1000000001, Visibility: runner.Distribution{P95: 1}, Checkpoints: []checkpoint{{Size: 1, WorstRatio: 1, ByteRatio: 1}}}
	if deltaPass(r) {
		t.Fatal("edit target relaxed")
	}
	r.MaxEditCPU = 1
	r.Visibility.P95 = 5000000001
	if deltaPass(r) {
		t.Fatal("visibility target relaxed")
	}
	r.Visibility.P95 = 1
	r.Checkpoints[0].ByteRatio = 2.01
	if deltaPass(r) {
		t.Fatal("disk target relaxed")
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
