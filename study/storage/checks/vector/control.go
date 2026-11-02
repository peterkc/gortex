package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"time"

	"gxstudy/dataset"
	"gxstudy/runner"
)

type approval struct {
	Approved         bool                `json:"approved"`
	SourceRevision   string              `json:"source_revision"`
	ExecutableSHA256 string              `json:"executable_sha256"`
	LauncherSHA256   string              `json:"launcher_sha256"`
	FixtureSHA256    string              `json:"fixture_sha256"`
	Host             string              `json:"host"`
	Operator         string              `json:"operator"`
	PriceCapUSD      float64             `json:"price_cap_usd"`
	WallTimeSeconds  float64             `json:"wall_time_seconds"`
	Limits           runner.MemoryLimits `json:"memory_limits_bytes"`
}
type runConfig struct {
	Data           string    `json:"data"`
	FixturePin     string    `json:"fixture_sha256"`
	Out            string    `json:"out"`
	Launcher       string    `json:"launcher"`
	Executable     string    `json:"executable"`
	CgroupRoot     string    `json:"cgroup_root"`
	Workers        int       `json:"workers"`
	K              int       `json:"k"`
	Candidates     int       `json:"candidates"`
	Deadline       time.Time `json:"deadline"`
	Approval       approval  `json:"approval"`
	ApprovalSHA256 string    `json:"approval_sha256"`
}
type targetResult struct {
	Target string  `json:"target"`
	P95MS  float64 `json:"measured_p95_ms"`
	Exact  bool    `json:"exact"`
	Status string  `json:"status"`
}
type result struct {
	Status        string         `json:"status"`
	Branch        string         `json:"branch_taken"`
	SyntheticOnly bool           `json:"synthetic_only"`
	ANNRun        bool           `json:"ann_run"`
	Targets       []targetResult `json:"targets"`
}
type runReceipt struct {
	Runtime    string                     `json:"go_runtime"`
	Platform   string                     `json:"platform"`
	Config     runConfig                  `json:"config"`
	Fixture    fixtureManifest            `json:"fixture"`
	State      string                     `json:"state"`
	Error      string                     `json:"error,omitempty"`
	Components map[string]json.RawMessage `json:"components"`
	Outputs    map[string]string          `json:"output_sha256"`
	Accounting string                     `json:"accounting"`
}

func runCLI(args []string) error {
	fs := flag.NewFlagSet("run", flag.ContinueOnError)
	var c runConfig
	var approvePath, approvePin, wall string
	var limits runner.MemoryLimits
	fs.StringVar(&c.Data, "data", "", "explicit fixture directory")
	fs.StringVar(&c.FixturePin, "fixture-pin", "", "explicit manifest SHA256")
	fs.StringVar(&c.Out, "out", "", "fresh output directory")
	fs.StringVar(&c.Launcher, "launcher", "", "accepted limited-command executable")
	fs.StringVar(&c.CgroupRoot, "cgroup-root", "", "delegated root; caller already inside it")
	fs.StringVar(&approvePath, "approval", "", "operator-supplied user approval receipt")
	fs.StringVar(&approvePin, "approval-pin", "", "approved receipt SHA256")
	fs.StringVar(&wall, "wall-time", "", "explicit duration bound; no default")
	fs.Int64Var(&limits.Runner, "runner-bytes", 0, "explicit page-aligned runner cap")
	fs.Int64Var(&limits.Checker, "checker-bytes", 0, "explicit page-aligned checker cap")
	fs.Int64Var(&limits.Engine, "engine-bytes", 0, "explicit page-aligned engine cap")
	fs.IntVar(&c.Workers, "workers", 0, "explicit workers, 1..4")
	fs.IntVar(&c.K, "k", 0, "explicit top k")
	fs.IntVar(&c.Candidates, "candidates", 0, "explicit global int8 candidate pool")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 0 || !filepath.IsAbs(c.Data) || !filepath.IsAbs(c.Out) || !filepath.IsAbs(c.Launcher) || !filepath.IsAbs(c.CgroupRoot) || !filepath.IsAbs(approvePath) {
		return fmt.Errorf("data, out, launcher, cgroup-root and approval must be explicit absolute paths")
	}
	duration, err := time.ParseDuration(wall)
	if err != nil || duration <= 0 {
		return fmt.Errorf("explicit positive wall-time required")
	}
	c.Deadline = time.Now().Add(duration)
	if !validRevision(sourceRevision) || c.Workers < 1 || c.Workers > 4 || c.K < 1 || c.Candidates < c.K || c.Candidates > 150000 {
		return fmt.Errorf("reviewed build revision, workers 1..4 and 1 <= k <= candidates <= 150000 required")
	}
	for _, limit := range []int64{limits.Runner, limits.Checker, limits.Engine} {
		if limit <= 0 || limit%int64(os.Getpagesize()) != 0 {
			return fmt.Errorf("explicit positive page-aligned memory limits required")
		}
	}
	c.Executable, err = os.Executable()
	if err != nil {
		return err
	}
	exePin, err := dataset.Hash(c.Executable)
	if err != nil {
		return err
	}
	launcherPin, err := dataset.Hash(c.Launcher)
	if err != nil {
		return err
	}
	c.ApprovalSHA256, err = dataset.Hash(approvePath)
	if err != nil {
		return err
	}
	if !validSHA(approvePin) || approvePin != c.ApprovalSHA256 {
		return fmt.Errorf("approval receipt hash mismatch")
	}
	if err := readJSON(approvePath, &c.Approval); err != nil {
		return err
	}
	host, err := os.Hostname()
	if err != nil {
		return err
	}
	want := approval{true, sourceRevision, exePin, launcherPin, c.FixturePin, host, c.Approval.Operator, c.Approval.PriceCapUSD, duration.Seconds(), limits}
	if err := checkApproval(c.Approval, want); err != nil {
		return err
	}
	manifest, err := readManifest(c.Data, c.FixturePin)
	if err != nil {
		return err
	}
	if manifest.Documents != 150000 || manifest.SourceRevision != sourceRevision || manifest.GeneratorSHA256 != exePin {
		return fmt.Errorf("measurement requires 150000 vectors from this pinned generator build")
	}
	dataAbs, err := filepath.Abs(c.Data)
	if err != nil {
		return err
	}
	outAbs, err := filepath.Abs(c.Out)
	if err != nil {
		return err
	}
	rel, err := filepath.Rel(dataAbs, outAbs)
	if err != nil {
		return err
	}
	if rel == "." || (rel != ".." && !strings.HasPrefix(rel, ".."+string(os.PathSeparator))) {
		return fmt.Errorf("output cannot be inside the immutable input fixture")
	}
	if err := os.Mkdir(c.Out, 0700); err != nil {
		return err
	}
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	ctx, deadlineCancel := context.WithDeadline(ctx, c.Deadline)
	defer deadlineCancel()
	return controlled(ctx, c, manifest)
}
func checkApproval(got, want approval) error {
	if !got.Approved || got.Operator == "" || got.PriceCapUSD < 0 || got != want {
		return fmt.Errorf("run identity, operator, price cap, duration or numeric memory approval mismatch")
	}
	return nil
}

