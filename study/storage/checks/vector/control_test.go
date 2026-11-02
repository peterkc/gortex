package main

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"gxstudy/runner"
)

func TestSelectionBranches(t *testing.T) {
	for _, test := range []struct {
		name           string
		fExact         bool
		fMS            float64
		iExact         bool
		iMS            float64
		status, branch string
	}{
		{"f32-first", true, 50, false, 10, "pass", "keep_exact_f32_int8_extra_evidence"},
		{"synthetic-int8-not-a-recommendation", true, 51, true, 49, "partial", "synthetic_only_return_user_for_real_vector_int8_decision"},
		{"one-approximate-result", true, 51, false, 49, "miss", "return_miss_to_user"},
		{"both-latency-miss", true, 51, true, 51, "miss", "both_latency_miss_return_user_ann_diagnostic_eligible_not_run"},
		{"f32-not-exact", false, 49, false, 49, "miss", "return_miss_to_user"},
	} {
		t.Run(test.name, func(t *testing.T) {
			got := selectResult(targetResult{Exact: test.fExact, P95MS: test.fMS}, targetResult{Exact: test.iExact, P95MS: test.iMS})
			if got.Status != test.status || got.Branch != test.branch || got.ANNRun || !got.SyntheticOnly {
				t.Fatal(got)
			}
		})
	}
}
func TestNearestRankP95(t *testing.T) {
	samples := []sample{{Phase: "warmup", LatencyNS: 999999999}}
	for i := 1; i <= 20; i++ {
		samples = append(samples, sample{Phase: "measure", LatencyNS: int64(i) * 1000000})
	}
	got, err := p95(samples)
	if err != nil || got != 19 {
		t.Fatal(got, err)
	}
	if _, err := p95(nil); err == nil {
		t.Fatal("missing measurements accepted")
	}
}
func TestExplicitRunArgumentsRefuseBeforeOutput(t *testing.T) {
	out := filepath.Join(t.TempDir(), "new-output")
	if err := dispatch(context.Background(), []string{"run", "-out", out}); err == nil {
		t.Fatal("missing explicit inputs accepted")
	}
	if _, err := os.Stat(out); !os.IsNotExist(err) {
		t.Fatal("refusal created output", err)
	}
	if err := dispatch(context.Background(), []string{"generate", "-out", out}); err == nil {
		t.Fatal("missing generator arguments accepted")
	}
	if _, err := os.Stat(out); !os.IsNotExist(err) {
		t.Fatal("refusal created fixture", err)
	}
	if err := dispatch(context.Background(), []string{"ann"}); err == nil {
		t.Fatal("ANN cannot run by default or as an ungated command")
	}
}
func TestEntrypointRequiresBinaryAndArguments(t *testing.T) {
	output, err := exec.Command("bash", "run.sh").CombinedOutput()
	if err == nil || !strings.Contains(string(output), "Usage:") {
		t.Fatal(string(output), err)
	}
}
func TestApprovalIdentityAndLimits(t *testing.T) {
	want := approval{true, testRevision, testGenerator, testGenerator, testGenerator, "host", "operator", 0, 600, runner.MemoryLimits{Runner: 134217728, Checker: 1073741824, Engine: 1073741824}}
	if err := checkApproval(want, want); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"approval", "host", "operator", "cap", "limits", "input"} {
		t.Run(name, func(t *testing.T) {
			got := want
			switch name {
			case "approval":
				got.Approved = false
			case "host":
				got.Host = "other"
			case "operator":
				got.Operator = ""
			case "cap":
				got.PriceCapUSD = -1
			case "limits":
				got.Limits.Engine *= 2
			case "input":
				got.FixtureSHA256 = strings.Repeat("3", 64)
			}
			if err := checkApproval(got, want); err == nil {
				t.Fatal("approval drift accepted")
			}
		})
	}
}
func TestFailedComponentCannotProducePass(t *testing.T) {
	out := t.TempDir()
	c := runConfig{Out: out, Data: t.TempDir(), FixturePin: testGenerator, Launcher: "/missing/limited-command", Executable: "/missing/vector-check", Approval: approval{SourceRevision: testRevision, LauncherSHA256: testGenerator, Host: "host", Limits: runner.MemoryLimits{Runner: 4096, Checker: 4096, Engine: 4096}}}
	if err := controlled(context.Background(), c, fixtureManifest{}); err == nil {
		t.Fatal("failed launcher accepted")
	}
	var got result
	if err := readJSON(filepath.Join(out, "result.json"), &got); err != nil {
		t.Fatal(err)
	}
	if got.Status != "blocked" || got.Branch != "execution_or_receipt_failure_return_user" {
		t.Fatal(got)
	}
	var receipt runReceipt
	if err := readJSON(filepath.Join(out, "run-receipt.json"), &receipt); err != nil {
		t.Fatal(err)
	}
	if receipt.State != "failed" || receipt.Error == "" || len(receipt.Outputs) == 0 {
		t.Fatal("failed attempt evidence absent", receipt)
	}
	if !reflect.DeepEqual(receipt.Config, c) {
		t.Fatal("request binding changed")
	}
}
