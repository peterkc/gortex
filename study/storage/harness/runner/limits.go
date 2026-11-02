package runner

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"gxstudy/adapter"
	"gxstudy/dataset"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"syscall"
)

type MemoryLimits struct {
	Runner  int64 `json:"runner"`
	Checker int64 `json:"checker"`
	Engine  int64 `json:"engine"`
}
type RunBinding struct {
	SourceRevision string       `json:"source_revision"`
	HarnessSHA256  string       `json:"harness_sha256"`
	DataSHA256     string       `json:"data_sha256"`
	RecipeSHA256   string       `json:"recipe_sha256"`
	Host           string       `json:"host"`
	Command        []string     `json:"command"`
	Limits         MemoryLimits `json:"memory_limits_bytes"`
}
type ComponentMemory struct {
	PID          int    `json:"pid"`
	LimitBytes   int64  `json:"limit_bytes"`
	PeakBytes    int64  `json:"cgroup_peak_bytes"`
	PeakRSSBytes int64  `json:"process_peak_rss_bytes"`
	RSSSource    string `json:"process_peak_rss_source"`
	OOMKills     int64  `json:"oom_kills"`
	ExitCode     int    `json:"exit_code"`
	Status       string `json:"status"`
	Error        string `json:"error,omitempty"`
	Scope        string `json:"scope"`
}
type componentReceipt struct {
	Binding       RunBinding                 `json:"binding"`
	RequestSHA256 string                     `json:"request_sha256"`
	Components    map[string]ComponentMemory `json:"components"`
	Outputs       map[string]string          `json:"output_sha256"`
	Success       bool                       `json:"success"`
	Error         string                     `json:"error,omitempty"`
}

func validDigest(value string) bool {
	decoded, err := hex.DecodeString(value)
	return err == nil && len(decoded) == sha256.Size
}
func readBinding(path, data string, args []string) (RunBinding, error) {
	var binding RunBinding
	f, err := os.Open(path)
	if err != nil {
		return binding, err
	}
	defer f.Close()
	decoder := json.NewDecoder(f)
	decoder.DisallowUnknownFields()
	if err = decoder.Decode(&binding); err != nil {
		return binding, err
	}
	var trailing any
	if err = decoder.Decode(&trailing); err != io.EOF {
		return binding, fmt.Errorf("trailing binding data")
	}
	if len(binding.SourceRevision) != 40 && len(binding.SourceRevision) != 64 {
		return binding, fmt.Errorf("missing source revision")
	}
	if _, err = hex.DecodeString(binding.SourceRevision); err != nil {
		return binding, fmt.Errorf("invalid source revision")
	}
	for _, value := range []string{binding.HarnessSHA256, binding.DataSHA256, binding.RecipeSHA256} {
		if !validDigest(value) {
			return binding, fmt.Errorf("missing or invalid source/harness/data/recipe binding")
		}
	}
	if binding.Limits.Runner <= 0 || binding.Limits.Checker <= 0 || binding.Limits.Engine <= 0 {
		return binding, fmt.Errorf("explicit positive runner, checker and engine hard limits required")
	}
	for _, limit := range []int64{binding.Limits.Runner, binding.Limits.Checker, binding.Limits.Engine} {
		if limit%int64(os.Getpagesize()) != 0 {
			return binding, fmt.Errorf("memory limits must be page aligned")
		}
	}
	host, err := os.Hostname()
	if err != nil {
		return binding, err
	}
	if binding.Host != host || !slices.Equal(binding.Command, args) {
		return binding, fmt.Errorf("host or command binding mismatch")
	}
	exe, err := os.Executable()
	if err != nil {
		return binding, err
	}
	digest, err := dataset.Hash(exe)
	if err != nil {
		return binding, err
	}
	if digest != binding.HarnessSHA256 {
		return binding, fmt.Errorf("harness executable binding mismatch")
	}
	if data != "" {
		digest, err = dataset.Hash(filepath.Join(data, "manifest.json"))
		if err != nil {
			return binding, err
		}
		if digest != binding.DataSHA256 {
			return binding, fmt.Errorf("data binding mismatch")
		}
	}
	return binding, nil
}
func scopeMemory(path string, limit int64, pid int, state *os.ProcessState, runErr error) (ComponentMemory, error) {
	r := ComponentMemory{PID: pid, LimitBytes: limit, Scope: "Linux cgroup-v2 component process tree; includes charged file cache; separate process RSS peak", Status: "failed", ExitCode: -1}
	peak, err := os.ReadFile(filepath.Join(path, "memory.peak"))
	if err != nil {
		return r, err
	}
	r.PeakBytes, err = strconv.ParseInt(strings.TrimSpace(string(peak)), 10, 64)
	if err != nil {
		return r, err
	}
	events, err := os.ReadFile(filepath.Join(path, "memory.events"))
	if err != nil {
		return r, err
	}
	for _, line := range strings.Split(string(events), "\n") {
		fields := strings.Fields(line)
		if len(fields) == 2 && fields[0] == "oom_kill" {
			r.OOMKills, err = strconv.ParseInt(fields[1], 10, 64)
			if err != nil {
				return r, err
			}
		}
	}
	if state != nil {
		r.ExitCode = state.ExitCode()
		if usage, ok := state.SysUsage().(*syscall.Rusage); ok {
			r.PeakRSSBytes = usage.Maxrss * 1024
			r.RSSSource = "wait status high-water"
		}
	}
	if runErr == nil && r.OOMKills == 0 && r.ExitCode == 0 {
		r.Status = "completed"
	}
	if runErr != nil {
		r.Error = runErr.Error()
	}
	if r.OOMKills > 0 {
		r.Error = "kernel memory limit exceeded"
	}
	return r, nil
}
func isolatedRunChild(original adapter.Backend, factory adapter.CatalogFactory, req runRequest) (err error) {
	defer func() { err = errors.Join(err, recordRunnerRSS(req)) }()
	ctx := context.Background()
	engine, err := startProcess(ctx, req.EngineScope, filepath.Join(req.Out, fmt.Sprintf("engine-%02d.log", req.Run)), false)
	if err != nil {
		return err
	}
	checker, err := startProcess(ctx, req.CheckerScope, filepath.Join(req.Out, fmt.Sprintf("checker-%02d.log", req.Run)), true)
	if err != nil {
		return errors.Join(err, engine.close())
	}
	remote, remoteFactory, err := remoteCandidate(ctx, engine)
	if err != nil {
		return errors.Join(err, engine.close(), checker.close())
	}
	req.checker = checker
	req.engine = engine
	runErr := runChild(remote, remoteFactory, req)
	engineErr := engine.close()
	checkerErr := checker.close()
	err = errors.Join(runErr, engineErr, checkerErr)
	resultPath := filepath.Join(req.Out, fmt.Sprintf("%s-%s-%02d.json", original.Name(), filepath.Base(req.Data), req.Run))
	if req.Run == 0 {
		resultPath = filepath.Join(req.Out, "run-00", "warmup-status.json")
	}
	var result map[string]any
	if readErr := dataset.ReadJSON(resultPath+".partial", &result); readErr != nil {
		return errors.Join(err, readErr)
	}
	engineMemory, memoryErr := scopeMemory(req.EngineScope, req.Binding.Limits.Engine, engine.cmd.Process.Pid, engine.cmd.ProcessState, engineErr)
	err = errors.Join(err, memoryErr)
	checkerMemory, memoryErr := scopeMemory(req.CheckerScope, req.Binding.Limits.Checker, checker.cmd.Process.Pid, checker.cmd.ProcessState, checkerErr)
	err = errors.Join(err, memoryErr)
	err = errors.Join(err, dataset.WriteJSON(filepath.Join(req.Out, fmt.Sprintf("engine-%02d.memory.json", req.Run)), engineMemory), dataset.WriteJSON(filepath.Join(req.Out, fmt.Sprintf("checker-%02d.memory.json", req.Run)), checkerMemory))
	result["engine_memory"] = engineMemory
	result["checker_memory"] = checkerMemory
	result["runner_pid"] = result["pid"]
	result["pid"] = engineMemory.PID
	result["runner_peak_rss_bytes"] = result["peak_rss_bytes"]
	result["peak_rss_bytes"] = engineMemory.PeakRSSBytes
	result["peak_rss_scope"] = "engine process only; runner and checker excluded; cgroup tree accounting is separate"
	if err != nil || engineMemory.Status != "completed" || checkerMemory.Status != "completed" {
		result["success"] = false
		if err == nil {
			err = fmt.Errorf("component memory failure")
		}
		result["component_error"] = err.Error()
	}
	return errors.Join(err, dataset.WriteJSON(resultPath, result))
}

