package main

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"gxstudy/adapter"
	"gxstudy/dataset"
	"gxstudy/oracle"
	"gxstudy/runner"
	"gxstudy/workload"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	cbase "study/checks/base"
	"syscall"
	"time"
)

const linkCap int64 = 1610612736

var errEdgeMismatch = errors.New("full edge multiset differs")

type parityResult struct {
	Status string
	Error  string
}

// Every resource and workload choice is explicit in the hash-bound operator file.
type config struct {
	BaseLayout     string              `json:"base_layout"`
	Schema         string              `json:"schema"`
	Data           string              `json:"data"`
	Out            string              `json:"out"`
	Launcher       string              `json:"launcher"`
	CgroupRoot     string              `json:"cgroup_root"`
	SourceRevision string              `json:"source_revision"`
	Approval       string              `json:"approval"`
	ApprovalSHA256 string              `json:"approval_sha256"`
	DeltaRepo      string              `json:"delta_repo"`
	Sizes          []int               `json:"delta_sizes"`
	Repeats        int                 `json:"query_repeats"`
	WallSeconds    int                 `json:"wall_seconds"`
	Limits         runner.MemoryLimits `json:"memory_limits_bytes"`
	LinkLimit      int64               `json:"link_memory_limit_bytes"`
}
type frozen struct {
	Counts     dataset.Counts
	Repos      []string
	QueryCount int
	Protected  map[string]string
}
type externalResult struct {
	State   string                 `json:"state"`
	Removed bool                   `json:"removed"`
	Memory  runner.ComponentMemory `json:"memory"`
	Error   string                 `json:"error"`
}
type targetResult struct {
	Target   string `json:"target"`
	Status   string `json:"status"`
	Branch   string `json:"branch"`
	Measured any    `json:"measured"`
	Reason   string `json:"reason,omitempty"`
}
type runSummary struct {
	Schema   string            `json:"schema"`
	Targets  []targetResult    `json:"targets"`
	Receipts map[string]string `json:"receipts_sha256"`
	Outputs  map[string]string `json:"outputs_sha256"`
	Complete bool              `json:"complete"`
	Error    string            `json:"error,omitempty"`
}

