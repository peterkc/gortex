package runner

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"gxstudy/dataset"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"
)

type externalPolicy struct {
	Schema   string    `json:"schema"`
	Deadline time.Time `json:"deadline"`
}
type externalReceipt struct {
	Schema        string          `json:"schema"`
	Binding       RunBinding      `json:"binding"`
	BindingSHA256 string          `json:"binding_sha256"`
	Deadline      time.Time       `json:"deadline"`
	Memory        ComponentMemory `json:"memory"`
	State         string          `json:"state"`
	Cgroup        string          `json:"cgroup"`
	Removed       bool            `json:"removed"`
	LogSHA256     string          `json:"log_sha256,omitempty"`
	Error         string          `json:"error,omitempty"`
}

// RunExternal is the thin CLI for bridge, inference and evaluator commands.
// It reuses the harness RunBinding: engine is the external process-tree limit.
// Runner/checker are not launched or measured here. The small cleanup owner must
// stay outside the target scope; its own enclosing limit belongs to the operator.
func RunExternal(args []string) error {
	fs := flag.NewFlagSet("limited-command", flag.ContinueOnError)
	bindingPath := fs.String("binding", "", "RunBinding JSON")
	bindingPin := fs.String("binding-pin", "", "expected binding SHA-256")
	policyPath := fs.String("policy", "", "hash-bound external-limits/v1 deadline")
	dataPath := fs.String("data", "", "hash-bound frozen data receipt")
	root := fs.String("cgroup-root", "", "delegated cgroup-v2 root")
	out := fs.String("out", "", "fresh output directory")
	if err := fs.Parse(args); err != nil {
		return err
	}
	command := fs.Args()
	if *bindingPath == "" || *policyPath == "" || *dataPath == "" || *out == "" || *root == "" || len(command) == 0 || !filepath.IsAbs(command[0]) {
		return fmt.Errorf("binding, binding-pin, policy, data, cgroup-root, out and absolute command required")
	}
	pin, err := dataset.Hash(*bindingPath)
	if err != nil {
		return err
	}
	if !validDigest(*bindingPin) || pin != *bindingPin {
		return fmt.Errorf("external binding hash mismatch")
	}
	binding, err := readBinding(*bindingPath, "", command)
	if err != nil {
		return err
	}
	for path, expected := range map[string]string{*policyPath: binding.RecipeSHA256, *dataPath: binding.DataSHA256} {
		actual, err := dataset.Hash(path)
		if err != nil {
			return err
		}
		if actual != expected {
			return fmt.Errorf("external input hash mismatch: %s", path)
		}
	}
	f, err := os.Open(*policyPath)
	if err != nil {
		return err
	}
	decoder := json.NewDecoder(io.LimitReader(f, 4096))
	decoder.DisallowUnknownFields()
	var policy externalPolicy
	decodeErr := decoder.Decode(&policy)
	var extra any
	trailingErr := decoder.Decode(&extra)
	closeErr := f.Close()
	if err := errors.Join(decodeErr, closeErr); err != nil {
		return err
	}
	if trailingErr != io.EOF || policy.Schema != "external-limits/v1" || !time.Now().Before(policy.Deadline) {
		return fmt.Errorf("external policy requires a future absolute deadline")
	}
	if err := os.Mkdir(*out, 0700); err != nil {
		return err
	}
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	ctx, deadlineCancel := context.WithDeadline(ctx, policy.Deadline)
	defer deadlineCancel()
	return runExternal(ctx, *root, *out, binding, pin, policy.Deadline)
}

func runExternal(ctx context.Context, root, out string, binding RunBinding, pin string, deadline time.Time) (err error) {
	receipt := externalReceipt{Schema: "external-command/v1", Binding: binding, BindingSHA256: pin, Deadline: deadline, State: "refused"}
	var scope *memoryScope
	defer func() {
		if scope != nil {
			cleanupErr := scope.close()
			receipt.Removed = cleanupErr == nil
			err = errors.Join(err, cleanupErr)
		}
		logPin, hashErr := dataset.Hash(filepath.Join(out, "command.log"))
		if hashErr == nil {
			receipt.LogSHA256 = logPin
		} else if !os.IsNotExist(hashErr) {
			err = errors.Join(err, hashErr)
		}
		if err != nil {
			receipt.Error = err.Error()
			if receipt.State == "completed" {
				receipt.State = "failed"
			}
		}
		err = errors.Join(err, dataset.WriteJSON(filepath.Join(out, "receipt.json"), receipt))
	}()
	scope, err = newMemoryScope(root, "external", binding.Limits.Engine)
	if scope != nil {
		receipt.Cgroup = scope.path
	}
	if err != nil {
		return err
	}
	if ctx.Err() != nil {
		return ctx.Err()
	}
	log, err := os.OpenFile(filepath.Join(out, "command.log"), os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, log.Close()) }()
	cmd := exec.CommandContext(ctx, binding.Command[0], binding.Command[1:]...)
	cmd.Stdout, cmd.Stderr = log, log
	release, err := limitCommand(cmd, scope.path)
	if err != nil {
		return err
	}
	defer release()
	// Cancellation kills the entire owned cgroup, not just the leader.
	cmd.Cancel = scope.stop
	runErr := cmd.Run()
	stopErr := scope.stop() // Also removes descendants after a normal leader exit.
	pid := 0
	if cmd.Process != nil {
		pid = cmd.Process.Pid
	}
	receipt.Memory, err = scopeMemory(scope.path, binding.Limits.Engine, pid, cmd.ProcessState, runErr)
	err = errors.Join(err, runErr, stopErr)
	switch {
	case receipt.Memory.OOMKills > 0:
		receipt.State = "oom"
		err = errors.Join(err, fmt.Errorf("external process tree exceeded memory.max"))
	case errors.Is(ctx.Err(), context.DeadlineExceeded):
		receipt.State = "deadline"
		err = errors.Join(err, ctx.Err())
	case ctx.Err() != nil:
		receipt.State = "cancelled"
		err = errors.Join(err, ctx.Err())
	case err == nil && receipt.Memory.Status == "completed":
		receipt.State = "completed"
	default:
		receipt.State = "failed"
		if err == nil {
			err = fmt.Errorf("external command did not complete")
		}
	}
	return err
}