func runLimitedCommand(ctx context.Context, exe, mode, requestPath, scope, logPath string) (*exec.Cmd, error) {
	cmd := exec.CommandContext(ctx, exe, mode, requestPath)
	log, err := os.OpenFile(logPath, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		return nil, err
	}
	cmd.Stdout = log
	cmd.Stderr = log
	release, err := limitCommand(cmd, scope)
	if err != nil {
		log.Close()
		return nil, err
	}
	defer release()
	err = cmd.Run()
	return cmd, errors.Join(err, log.Close())
}

// The runner reaps engine/checker children, so its wait status is not a self-only
// RSS source. Write RUSAGE_SELF from inside the runner after its owned joins.
func recordRunnerRSS(req runRequest) error {
	var usage syscall.Rusage
	if err := syscall.Getrusage(syscall.RUSAGE_SELF, &usage); err != nil {
		return err
	}
	peak := usage.Maxrss
	if runtime.GOOS == "linux" {
		peak *= 1024
	}
	return dataset.WriteJSON(filepath.Join(req.Out, fmt.Sprintf("runner-%02d.memory.json", req.Run)), ComponentMemory{PID: os.Getpid(), PeakRSSBytes: peak, RSSSource: "RUSAGE_SELF"})
}
func runnerMemory(req runRequest, cmd *exec.Cmd, runErr error) (ComponentMemory, error) {
	pid := 0
	var state *os.ProcessState
	if cmd != nil {
		state = cmd.ProcessState
		if cmd.Process != nil {
			pid = cmd.Process.Pid
		}
	}
	memory, err := scopeMemory(req.RunnerScope, req.Binding.Limits.Runner, pid, state, runErr)
	memory.PeakRSSBytes = 0
	memory.RSSSource = "self RSS unavailable"
	var own ComponentMemory
	readErr := dataset.ReadJSON(filepath.Join(req.Out, fmt.Sprintf("runner-%02d.memory.json", req.Run)), &own)
	if readErr == nil && (own.PID != pid || own.PeakRSSBytes <= 0 || own.RSSSource != "RUSAGE_SELF") {
		readErr = fmt.Errorf("invalid runner self-RSS record")
	}
	if readErr != nil {
		memory.Status = "failed"
		memory.Error = errors.Join(runErr, readErr).Error()
		return memory, errors.Join(err, readErr)
	}
	memory.PeakRSSBytes = own.PeakRSSBytes
	memory.RSSSource = own.RSSSource
	return memory, err
}