func digest(s string) bool { b, e := hex.DecodeString(s); return e == nil && len(b) == 32 }
func readConfig(path, pin string) (c config, err error) {
	if path == "" || !digest(pin) {
		return c, fmt.Errorf("explicit config and SHA256 required")
	}
	actual, err := dataset.Hash(path)
	if err != nil {
		return c, err
	}
	if actual != pin {
		return c, fmt.Errorf("config hash mismatch")
	}
	f, err := os.Open(path)
	if err != nil {
		return c, err
	}
	defer func() { err = errors.Join(err, f.Close()) }()
	dec := json.NewDecoder(f)
	dec.DisallowUnknownFields()
	if err = dec.Decode(&c); err != nil {
		return c, err
	}
	var extra any
	if dec.Decode(&extra) != io.EOF {
		return c, fmt.Errorf("trailing config data")
	}
	if c.BaseLayout != "c-parquet" {
		return c, fmt.Errorf("measurement requires the C Parquet base; JSON is tiny-test-only")
	}
	if c.Schema != "linkdelta-measure/v1" || c.Repeats < 1 || c.WallSeconds < 1 || c.DeltaRepo == "" || len(c.Sizes) == 0 {
		return c, fmt.Errorf("explicit schema, repo, sizes, repeats and wall bound required")
	}
	if len(c.SourceRevision) != 40 && len(c.SourceRevision) != 64 {
		return c, fmt.Errorf("full source revision required")
	}
	if _, e := hex.DecodeString(c.SourceRevision); e != nil {
		return c, e
	}
	if c.LinkLimit != linkCap {
		return c, fmt.Errorf("Check 3 requires explicit link cap 1610612736 bytes")
	}
	for _, limit := range []int64{c.Limits.Runner, c.Limits.Checker, c.Limits.Engine, c.LinkLimit} {
		if limit <= 0 || limit%4096 != 0 {
			return c, fmt.Errorf("positive page-aligned numeric limits required")
		}
	}
	previous := 0
	for _, n := range c.Sizes {
		if n <= previous {
			return c, fmt.Errorf("delta sizes must increase")
		}
		previous = n
	}
	for _, p := range []string{c.Data, c.Out, c.Launcher, c.CgroupRoot, c.Approval} {
		if !filepath.IsAbs(p) || filepath.Clean(p) != p {
			return c, fmt.Errorf("absolute canonical paths required")
		}
	}
	if !digest(c.ApprovalSHA256) {
		return c, fmt.Errorf("approval hash required")
	}
	actual, err = dataset.Hash(c.Approval)
	if err != nil {
		return c, err
	}
	if actual != c.ApprovalSHA256 {
		return c, fmt.Errorf("approval hash mismatch")
	}
	rel, err := filepath.Rel(c.Data, c.Out)
	if err != nil {
		return c, err
	}
	if rel == "." || filepath.IsLocal(rel) {
		return c, fmt.Errorf("output must be outside dataset")
	}
	return c, nil
}
func loadTier(dir string) ([]dataset.Dataset, dataset.Manifest, error) {
	var m dataset.Manifest
	manifest, err := dataset.SafeFile(dir, "manifest.json")
	if err != nil {
		return nil, m, err
	}
	if err = dataset.ReadJSON(manifest, &m); err != nil {
		return nil, m, err
	}
	if m.Version != dataset.GeneratorVersion {
		return nil, m, fmt.Errorf("wrong dataset version")
	}
	if m.Repo != "" {
		d, e := dataset.OpenSource(dir)
		return []dataset.Dataset{d}, m, e
	}
	if len(m.Repos) == 0 {
		return nil, m, fmt.Errorf("empty tier")
	}
	for name, pin := range m.SHA256 {
		p, e := dataset.SafeFile(dir, name)
		if e != nil {
			return nil, m, e
		}
		got, e := dataset.Hash(p)
		if e != nil {
			return nil, m, e
		}
		if got != pin {
			return nil, m, fmt.Errorf("tier hash mismatch: %s", name)
		}
	}
	var ds []dataset.Dataset
	seen := map[string]bool{}
	var counts dataset.Counts
	for _, repo := range m.Repos {
		if !filepath.IsLocal(repo) || filepath.Clean(repo) != repo || repo == "." || seen[repo] {
			return nil, m, fmt.Errorf("invalid/duplicate repository path")
		}
		seen[repo] = true
		for _, file := range append(slices.Clone(dataset.RecordFiles), "manifest.json") {
			if !digest(m.SHA256[filepath.Join(repo, file)]) {
				return nil, m, fmt.Errorf("incomplete tier checksum map")
			}
		}
		d, e := dataset.OpenSource(filepath.Join(dir, repo))
		if e != nil {
			return nil, m, e
		}
		if d.Repo != repo {
			return nil, m, fmt.Errorf("repository identity mismatch")
		}
		ds = append(ds, d)
		counts = dataset.Sum(counts, d.Meta.Counts)
	}
	if counts != m.Counts {
		return nil, m, fmt.Errorf("tier counts mismatch")
	}
	return ds, m, dataset.ValidateTierIDs(ds)
}
func querySet(c config, ds []dataset.Dataset) ([]workload.Query, error) {
	var qs []workload.Query
	p := filepath.Join(c.Data, "queries.json")
	if err := dataset.ReadJSON(p, &qs); err != nil {
		return nil, err
	}
	if len(qs) == 0 {
		return nil, fmt.Errorf("empty query workload")
	}
	for i := range qs {
		qs[i].ExactRank = true
	}
	// Add explicit changed/removed/file probes without discarding any tier query.
	var d dataset.Dataset
	for _, v := range ds {
		if v.Repo == c.DeltaRepo {
			d = v
		}
	}
	if d.Repo == "" {
		return nil, fmt.Errorf("delta repository absent")
	}
	change, err := runner.MakeChange(d)
	if err != nil {
		return nil, err
	}
	for _, n := range change.Replacement.Nodes {
		for _, op := range []string{"id", "name", "in", "out"} {
			key := n.ID
			if op == "name" {
				key = n.Name
			}
			qs = append(qs, workload.Query{ID: fmt.Sprintf("delta-%d", len(qs)), Op: op, Repo: d.Repo, Key: key, Kind: "any", K: -1, ExactRank: true})
		}
	}
	qs = append(qs, workload.Query{ID: "delta-file", Op: "file", Repo: d.Repo, Key: change.Path})
	for _, op := range []string{"symbols", "content"} {
		qs = append(qs, workload.Query{ID: "delta-marker-" + op, Op: op, Repo: d.Repo, Key: "deltaprobe", K: max(d.Meta.Counts.Symbols, d.Meta.Counts.Content) + len(change.Replacement.Content) + len(change.Replacement.Symbols) + 1, ExactRank: true})
	}
	return qs, nil
}
func treePins(dir string) (map[string]string, error) {
	pins := map[string]string{}
	err := filepath.WalkDir(dir, func(p string, de os.DirEntry, e error) error {
		if e != nil {
			return e
		}
		if de.IsDir() {
			return nil
		}
		if !de.Type().IsRegular() {
			return fmt.Errorf("nonregular protected input")
		}
		pin, e := dataset.Hash(p)
		if e != nil {
			return e
		}
		rel, e := filepath.Rel(dir, p)
		if e != nil {
			return e
		}
		pins[rel] = pin
		return nil
	})
	return pins, err
}
func measure(args []string) (err error) {
	fs := flag.NewFlagSet("measure", flag.ContinueOnError)
	cfgPath := fs.String("config", "", "operator config")
	pin := fs.String("config-pin", "", "SHA256")
	if err = fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 0 {
		return fmt.Errorf("unexpected arguments")
	}
	c, err := readConfig(*cfgPath, *pin)
	if err != nil {
		return err
	}
	// Bootstrap reads only metadata, never corpus rows. The capped control worker
	// validates and prepares the full dataset before engine work starts.
	if err = checkPath(c.Data); err != nil {
		return err
	}
	if err = checkPath(c.Launcher); err != nil {
		return err
	}
	if err = checkPath(filepath.Dir(c.Out)); err != nil {
		return err
	}
	if err = os.Mkdir(c.Out, 0700); err != nil {
		return err
	}
	exe, err := os.Executable()
	if err != nil {
		return err
	}
	exe, err = filepath.EvalSymlinks(exe)
	if err != nil {
		return err
	}
	cfgCopy := filepath.Join(c.Out, "config.json")
	raw, err := os.ReadFile(*cfgPath)
	if err != nil {
		return err
	}
	if err = os.WriteFile(cfgCopy, raw, 0600); err != nil {
		return err
	}
	deadline := time.Now().Add(time.Duration(c.WallSeconds) * time.Second)
	if err = dataset.WriteJSON(filepath.Join(c.Out, "policy.json"), struct {
		Schema   string    `json:"schema"`
		Deadline time.Time `json:"deadline"`
	}{"external-limits/v1", deadline}); err != nil {
		return err
	}
	for _, name := range []string{"home", "config", "cache", "data", "tmp", "state", "runtime"} {
		if err = os.Mkdir(filepath.Join(c.Out, name), 0700); err != nil {
			return err
		}
	}
	os.Setenv("HOME", filepath.Join(c.Out, "home"))
	os.Setenv("TMPDIR", filepath.Join(c.Out, "tmp"))
	os.Setenv("XDG_CONFIG_HOME", filepath.Join(c.Out, "config"))
	os.Setenv("XDG_CACHE_HOME", filepath.Join(c.Out, "cache"))
	os.Setenv("XDG_DATA_HOME", filepath.Join(c.Out, "data"))
	os.Setenv("XDG_STATE_HOME", filepath.Join(c.Out, "state"))
	os.Setenv("XDG_RUNTIME_DIR", filepath.Join(c.Out, "runtime"))
	os.Setenv("GOMAXPROCS", "4")
	meta := map[string]string{"config_sha256": *pin, "source_revision": c.SourceRevision, "approval_sha256": c.ApprovalSHA256}
	for name, p := range map[string]string{"binary_sha256": exe, "launcher_sha256": c.Launcher, "manifest_sha256": filepath.Join(c.Data, "manifest.json")} {
		h, e := dataset.Hash(p)
		if e != nil {
			return e
		}
		meta[name] = h
	}
	frozenPath := filepath.Join(c.Out, "inputs.json")
	if err = dataset.WriteJSON(frozenPath, meta); err != nil {
		return err
	}
	command := []string{exe, "worker", "-config", cfgCopy, "-config-pin", *pin, "-task", "control"}
	receipt, runErr := launch(c, "controller", c.Limits.Runner, command, frozenPath)
	resultPin, resultErr := dataset.Hash(filepath.Join(c.Out, "result.json"))
	final := struct {
		Schema         string
		SourceRevision string
		Inputs         map[string]string
		Controller     externalResult
		ResultSHA256   string
		Complete       bool
		Error          string
	}{Schema: "linkdelta-terminal/v1", SourceRevision: c.SourceRevision, Inputs: meta, Controller: receipt, ResultSHA256: resultPin, Complete: runErr == nil && resultErr == nil}
	if runErr != nil || resultErr != nil {
		final.Error = errors.Join(runErr, resultErr).Error()
	}
	writeErr := dataset.WriteJSON(filepath.Join(c.Out, "run-receipt.json"), final)
	return errors.Join(runErr, resultErr, writeErr)
}
func launch(c config, name string, limit int64, command []string, dataPath string) (receipt externalResult, err error) {
	host, err := os.Hostname()
	if err != nil {
		return receipt, err
	}
	h, err := dataset.Hash(c.Launcher)
	if err != nil {
		return receipt, err
	}
	dataHash, err := dataset.Hash(dataPath)
	if err != nil {
		return receipt, err
	}
	policy := filepath.Join(c.Out, "policy.json")
	recipe, err := dataset.Hash(policy)
	if err != nil {
		return receipt, err
	}
	limits := c.Limits
	limits.Engine = limit
	binding := runner.RunBinding{SourceRevision: c.SourceRevision, HarnessSHA256: h, DataSHA256: dataHash, RecipeSHA256: recipe, Host: host, Command: command, Limits: limits}
	bindingPath := filepath.Join(c.Out, name+"-binding.json")
	if err = dataset.WriteJSON(bindingPath, binding); err != nil {
		return receipt, err
	}
	pin, err := dataset.Hash(bindingPath)
	if err != nil {
		return receipt, err
	}
	out := filepath.Join(c.Out, name+"-scope")
	args := []string{"-binding", bindingPath, "-binding-pin", pin, "-policy", policy, "-data", dataPath, "-cgroup-root", c.CgroupRoot, "-out", out, "--"}
	args = append(args, command...)
	cmd := exec.Command(c.Launcher, args...)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	runErr := cmd.Run()
	readErr := dataset.ReadJSON(filepath.Join(out, "receipt.json"), &receipt)
	if readErr != nil {
		return receipt, errors.Join(runErr, readErr)
	}
	if receipt.Memory.LimitBytes != limit || receipt.Memory.PID <= 0 || receipt.Memory.PeakBytes <= 0 || receipt.Memory.Status != "completed" {
		return receipt, errors.Join(runErr, fmt.Errorf("%s has incomplete component accounting", name))
	}
	if receipt.State != "completed" || !receipt.Removed {
		return receipt, errors.Join(runErr, fmt.Errorf("%s failed: %s %s", name, receipt.State, receipt.Error))
	}
	return receipt, runErr
}
func work(ctx context.Context, c config, task string) error {
	switch task {
	case "control":
		return control(c)
	case "prepare":
		return prepare(ctx, c)
	case "link-global", "link-package":
		_, err := resolveFacts(ctx, filepath.Join(c.Out, "facts"), filepath.Join(c.Out, task), strings.TrimPrefix(task, "link-"))
		return err
	case "check-link-global", "check-link-package":
		e := compareEdgeFiles(filepath.Join(c.Out, "facts", "expected.jsonl"), filepath.Join(c.Out, strings.TrimPrefix(task, "check-"), "edges.jsonl"), filepath.Join(c.Out, task))
		if e != nil && !errors.Is(e, errEdgeMismatch) {
			return e
		}
		result := parityResult{Status: "pass"}
		if e != nil {
			result.Status = "miss"
			result.Error = e.Error()
		}
		return dataset.WriteJSON(filepath.Join(c.Out, task+".json"), result)
	case "delta-arrow", "delta-parquet":
		return measureDeltas(ctx, c, strings.TrimPrefix(task, "delta-"))
	case "verify-arrow", "verify-parquet":
		return verifyDeltas(ctx, c, strings.TrimPrefix(task, "verify-"))
	default:
		return fmt.Errorf("unknown worker task")
	}
}
func control(c config) (err error) {
	summary := runSummary{Schema: "linkdelta-run/v1", Receipts: map[string]string{}, Outputs: map[string]string{}}
	defer func() {
		for _, target := range []string{"check3-link-peak-and-parity", "check4-arrow", "check4-parquet", "check4-delta-selection"} {
			found := false
			for _, result := range summary.Targets {
				if result.Target == target {
					found = true
				}
			}
			if !found {
				summary.Targets = append(summary.Targets, targetResult{Target: target, Status: "blocked", Branch: "not-completed", Reason: "Inspect the preserved component receipts and error; no target pass is inferred."})
			}
		}
		summary.Complete = err == nil
		if err != nil {
			summary.Error = err.Error()
		}
		for _, name := range []string{"prepare", "link-global", "check-link-global", "link-package", "check-link-package", "delta-arrow", "delta-parquet", "verify-arrow", "verify-parquet"} {
			p := filepath.Join(c.Out, name+"-scope", "receipt.json")
			if pin, e := dataset.Hash(p); e == nil {
				summary.Receipts[name] = pin
			}
		}
		// Retain every produced output and hash it; failed scope files remain too.
		e := filepath.WalkDir(c.Out, func(p string, de os.DirEntry, e error) error {
			if e != nil {
				return e
			}
			if de.IsDir() {
				if p != c.Out && slices.Contains([]string{"home", "cache", "config", "tmp", "state", "runtime"}, de.Name()) {
					return filepath.SkipDir
				}
				return nil
			}
			if !de.Type().IsRegular() {
				return fmt.Errorf("unexpected output type")
			}
			if p == filepath.Join(c.Out, "result.json") {
				return nil
			}
			pin, e := dataset.Hash(p)
			if e != nil {
				return e
			}
			rel, e := filepath.Rel(c.Out, p)
			if e != nil {
				return e
			}
			summary.Outputs[rel] = pin
			return nil
		})
		err = errors.Join(err, e)
		if e != nil {
			summary.Complete = false
			summary.Error = err.Error()
		}
		err = errors.Join(err, dataset.WriteJSON(filepath.Join(c.Out, "result.json"), summary))
	}()
	exe, e := os.Executable()
	if e != nil {
		return e
	}
	cfg := filepath.Join(c.Out, "config.json")
	pin, e := dataset.Hash(cfg)
	if e != nil {
		return e
	}
	run := func(task string, limit int64) (externalResult, error) {
		return launch(c, task, limit, []string{exe, "worker", "-config", cfg, "-config-pin", pin, "-task", task}, filepath.Join(c.Out, "inputs.json"))
	}
	if _, err = run("prepare", c.Limits.Checker); err != nil {
		return err
	}
	attemptLink := func(mode string) (externalResult, linkResult, bool, error) {
		rec, e := run("link-"+mode, c.LinkLimit)
		var result linkResult
		if e != nil {
			if rec.State == "oom" && rec.Removed && rec.Memory.LimitBytes == linkCap && rec.Memory.OOMKills > 0 {
				result.Status = "miss"
				return rec, result, false, nil
			}
			return rec, result, false, e
		}
		if e = dataset.ReadJSON(filepath.Join(c.Out, "link-"+mode, "result.json"), &result); e != nil {
			return rec, result, false, e
		}
		if _, e = run("check-link-"+mode, c.Limits.Checker); e != nil {
			return rec, result, false, e
		}
		var parity parityResult
		if e = dataset.ReadJSON(filepath.Join(c.Out, "check-link-"+mode+".json"), &parity); e != nil {
			return rec, result, false, e
		}
		if parity.Status != "pass" && parity.Status != "miss" {
			return rec, result, false, fmt.Errorf("invalid link parity result")
		}
		ok := result.Status == "pass" && parity.Status == "pass" && rec.Memory.PeakBytes > 0 && rec.Memory.PeakBytes <= linkCap
		if !ok && result.Status == "pass" {
			result.Status = "miss"
		}
		return rec, result, ok, nil
	}
	lr, l, linkOK, linkErr := attemptLink("global")
	branch, status := "global", "pass"
	if linkErr != nil {
		return linkErr
	}
	if !linkOK {
		lr, l, linkOK, linkErr = attemptLink("package")
		branch = "per-package"
		if linkErr != nil {
			return linkErr
		}
		if !linkOK {
			status = l.Status
			if status != "partial" {
				status = "miss"
			}
			branch = "stop-return-design-question"
		}
	}
	summary.Targets = append(summary.Targets, targetResult{Target: "check3-link-peak-and-parity", Status: status, Branch: branch, Measured: struct {
		PeakBytes int64
		Link      linkResult
	}{lr.Memory.PeakBytes, l}, Reason: "Synthetic qualified-name/file-scope resolution only; language resolver rules untested."})
	if status != "pass" {
		return fmt.Errorf("Check 3 did not pass: %s", branch)
	}
	deltaResults := map[string]deltaReport{}
	for _, format := range []string{"arrow", "parquet"} {
		if _, err = run("delta-"+format, c.Limits.Engine); err != nil {
			return err
		}
		if _, err = run("verify-"+format, c.Limits.Checker); err != nil {
			return err
		}
		var report deltaReport
		if err = dataset.ReadJSON(filepath.Join(c.Out, "delta-"+format, "metrics.json"), &report); err != nil {
			return err
		}
		deltaResults[format] = report
		var verification verifyReport
		if err = dataset.ReadJSON(filepath.Join(c.Out, "verify-"+format+".json"), &verification); err != nil {
			return err
		}
		parityStatus := "pass"
		if verification.NativeTopKEqual != verification.NativeTopKComparisons {
			parityStatus = "partial"
		}
		summary.Targets = append(summary.Targets, deltaTarget(format, report), targetResult{Target: "check4-" + format + "-native-ranking-parity", Status: parityStatus, Branch: "measured-diagnostic-not-a-ranking-equivalence-claim", Measured: verification})
	}
	a, p := deltaResults["arrow"], deltaResults["parquet"]
	selection := "unresolved"
	selectionStatus := "partial"
	if a.MaxEditCPU > 1e9 && p.MaxEditCPU > 1e9 {
		selection = "stop-ask-user-both-edit-bound-misses"
		selectionStatus = "miss"
	} else {
		ap, pp := deltaPass(a), deltaPass(p)
		switch {
		case ap && pp:
			selection = "both-pass-lead-selects-simplest"
			selectionStatus = "partial"
		case ap:
			selection = "arrow"
			selectionStatus = "pass"
		case pp:
			selection = "parquet"
			selectionStatus = "pass"
		default:
			selection = "no-passing-form"
			selectionStatus = "miss"
		}
	}
	summary.Targets = append(summary.Targets, targetResult{Target: "check4-delta-selection", Status: selectionStatus, Branch: selection, Measured: map[string]string{"base": "shared C Parquet plus native Bleve", "edit_batch": "alternating harness MakeChange and original-file replacement; same file churn", "scope": "full tier query workload plus delta probes; no daemon routing"}})
	var f frozen
	if err = dataset.ReadJSON(filepath.Join(c.Out, "frozen.json"), &f); err != nil {
		return err
	}
	pins, e := treePins(c.Data)
	if e != nil {
		return e
	}
	if !reflect.DeepEqual(pins, f.Protected) {
		return fmt.Errorf("protected dataset changed")
	}
	return nil
}
func prepare(ctx context.Context, c config) error {
	ds, m, err := loadTier(c.Data)
	if err != nil {
		return err
	}
	qs, err := querySet(c, ds)
	if err != nil {
		return err
	}
	if err = dataset.WriteJSON(filepath.Join(c.Out, "queries.json"), qs); err != nil {
		return err
	}
	var input dataset.Dataset
	var repos []string
	for _, d := range ds {
		repos = append(repos, d.Repo)
		if d.Repo == c.DeltaRepo {
			input = d
		}
	}
	pins, err := treePins(c.Data)
	if err != nil {
		return err
	}
	f := frozen{Counts: m.Counts, Repos: repos, QueryCount: len(qs), Protected: pins}
	if err = dataset.WriteJSON(filepath.Join(c.Out, "frozen.json"), f); err != nil {
		return err
	}
	if err = prepareLink(ctx, ds, filepath.Join(c.Out, "facts")); err != nil {
		return err
	}
	c1, err := runner.MakeChange(input)
	if err != nil {
		return err
	}
	// Keep the harness-selected file's original complete output for alternating
	// edits. This grows sealed chain length without silently cloning repositories.
	var c0 adapter.FileChange
	c0.Path = c1.Path
	c0.Replacement.Repo = input.Repo
	owned := map[string]bool{}
	for n, e := range input.NodeRows() {
		if e != nil {
			return e
		}
		if n.FilePath == c0.Path {
			owned[n.ID] = true
			c0.Replacement.Nodes = append(c0.Replacement.Nodes, n)
		}
	}
	for v, e := range input.EdgeRows() {
		if e != nil {
			return e
		}
		if v.FilePath == c0.Path || owned[v.From] {
			c0.Replacement.Edges = append(c0.Replacement.Edges, v)
		}
	}
	for v, e := range input.FileRows() {
		if e != nil {
			return e
		}
		if v.Path == c0.Path {
			c0.Replacement.Files = append(c0.Replacement.Files, v)
		}
	}
	for v, e := range input.SymbolRows() {
		if e != nil {
			return e
		}
		if owned[v.NodeID] {
			c0.Replacement.Symbols = append(c0.Replacement.Symbols, v)
		}
	}
	for v, e := range input.ContentRows() {
		if e != nil {
			return e
		}
		if v.FilePath == c0.Path || owned[v.NodeID] {
			c0.Replacement.Content = append(c0.Replacement.Content, v)
		}
	}
	for v, e := range input.VectorRows() {
		if e != nil {
			return e
		}
		if owned[v.NodeID] {
			c0.Replacement.Vectors = append(c0.Replacement.Vectors, v)
		}
	}
	if err = dataset.WriteJSON(filepath.Join(c.Out, "changes.json"), []adapter.FileChange{c1, c0}); err != nil {
		return err
	}
	// Exactly two clean disk snapshots serve every odd/even checkpoint. These
	// are independently constructed from the source and the declared edit batch.
	changed, err := adapter.ChangeSource(input, c1)
	if err != nil {
		return err
	}
	_, err = dataset.Write(filepath.Join(c.Out, "changed-source"), changed)
	return err
}
func compareEdgeFiles(want, got, out string) (err error) {
	if err = os.Mkdir(out, 0700); err != nil {
		return err
	}
	for i, p := range []string{want, got} {
		cmd := exec.Command("sort", "-S", "4M", "-T", out, "-o", filepath.Join(out, fmt.Sprint(i)), p)
		cmd.Env = append(os.Environ(), "LC_ALL=C")
		if b, e := cmd.CombinedOutput(); e != nil {
			return fmt.Errorf("sort edge multiset: %w: %s", e, b)
		}
	}
	a, err := os.Open(filepath.Join(out, "0"))
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, a.Close()) }()
	b, err := os.Open(filepath.Join(out, "1"))
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, b.Close()) }()
	ra, rb := bufio.NewReader(a), bufio.NewReader(b)
	for {
		la, ea := ra.ReadString('\n')
		lb, eb := rb.ReadString('\n')
		if la != lb {
			return errEdgeMismatch
		}
		if ea == io.EOF && eb == io.EOF {
			return nil
		}
		if ea != nil || eb != nil {
			return errors.Join(ea, eb)
		}
	}
}
func cpuNS() (int64, error) {
	var u syscall.Rusage
	if err := syscall.Getrusage(syscall.RUSAGE_SELF, &u); err != nil {
		return 0, err
	}
	return u.Utime.Sec*1e9 + u.Utime.Usec*1e3 + u.Stime.Sec*1e9 + u.Stime.Usec*1e3, nil
}

