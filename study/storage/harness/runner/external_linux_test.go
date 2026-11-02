//go:build linux

package runner

import (
	"context"
	"crypto/sha256"
	"fmt"
	"gxstudy/dataset"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

func TestExternalLimitsDeadlineOOMAndCleanup(t *testing.T) {
	root := testCgroupRoot(t)
	t.Setenv("GXSTUDY_UNAPPROVED", "must-not-reach-child")
	t.Setenv("PYTHONDONTWRITEBYTECODE", "1")
	python, err := exec.LookPath("python3.12")
	if err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		name, script, state string
		seconds             int
	}{
		{"normal", `import os,pathlib
assert 'GXSTUDY_UNAPPROVED' not in os.environ
assert os.environ['PYTHONDONTWRITEBYTECODE'] == '1'
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
			expectedEnvironment := fmt.Sprintf("%x", sha256.Sum256([]byte(strings.Join(externalEnvironment(os.Environ()), "\x00")+"\x00")))
			if receipt.EnvironmentSHA256 != expectedEnvironment {
				t.Fatal("child environment was not bound", receipt.EnvironmentSHA256)
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
	for _, kind := range []string{"pin", "data", "policy", "expired", "command", "relative-command", "unknown-policy"} {
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
				if err := dataset.WriteJSON(policy, map[string]any{"schema": "external-limits/v1", "deadline": deadline, "unknown": 1}); err != nil {
					t.Fatal(err)
				}
			}
			command := []string{"/bin/true"}
			if kind == "relative-command" {
				command = []string{"true"}
			}
			binding := fixtureBinding(t, "", command)
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
			args := []string{"-binding", bindingPath, "-binding-pin", pin, "-policy", policy, "-data", data, "-cgroup-root", t.TempDir(), "-out", out, "--"}
			args = append(args, command...)
			if err := RunExternal(args); err == nil {
				t.Fatal("unbound input accepted", kind)
			}
			if _, err := os.Stat(out); !os.IsNotExist(err) {
				t.Fatal("invalid input created output", kind)
			}
		})
	}
}

func TestExternalEnvironmentIsExplicitStableAndNonmutating(t *testing.T) {
	ambient := []string{"PATH=first", "AWS_SECRET_ACCESS_KEY=secret", "GITHUB_TOKEN=secret", "RUNNER_TEMP=other", "OUT=implicit", "PRIME_SESSION_ID=other", "PATH=last", "HOME=/fixture", "GOTOOLCHAIN=local", "GOMEMLIMIT=1GiB", "UV_PROJECT_ENVIRONMENT=/env", "HF_HUB_OFFLINE=1", "PYTHONDONTWRITEBYTECODE=1"}
	before := slices.Clone(ambient)
	want := []string{"GOMEMLIMIT=1GiB", "GOTOOLCHAIN=local", "HF_HUB_OFFLINE=1", "HOME=/fixture", "PATH=last", "PYTHONDONTWRITEBYTECODE=1", "UV_PROJECT_ENVIRONMENT=/env"}
	if got := externalEnvironment(ambient); !slices.Equal(got, want) {
		t.Fatal("unexpected child environment", got)
	}
	if !slices.Equal(ambient, before) {
		t.Fatal("ambient environment mutated")
	}
	if got := externalEnvironment(nil); got == nil || len(got) != 0 {
		t.Fatal("empty environment would inherit ambient values", got)
	}
}

func TestExternalCommandRequiresKillBeforeStartAndBoundsWait(t *testing.T) {
	scope := &memoryScope{path: t.TempDir()}
	binding := RunBinding{Command: []string{"/bin/true"}}
	cmd, release, err := externalCommand(context.Background(), binding, scope)
	if err == nil || cmd != nil || release != nil || !strings.Contains(err.Error(), "cgroup.kill") {
		t.Fatal("missing cgroup.kill did not refuse setup", cmd, err)
	}
	if err := os.WriteFile(filepath.Join(scope.path, "cgroup.kill"), nil, 0600); err != nil {
		t.Fatal(err)
	}
	// Setup only: this ordinary temporary directory is never used to start a child.
	cmd, release, err = externalCommand(context.Background(), binding, scope)
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	if cmd.Process != nil || cmd.WaitDelay != defaultSafetyTimeout || cmd.Cancel == nil || cmd.Env == nil {
		t.Fatal("unbounded or premature command setup", cmd)
	}
}

func TestExternalCancelledBeforeStartRemovesFreshScope(t *testing.T) {
	root := testCgroupRoot(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	out := t.TempDir()
	binding := fixtureBinding(t, "", []string{"/bin/true"})
	if err := runExternal(ctx, root, out, binding, strings.Repeat("a", 64), time.Now().Add(time.Minute)); err == nil {
		t.Fatal("cancelled launch accepted")
	}
	var receipt externalReceipt
	if err := dataset.ReadJSON(filepath.Join(out, "receipt.json"), &receipt); err != nil {
		t.Fatal(err)
	}
	if receipt.State != "refused" || !receipt.Removed || receipt.Memory.PID != 0 {
		t.Fatal("unstarted scope not removed", receipt)
	}
	if _, err := os.Stat(receipt.Cgroup); !os.IsNotExist(err) {
		t.Fatal("unstarted scope leaked", err)
	}
}
