package runner

import (
	"context"
	"crypto/sha256"
	"errors"
	"flag"
	"fmt"
	"gxstudy/adapter"
	"gxstudy/dataset"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"syscall"
)

func controlledRun(ctx context.Context, exe, mode, root string, req runRequest) (err error) {
	receipt := componentReceipt{Binding: req.Binding, Components: map[string]ComponentMemory{}, Outputs: map[string]string{}}
	var scopes []*memoryScope
	receiptPath := filepath.Join(req.Out, fmt.Sprintf("components-%02d.json", req.Run))
	defer func() {
		for i := len(scopes) - 1; i >= 0; i-- {
			err = errors.Join(err, scopes[i].close())
		}
		receipt.Success = err == nil
		if err != nil {
			receipt.Error = err.Error()
		}
		// Bind this attempt only. Do not accumulate prior attempts' filenames or
		// sample records in the unmeasured, small controller process.
		names := []string{fmt.Sprintf("request-%02d.json", req.Run), fmt.Sprintf("runner-%02d.log", req.Run), fmt.Sprintf("runner-%02d.memory.json", req.Run)}
		for _, component := range []string{"engine", "checker"} {
			base := fmt.Sprintf("%s-%02d", component, req.Run)
			names = append(names, base+".log", base+".log.pid", base+".memory.json")
		}
		if req.Adapter != "" {
			result := fmt.Sprintf("%s-%s-%02d.json", req.Adapter, filepath.Base(req.Data), req.Run)
			if req.Run == 0 {
				result = filepath.Join("run-00", "warmup-status.json")
			}
			names = append(names, result, result+".partial")
		}
		for _, name := range names {
			path := filepath.Join(req.Out, name)
			hash, hashErr := dataset.Hash(path)
			if os.IsNotExist(hashErr) {
				continue
			}
			if hashErr != nil {
				err = errors.Join(err, hashErr)
				continue
			}
			receipt.Outputs[name] = hash
		}
		if req.CatalogResult != "" {
			hash, hashErr := dataset.Hash(req.CatalogResult)
			err = errors.Join(err, hashErr)
			if hashErr == nil {
				receipt.Outputs["catalog-result.json"] = hash
			}
		}
		receipt.Success = err == nil
		if err != nil {
			receipt.Error = err.Error()
		}
		err = errors.Join(err, dataset.WriteJSON(receiptPath, receipt))
	}()
	for _, component := range []struct {
		name  string
		limit int64
	}{{"runner", req.Binding.Limits.Runner}, {"checker", req.Binding.Limits.Checker}, {"engine", req.Binding.Limits.Engine}} {
		scope, scopeErr := newMemoryScope(root, component.name, component.limit)
		if scope != nil {
			scopes = append(scopes, scope)
		}
		if scopeErr != nil {
			return scopeErr
		}
		switch component.name {
		case "runner":
			req.RunnerScope = scope.path
		case "checker":
			req.CheckerScope = scope.path
		case "engine":
			req.EngineScope = scope.path
		}
	}
	requestPath := filepath.Join(req.Out, fmt.Sprintf("request-%02d.json", req.Run))
	if err = dataset.WriteJSON(requestPath, req); err != nil {
		return err
	}
	receipt.RequestSHA256, err = dataset.Hash(requestPath)
	if err != nil {
		return err
	}
	cmd, runErr := runLimitedCommand(ctx, exe, mode, requestPath, req.RunnerScope, filepath.Join(req.Out, fmt.Sprintf("runner-%02d.log", req.Run)))
	err = runErr
	if runErr != nil {
		for _, scope := range scopes {
			err = errors.Join(err, scope.stop())
		}
	}
	memory, memoryErr := runnerMemory(req, cmd, runErr)
	receipt.Components["runner"] = memory
	err = errors.Join(err, memoryErr)
	for _, component := range []struct {
		name, path string
		limit      int64
	}{{"engine", req.EngineScope, req.Binding.Limits.Engine}, {"checker", req.CheckerScope, req.Binding.Limits.Checker}} {
		pidBytes, pidErr := os.ReadFile(filepath.Join(req.Out, fmt.Sprintf("%s-%02d.log.pid", component.name, req.Run)))
		pid := 0
		if pidErr == nil {
			pid, pidErr = strconv.Atoi(string(pidBytes))
		}
		memory, memoryErr := scopeMemory(component.path, component.limit, pid, nil, runErr)
		// Successful subprocess joins were recorded by the runner; without that
		// evidence a dead runner cannot certify another component as completed.
		var terminal ComponentMemory
		terminalErr := dataset.ReadJSON(filepath.Join(req.Out, fmt.Sprintf("%s-%02d.memory.json", component.name, req.Run)), &terminal)
		if terminalErr == nil {
			memory = terminal
		} else {
			err = errors.Join(err, terminalErr, pidErr)
		}
		receipt.Components[component.name] = memory
		err = errors.Join(err, memoryErr)
		if memory.Status != "completed" {
			err = errors.Join(err, fmt.Errorf("%s did not complete", component.name))
		}
	}
	if req.Adapter != "" {
		resultPath := filepath.Join(req.Out, fmt.Sprintf("%s-%s-%02d.json", req.Adapter, filepath.Base(req.Data), req.Run))
		if req.Run == 0 {
			resultPath = filepath.Join(req.Out, "run-00", "warmup-status.json")
		}
		var result map[string]any
		readErr := dataset.ReadJSON(resultPath, &result)
		err = errors.Join(err, readErr)
		if readErr == nil {
			result["runner_memory"] = receipt.Components["runner"]
			result["runner_peak_rss_bytes"] = receipt.Components["runner"].PeakRSSBytes
			if err != nil {
				result["success"] = false
				result["component_error"] = err.Error()
			}
			err = errors.Join(err, dataset.WriteJSON(resultPath, result))
		}
	}
	return err
}