type checkpoint struct {
	Size                  int
	BaseP95, DeltaP95     map[string]float64
	Ratio                 map[string]float64
	WorstRatio            float64
	DeltaBytes, BaseBytes int64
	ByteRatio             float64
}
type deltaReport struct {
	Format           string
	MaxEditCPU       int64
	Visibility       runner.Distribution
	Edits            int
	Checkpoints      []checkpoint
	CompactionBranch string
	Threshold        *int
	LowerBound       *int
}
type sample struct {
	Size   int
	Repeat int
	Form   string
	Query  workload.Query
	Answer workload.Answer
}

func measureDeltas(ctx context.Context, c config, format string) (err error) {
	ds, _, err := loadTier(c.Data)
	if err != nil {
		return err
	}
	var changes []adapter.FileChange
	if err = dataset.ReadJSON(filepath.Join(c.Out, "changes.json"), &changes); err != nil {
		return err
	}
	var qs []workload.Query
	if err = dataset.ReadJSON(filepath.Join(c.Out, "queries.json"), &qs); err != nil {
		return err
	}
	out := filepath.Join(c.Out, "delta-"+format)
	if err = os.Mkdir(out, 0700); err != nil {
		return err
	}
	spool, err := os.Create(filepath.Join(out, "answers.jsonl"))
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, spool.Close()) }()
	enc := json.NewEncoder(spool)
	b := backend{format: format, baseBackend: cbase.Backend{SideIndex: false}}
	l, err := newLifecycle(out, b)
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, l.close()) }()
	segments := map[string]adapter.Segment{}
	defer func() {
		for _, s := range segments {
			err = errors.Join(err, s.Close())
		}
	}()
	var current adapter.SegmentRef
	var baseBytes int64
	for i, d := range ds {
		r, stats, e := b.Build(ctx, d, filepath.Join(out, fmt.Sprintf("base-%d", i)))
		if e != nil {
			return e
		}
		if d.Repo == c.DeltaRepo {
			current = r
			baseBytes = stats.Bytes
			if e = l.publish(ctx, r, nil); e != nil {
				return e
			}
		}
		s, e := b.Open(ctx, r.Path, r)
		if e != nil {
			return e
		}
		if external := s.(*segment).external; external != nil {
			segments[d.Repo] = external
		} else {
			segments[d.Repo] = s
		}
	}
	report := deltaReport{Format: format}
	var visibility []int64
	var deltaBytes int64
	for n := 1; n <= c.Sizes[len(c.Sizes)-1]; n++ {
		old := current
		c0, e := cpuNS()
		if e != nil {
			return e
		}
		start := time.Now()
		next, stats, e := b.BuildDelta(ctx, old, changes[(n-1)%2], filepath.Join(out, fmt.Sprintf("delta-%06d", n)))
		if e != nil {
			return e
		}
		if e = l.publish(ctx, next, &old); e != nil {
			return e
		}
		visible, release, e := l.pin(ctx, c.DeltaRepo)
		if e != nil {
			return e
		}
		if _, e = visible.FileNodes(ctx, changes[0].Path); e != nil {
			_ = release()
			return e
		}
		elapsed := time.Since(start).Nanoseconds()
		c1, e := cpuNS()
		if e != nil {
			_ = release()
			return e
		}
		if e = release(); e != nil {
			return e
		}
		report.MaxEditCPU = max(report.MaxEditCPU, c1-c0)
		visibility = append(visibility, elapsed)
		report.Edits++
		deltaBytes += stats.Bytes
		current = next
		if !slices.Contains(c.Sizes, n) {
			continue
		}
		// Equal visible data, two physical layouts. The clean base is rebuilt from
		// the independent odd/even source, never from the candidate's own answers.
		inputPath := filepath.Join(c.Data, c.DeltaRepo)
		if len(ds) == 1 {
			inputPath = ds[0].Source.Dir
		}
		if n%2 == 1 {
			inputPath = filepath.Join(c.Out, "changed-source")
		}
		source, e := dataset.OpenSource(inputPath)
		if e != nil {
			return e
		}
		clean, _, e := b.Build(ctx, source, filepath.Join(out, fmt.Sprintf("compact-%06d", n)))
		if e != nil {
			return e
		}
		base, e := b.Open(ctx, clean.Path, clean)
		if e != nil {
			return e
		}
		delta, e := b.Open(ctx, current.Path, current)
		if e != nil {
			return errors.Join(e, base.Close())
		}
		cp := checkpoint{Size: n, BaseP95: map[string]float64{}, DeltaP95: map[string]float64{}, Ratio: map[string]float64{}, DeltaBytes: deltaBytes, BaseBytes: baseBytes, ByteRatio: float64(deltaBytes) / float64(baseBytes)}
		timings := map[string]map[string][]int64{"base": {}, "delta": {}}
		// Warm each layout with the identical complete query sequence. Both see warm
		// OS caches. Serial alternating order limits systematic ordering bias.
		for repeat := -1; repeat < c.Repeats; repeat++ {
			forms := []string{"base", "delta"}
			if repeat%2 == 0 {
				slices.Reverse(forms)
			}
			for _, form := range forms {
				selected := base
				if external := base.(*segment).external; external != nil {
					selected = external
				}
				if form == "delta" {
					selected = delta
				}
				view := map[string]adapter.Segment{}
				for repo, s := range segments {
					view[repo] = s
				}
				view[c.DeltaRepo] = selected
				fan := adapter.Fanout{Segments: view}
				for _, q := range qs {
					start := time.Now()
					answer, e := workload.Evaluate(ctx, view, fan, q)
					ns := time.Since(start).Nanoseconds()
					if e != nil {
						return errors.Join(e, base.Close(), delta.Close())
					}
					if repeat >= 0 {
						timings[form][q.Op] = append(timings[form][q.Op], ns)
					}
					if e = enc.Encode(sample{n, repeat, form, q, answer}); e != nil {
						return errors.Join(e, base.Close(), delta.Close())
					}
				}
			}
		}
		if e = errors.Join(base.Close(), delta.Close()); e != nil {
			return e
		}
		for op, ns := range timings["base"] {
			a := runner.Summarize(ns).P95
			d := runner.Summarize(timings["delta"][op]).P95
			if a <= 0 {
				return fmt.Errorf("nonpositive baseline timing")
			}
			cp.BaseP95[op] = a
			cp.DeltaP95[op] = d
			cp.Ratio[op] = d / a
			cp.WorstRatio = max(cp.WorstRatio, d/a)
		}
		report.Checkpoints = append(report.Checkpoints, cp)
	}
	report.Visibility = runner.Summarize(visibility)
	report = threshold(report)
	return dataset.WriteJSON(filepath.Join(out, "metrics.json"), report)
}
func threshold(r deltaReport) deltaReport {
	for i, p := range r.Checkpoints {
		if p.WorstRatio > 1.10 {
			r.CompactionBranch = "observed-10-percent-crossing"
			if i == 0 {
				r.CompactionBranch = "crossing-at-smallest-size-threshold-unresolved"
			} else {
				n := r.Checkpoints[i-1].Size
				r.Threshold = &n
			}
			return r
		}
	}
	r.CompactionBranch = "no-crossing-lower-bound-only"
	if len(r.Checkpoints) > 0 {
		n := r.Checkpoints[len(r.Checkpoints)-1].Size
		r.LowerBound = &n
	}
	return r
}
func deltaPass(r deltaReport) bool {
	if r.MaxEditCPU > 1e9 || r.Visibility.P95 > 5e9 || len(r.Checkpoints) == 0 {
		return false
	}
	var chosen *int
	if r.Threshold != nil {
		chosen = r.Threshold
	} else {
		chosen = r.LowerBound
	}
	if chosen == nil {
		return false
	}
	for _, p := range r.Checkpoints {
		if p.Size == *chosen {
			return p.WorstRatio <= 1.10 && p.ByteRatio <= 2
		}
	}
	return false
}
func deltaTarget(format string, r deltaReport) targetResult {
	status := "pass"
	if !deltaPass(r) {
		status = "miss"
	}
	return targetResult{Target: "check4-" + format, Status: status, Branch: r.CompactionBranch, Measured: r}
}

