//go:build linux

package runner

import (
	"context"
	"gxstudy/dataset"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestExternalLimitsDeadlineOOMAndCleanup(t *testing.T) {
	root := testCgroupRoot(t)
	python, err := exec.LookPath("python3.12")
	if err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		name, script, state string
		seconds             int
	}{
		{"normal", `import os,pathlib
p=pathlib.Path('/sys/fs/cgroup')/pathlib.Path('/proc/self/cgroup').read_text().strip().split('::')[1].lstrip('/')
assert (p/'memory.max').read_text().strip() == '67108864'
assert (p/'memory.swap.max').read_text().strip() == '0'
assert (p/'memory.oom.group').read_text().strip() == '1'
print('within-limit', flush=True)`, "completed", 10},
		{"oom", "print('before-oom',flush=True); x=bytearray(128*1024*1024)", "oom", 10},
		{"deadline", "import signal; print('before-deadline',flush=True); signal.pause()", "deadline", 2},
		{"failure", "print('partial-failure',flush=True); raise SystemExit(7)", "failed", 10},
		{"descendant", "import subprocess,sys; subprocess.Popen([sys.executable,'-c','import signal; signal.pause()']); print('spawned',flush=True)", "completed", 10},
	} {
		t.Run(test.name, func(t *testing.T) {
			data, policy := filepath.Join(t.TempDir(), "data.json"), filepath.Join(t.TempDir(), "policy.json")
			if err := os.WriteFile(data, []byte("frozen-fixture"), 0600); err != nil {
				t.Fatal(err)
			}
			deadline := time.Now().Add(time.Duration(test.seconds) * time.Second)
			if err := dataset.WriteJSON(policy, externalPolicy{Schema: "external-limits/v1", Deadline: deadline}); err != nil {
				t.Fatal(err)
			}
			binding := fixtureBinding(t, "", []string{python, "-c", test.script})
			binding.Limits.Engine = 64 << 20
			binding.DataSHA256, err = dataset.Hash(data)
			if err != nil {
				t.Fatal(err)
			}
			binding.RecipeSHA256, err = dataset.Hash(policy)
			if err != nil {
				t.Fatal(err)
			}
			bindingPath := filepath.Join(t.TempDir(), "binding.json")
			if err := dataset.WriteJSON(bindingPath, binding); err != nil {
				t.Fatal(err)
			}
			pin, err := dataset.Hash(bindingPath)
			if err != nil {
				t.Fatal(err)
			}
			out := filepath.Join(t.TempDir(), "out")
			args := []string{"-binding", bindingPath, "-binding-pin", pin, "-policy", policy, "-data", data, "-out", out, "-cgroup-root", root, "--"}
			args = append(args, binding.Command...)
			runErr := RunExternal(args)
			if (runErr == nil) != (test.state == "completed") {
				t.Fatalf("unexpected command result: %v", runErr)
			}
			var receipt externalReceipt
			if err := dataset.ReadJSON(filepath.Join(out, "receipt.json"), &receipt); err != nil {
				t.Fatal(err)
			}
			if receipt.State != test.state || !receipt.Removed || receipt.Memory.PeakBytes <= 0 || receipt.Memory.LimitBytes != 64<<20 {
				t.Fatalf("lost limit/terminal evidence: %+v", receipt)
			}
			if _, err := os.Stat(receipt.Cgroup); !os.IsNotExist(err) {
				t.Fatal("owned scope leaked", err)
			}
			if test.state == "oom" && receipt.Memory.OOMKills < 1 {
				t.Fatal("OOM not reported")
			}
			log, err := os.ReadFile(filepath.Join(out, "command.log"))
			if err != nil || len(log) == 0 || receipt.LogSHA256 == "" {
				t.Fatal("partial output lost", err)
			}
			if test.state == "deadline" && !strings.Contains(string(log), "before-deadline") {
				t.Fatal("deadline output lost")
			}
			if err := RunExternal(args); err == nil {
				t.Fatal("existing output overwritten")
			}
		})
	}
}