// Catalog measurements use the same enforced component boundary. Control
// receipts are separate from the catalog's fresh benchmark directory.
func RunLimitedCatalog(b adapter.Backend, factory adapter.CatalogFactory, args []string) error {
	fs := flag.NewFlagSet("catalog-bench-limits", flag.ContinueOnError)
	bindingPath := fs.String("binding", "", "required hash-bound command and memory limits")
	root := fs.String("cgroup-root", "", "required delegated cgroup-v2 root")
	// Limit flags must precede --; the remaining flags belong to the existing CLI.
	if err := fs.Parse(args); err != nil {
		return err
	}
	remaining := fs.Args()
	if *bindingPath == "" || *root == "" || len(remaining) == 0 {
		return fmt.Errorf("catalog-bench requires -binding FILE -cgroup-root DIR -- <catalog flags>")
	}
	out := ""
	for i, arg := range remaining {
		if arg == "-out" && i+1 < len(remaining) {
			out = remaining[i+1]
		}
	}
	if out == "" {
		return fmt.Errorf("catalog -out required as a separate argument")
	}
	absolute, err := filepath.Abs(out)
	if err != nil {
		return err
	}
	binding, err := readBinding(*bindingPath, "", os.Args[1:])
	if err != nil {
		return err
	}
	inputDigest, err := catalogInputDigest(remaining)
	if err != nil {
		return err
	}
	if binding.DataSHA256 != inputDigest {
		return fmt.Errorf("catalog fixture-spec binding mismatch")
	}
	control := absolute + ".control"
	if err = os.Mkdir(control, 0755); err != nil {
		return err
	}
	exe, err := os.Executable()
	if err != nil {
		return err
	}
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	return controlledRun(ctx, exe, "__catalog_bench_run", *root, runRequest{Out: control, Run: 1, Binding: binding, CatalogArgs: remaining, CatalogResult: filepath.Join(absolute, "result.json")})
}
func catalogRunChild(b adapter.Backend, factory adapter.CatalogFactory, path string) (err error) {
	var req runRequest
	if err = dataset.ReadJSON(path, &req); err != nil {
		return err
	}
	defer func() { err = errors.Join(err, recordRunnerRSS(req)) }()
	engine, err := startProcess(context.Background(), req.EngineScope, filepath.Join(req.Out, "engine-01.log"), false)
	if err != nil {
		return err
	}
	checker, err := startProcess(context.Background(), req.CheckerScope, filepath.Join(req.Out, "checker-01.log"), true)
	if err != nil {
		return errors.Join(err, engine.close())
	}
	remote, remoteFactory, err := remoteCandidate(context.Background(), engine)
	if err == nil {
		err = RunCatalogBenchmark(remote, remoteFactory, req.CatalogArgs)
	}
	engineErr := engine.close()
	checkerErr := checker.close()
	err = errors.Join(err, engineErr, checkerErr)
	for _, c := range []struct {
		name   string
		client *processClient
		limit  int64
		scope  string
		err    error
	}{{"engine", engine, req.Binding.Limits.Engine, req.EngineScope, engineErr}, {"checker", checker, req.Binding.Limits.Checker, req.CheckerScope, checkerErr}} {
		memory, memoryErr := scopeMemory(c.scope, c.limit, c.client.cmd.Process.Pid, c.client.cmd.ProcessState, c.err)
		err = errors.Join(err, memoryErr)
		err = errors.Join(err, dataset.WriteJSON(filepath.Join(req.Out, c.name+"-01.memory.json"), memory))
	}
	var result map[string]any
	if readErr := dataset.ReadJSON(req.CatalogResult+".partial", &result); readErr != nil {
		return errors.Join(err, readErr)
	}
	result["success"] = err == nil
	if err != nil {
		result["component_error"] = err.Error()
	}
	return errors.Join(err, dataset.WriteJSON(req.CatalogResult, result))
}

func catalogInputDigest(args []string) (string, error) {
	fs := flag.NewFlagSet("catalog-fixture-binding", flag.ContinueOnError)
	fs.String("candidate", "", "candidate")
	fs.String("out", "", "out")
	fs.Int("repeats", 30, "repeats")
	entries := fs.Int("entries", 1, "entries")
	seed := fs.Int64("seed", 20261025, "seed")
	if err := fs.Parse(args); err != nil {
		return "", err
	}
	return fmt.Sprintf("%x", sha256.Sum256([]byte(fmt.Sprintf("gxstudy-catalog-fixture/v1\nentries=%d\nseed=%d\n", *entries, *seed)))), nil
}