// This small controller stays outside every target cgroup so it can observe and
// preserve terminal receipts after an engine, checker or reducer is killed.
func controlled(ctx context.Context, c runConfig, manifest fixtureManifest) (err error) {
	receipt := runReceipt{Runtime: runtime.Version(), Platform: runtime.GOOS + "/" + runtime.GOARCH, Config: c, Fixture: manifest, State: "failed", Components: map[string]json.RawMessage{}, Outputs: map[string]string{}, Accounting: "Engine/checker/reducer run serially in independent accepted limited-command scopes. Each component entry preserves external-command/v1 cgroup peak, OOM, exit, PID, RSS and cleanup. Runner means the result reducer. Small outer controller and launcher overhead are not engine memory; their enclosing cap is operator-owned."}
	defer func() {
		final := result{Status: "blocked", Branch: "execution_or_receipt_failure_return_user", SyntheticOnly: true, Targets: []targetResult{}}
		if err == nil {
			err = readJSON(filepath.Join(c.Out, "result.partial.json"), &final)
		}
		var terminalErr error
		walkErr := filepath.WalkDir(c.Out, func(path string, entry fs.DirEntry, walkErr error) error {
			if walkErr != nil {
				return walkErr
			}
			if entry.IsDir() {
				return nil
			}
			rel, e := filepath.Rel(c.Out, path)
			if e != nil {
				return e
			}
			pin, e := dataset.Hash(path)
			if e != nil {
				return e
			}
			receipt.Outputs[rel] = pin
			if entry.Name() == "receipt.json" {
				b, e := os.ReadFile(path)
				if e != nil {
					return e
				}
				receipt.Components[filepath.Dir(rel)] = b
				var terminal struct {
					State   string `json:"state"`
					Removed bool   `json:"removed"`
				}
				if e := json.Unmarshal(b, &terminal); e != nil {
					terminalErr = errors.Join(terminalErr, e)
					return nil
				}
				if terminal.State != "completed" || !terminal.Removed {
					terminalErr = errors.Join(terminalErr, fmt.Errorf("component %s did not complete with owned cleanup", rel))
				}
			}
			return nil
		})
		err = errors.Join(err, walkErr, terminalErr)
		if len(receipt.Components) != 5 {
			err = errors.Join(err, fmt.Errorf("expected five terminal component receipts, got %d", len(receipt.Components)))
		}
		if err != nil {
			final.Status = "blocked"
			final.Branch = "execution_or_receipt_failure_return_user"
		}
		resultPath := filepath.Join(c.Out, "result.json")
		err = errors.Join(err, writeJSON(resultPath, final))
		pin, hashErr := dataset.Hash(resultPath)
		err = errors.Join(err, hashErr)
		if hashErr == nil {
			receipt.Outputs["result.json"] = pin
		}
		if err != nil {
			receipt.Error = err.Error()
		} else {
			receipt.State = "completed"
		}
		err = errors.Join(err, writeJSON(filepath.Join(c.Out, "run-receipt.json"), receipt))
	}()
	request := filepath.Join(c.Out, "request.json")
	if err := writeJSON(request, c); err != nil {
		return err
	}
	policy := filepath.Join(c.Out, "policy.json")
	if err := writeJSON(policy, map[string]any{"schema": "external-limits/v1", "deadline": c.Deadline}); err != nil {
		return err
	}
	for _, mode := range []string{"f32", "int8"} {
		if err := launch(ctx, c, "engine-"+mode, c.Approval.Limits.Engine, []string{c.Executable, "__engine", request, mode}); err != nil {
			return err
		}
		if err := launch(ctx, c, "checker-"+mode, c.Approval.Limits.Checker, []string{c.Executable, "__checker", request, mode}); err != nil {
			return err
		}
	}
	return launch(ctx, c, "runner", c.Approval.Limits.Runner, []string{c.Executable, "__summarize", request})
}