func TestExternalRefusesUndelegatedRootBeforeCommand(t *testing.T) {
	out := t.TempDir()
	marker := filepath.Join(out, "must-not-start")
	binding := fixtureBinding(t, "", []string{"/bin/sh", "-c", "touch " + marker})
	err := runExternal(context.Background(), t.TempDir(), out, binding, strings.Repeat("a", 64), time.Now().Add(time.Minute))
	if err == nil {
		t.Fatal("undelegated cgroup accepted")
	}
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Fatal("command started before refusal")
	}
	var receipt externalReceipt
	if err := dataset.ReadJSON(filepath.Join(out, "receipt.json"), &receipt); err != nil {
		t.Fatal(err)
	}
	if receipt.State != "refused" || receipt.Removed || receipt.Memory.PID != 0 {
		t.Fatal("refusal reported as execution", receipt)
	}
}

func TestExternalMissingBindingStartsNothing(t *testing.T) {
	out := filepath.Join(t.TempDir(), "out")
	if err := RunExternal([]string{"-out", out, "--", "/bin/true"}); err == nil {
		t.Fatal("missing binding accepted")
	}
	if _, err := os.Stat(out); !os.IsNotExist(err) {
		t.Fatal("invalid binding produced outputs")
	}
}

func TestExternalRejectsUnboundInputsAndExpiredPolicy(t *testing.T) {
	for _, kind := range []string{"pin", "data", "policy", "expired", "command", "unknown-policy"} {
		t.Run(kind, func(t *testing.T) {
			directory := t.TempDir()
			data := filepath.Join(directory, "data")
			policy := filepath.Join(directory, "policy.json")
			if err := os.WriteFile(data, []byte("fixture"), 0600); err != nil {
				t.Fatal(err)
			}
			deadline := time.Now().Add(time.Minute)
			if kind == "expired" {
				deadline = time.Now().Add(-time.Minute)
			}
			if err := dataset.WriteJSON(policy, externalPolicy{Schema: "external-limits/v1", Deadline: deadline}); err != nil {
				t.Fatal(err)
			}
			if kind == "unknown-policy" {
				if err := os.WriteFile(policy, []byte(`{"schema":"external-limits/v1","unknown":1}`), 0600); err != nil {
					t.Fatal(err)
				}
			}
			binding := fixtureBinding(t, "", []string{"/bin/true"})
			var err error
			binding.DataSHA256, err = dataset.Hash(data)
			if err != nil {
				t.Fatal(err)
			}
			binding.RecipeSHA256, err = dataset.Hash(policy)
			if err != nil {
				t.Fatal(err)
			}
			if kind == "data" {
				binding.DataSHA256 = strings.Repeat("0", 64)
			}
			if kind == "policy" {
				binding.RecipeSHA256 = strings.Repeat("0", 64)
			}
			if kind == "command" {
				binding.Command = []string{"/bin/false"}
			}
			bindingPath := filepath.Join(directory, "binding.json")
			if err := dataset.WriteJSON(bindingPath, binding); err != nil {
				t.Fatal(err)
			}
			pin, err := dataset.Hash(bindingPath)
			if err != nil {
				t.Fatal(err)
			}
			if kind == "pin" {
				pin = strings.Repeat("0", 64)
			}
			out := filepath.Join(directory, "out")
			args := []string{"-binding", bindingPath, "-binding-pin", pin, "-policy", policy, "-data", data, "-cgroup-root", t.TempDir(), "-out", out, "--", "/bin/true"}
			if err := RunExternal(args); err == nil {
				t.Fatal("unbound input accepted", kind)
			}
			if _, err := os.Stat(out); !os.IsNotExist(err) {
				t.Fatal("invalid input created output", kind)
			}
		})
	}
}