type verifyReport struct {
	Status                                          string
	Checked, NativeTopKComparisons, NativeTopKEqual int
	NativeTopKMatchRate                             float64
	DifferencesSHA256                               string
}

func verifyDeltas(ctx context.Context, c config, format string) (err error) {
	ds, _, err := loadTier(c.Data)
	if err != nil {
		return err
	}
	changed, err := dataset.OpenSource(filepath.Join(c.Out, "changed-source"))
	if err != nil {
		return err
	}
	originalView, _ := oracle.DiskView(ds)
	changedDS := slices.Clone(ds)
	for i, d := range changedDS {
		if d.Repo == c.DeltaRepo {
			changedDS[i] = changed
		}
	}
	changedView, _ := oracle.DiskView(changedDS)
	scratch, err := os.MkdirTemp(c.Out, "verify-native-"+format+"-")
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, os.RemoveAll(scratch)) }()
	pairDir, memoDir := filepath.Join(scratch, "pairs"), filepath.Join(scratch, "memo")
	if err = os.Mkdir(pairDir, 0700); err != nil {
		return err
	}
	if err = os.Mkdir(memoDir, 0700); err != nil {
		return err
	}
	diffPath := filepath.Join(c.Out, "native-rank-differences-"+format+".jsonl")
	diff, err := os.Create(diffPath)
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, diff.Close()) }()
	enc := json.NewEncoder(diff)
	report := verifyReport{Status: "pass"}
	err = eachJSON(filepath.Join(c.Out, "delta-"+format, "answers.jsonl"), func(s sample) error {
		view, sources := originalView, ds
		if s.Size%2 == 1 {
			view, sources = changedView, changedDS
		}
		q := s.Query
		fullKey, e := json.Marshal(struct {
			Odd    bool
			Query  workload.Query
			Answer workload.Answer
		}{s.Size%2 == 1, s.Query, s.Answer})
		if e != nil {
			return e
		}
		memoPath := filepath.Join(memoDir, fmt.Sprintf("%x", sha256.Sum256(fullKey)))
		_, memoErr := os.Stat(memoPath)
		if memoErr != nil && !os.IsNotExist(memoErr) {
			return memoErr
		}
		if os.IsNotExist(memoErr) {
			want, e := workload.Evaluate(ctx, view, adapter.Fanout{Segments: view}, s.Query)
			if e != nil {
				return e
			}
			if workload.Lexical(q.Op) {
				ids := make([]string, len(s.Answer.Hits))
				for i, h := range s.Answer.Hits {
					ids[i] = h.ID
				}
				expected, e := oracle.DiskReference(ctx, sources, q, ids)
				if e != nil {
					return e
				}
				q.NativeScoring = true
				q.MatchRepos = expected.MatchRepos
				// Full marker coverage is exact. Other native top-k ranks are diagnostic,
				// because per-index statistics can legitimately change the ordered top-k.
				q.ExactRank = strings.HasPrefix(q.ID, "delta-marker-")
				q.MinOverlap = 0
			}
			check := workload.Compare(q, want, s.Answer)
			if check.Mismatch {
				return fmt.Errorf("%s size %d %s query %s: %s", format, s.Size, s.Form, q.ID, check.Detail)
			}
			if e = os.WriteFile(memoPath, []byte("checked-complete\n"), 0600); e != nil {
				return e
			}
		}
		report.Checked++
		if !workload.Lexical(q.Op) || s.Repeat < 0 {
			return nil
		}
		key, _ := json.Marshal(struct {
			Size, Repeat int
			ID           string
		}{s.Size, s.Repeat, q.ID})
		name := fmt.Sprintf("%x", sha256.Sum256(key))
		other := "base"
		if s.Form == "base" {
			other = "delta"
		}
		var paired sample
		p := filepath.Join(pairDir, name+"-"+other+".json")
		e = dataset.ReadJSON(p, &paired)
		if os.IsNotExist(e) {
			return dataset.WriteJSON(filepath.Join(pairDir, name+"-"+s.Form+".json"), s)
		}
		if e != nil {
			return e
		}
		a, b := paired, s
		if a.Form != "base" {
			a, b = b, a
		}
		ids := func(hits []adapter.Hit) []string {
			out := make([]string, len(hits))
			for i, h := range hits {
				out[i] = h.ID
			}
			return out
		}
		report.NativeTopKComparisons++
		if slices.Equal(ids(a.Answer.Hits), ids(b.Answer.Hits)) {
			report.NativeTopKEqual++
		} else {
			if e = enc.Encode(struct {
				Size, Repeat int
				Query        workload.Query
				Clean, Delta []adapter.Hit
			}{s.Size, s.Repeat, s.Query, a.Answer.Hits, b.Answer.Hits}); e != nil {
				return e
			}
		}
		return os.Remove(p)
	})
	if err != nil {
		return err
	}
	if report.Checked == 0 {
		return fmt.Errorf("empty answer spool")
	}
	entries, err := os.ReadDir(pairDir)
	if err != nil {
		return err
	}
	if len(entries) != 0 {
		return fmt.Errorf("unpaired native rank samples")
	}
	if report.NativeTopKComparisons > 0 {
		report.NativeTopKMatchRate = float64(report.NativeTopKEqual) / float64(report.NativeTopKComparisons)
	}
	if err = diff.Sync(); err != nil {
		return err
	}
	report.DifferencesSHA256, err = dataset.Hash(diffPath)
	if err != nil {
		return err
	}
	return dataset.WriteJSON(filepath.Join(c.Out, "verify-"+format+".json"), report)
}