func launch(ctx context.Context, c runConfig, name string, limit int64, command []string) error {
	for path, expected := range map[string]string{c.Executable: c.Approval.ExecutableSHA256, c.Launcher: c.Approval.LauncherSHA256} {
		pin, err := dataset.Hash(path)
		if err != nil {
			return err
		}
		if pin != expected {
			return fmt.Errorf("executable drift before %s", name)
		}
	}
	request := filepath.Join(c.Out, "request.json")
	requestPin, err := dataset.Hash(request)
	if err != nil {
		return err
	}
	policy := filepath.Join(c.Out, "policy.json")
	policyPin, err := dataset.Hash(policy)
	if err != nil {
		return err
	}
	// RunExternal's engine slot caps this one named component. Preserve its role.
	binding := runner.RunBinding{SourceRevision: c.Approval.SourceRevision, HarnessSHA256: c.Approval.LauncherSHA256, DataSHA256: requestPin, RecipeSHA256: policyPin, Host: c.Approval.Host, Command: command, Limits: runner.MemoryLimits{Runner: c.Approval.Limits.Runner, Checker: c.Approval.Limits.Checker, Engine: limit}}
	path := filepath.Join(c.Out, name+"-binding.json")
	if err := writeJSON(path, binding); err != nil {
		return err
	}
	pin, err := dataset.Hash(path)
	if err != nil {
		return err
	}
	cmd := exec.CommandContext(ctx, c.Launcher, "-binding", path, "-binding-pin", pin, "-policy", policy, "-data", request, "-cgroup-root", c.CgroupRoot, "-out", filepath.Join(c.Out, name+"-launch"), "--")
	cmd.Args = append(cmd.Args, command...)
	cmd.Stdout, cmd.Stderr = os.Stdout, os.Stderr
	cmd.Cancel = func() error { return cmd.Process.Signal(syscall.SIGTERM) }
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("%s: %w", name, err)
	}
	return nil
}

func summarize(c runConfig) error {
	var targets []targetResult
	for _, mode := range []string{"f32", "int8"} {
		var engine engineResult
		var checker checkResult
		if err := readJSON(filepath.Join(c.Out, mode+"-engine.json"), &engine); err != nil {
			return err
		}
		if err := readJSON(filepath.Join(c.Out, mode+"-check.json"), &checker); err != nil {
			return err
		}
		latency, err := p95(engine.Samples)
		if err != nil {
			return err
		}
		status := "miss"
		if checker.Exact && latency <= 50 {
			status = "pass"
		}
		target := mode + " exact ordered top-k at 150000x768, <=4 workers, p95<=50ms"
		if mode == "f32" {
			target = "f32 scalar single-accumulator float64 kernel; exact ordered top-k at 150000x768, <=4 workers, p95<=50ms"
			if !checker.Exact {
				status = "blocked"
			}
		}
		targets = append(targets, targetResult{Target: target, P95MS: latency, Exact: checker.Exact, Status: status})
	}
	return writeJSON(filepath.Join(c.Out, "result.partial.json"), selectResult(targets[0], targets[1]))
}
func selectResult(f32, int8 targetResult) result {
	r := result{Status: "miss", Branch: "return_miss_to_user", SyntheticOnly: true, Targets: []targetResult{f32, int8}}
	if !f32.Exact {
		r.Status = "blocked"
		r.Branch = "f32_reference_mismatch_return_user"
		return r
	}
	if f32.P95MS <= 50 {
		r.Status = "pass"
		r.Branch = "keep_exact_f32_int8_extra_evidence"
		return r
	}
	if f32.P95MS > 50 && int8.P95MS > 50 {
		r.Branch = "both_latency_miss_return_user_ann_diagnostic_eligible_not_run"
		return r
	}
	if int8.Exact && int8.P95MS <= 50 {
		r.Status = "partial"
		r.Branch = "synthetic_only_return_user_for_real_vector_int8_decision"
	}
	return r
}
