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
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"syscall"
	"time"
)

const linkCap int64 = 1610612736
const finalizationReserve = 60 * time.Second

func launchPolicy(c config, name string) string {
	if name == "controller" {
		return filepath.Join(c.Out, "policy.json")
	}
	return filepath.Join(c.Out, "worker-policy.json")
}
func writePolicies(c config, now time.Time) error {
	deadline := now.Add(time.Duration(c.WallSeconds) * time.Second)
	workerDeadline := deadline.Add(-finalizationReserve)
	if !workerDeadline.After(now) {
		return fmt.Errorf("wall budget must exceed the 60-second controller finalization reserve")
	}
	for name, until := range map[string]time.Time{"controller": deadline, "worker": workerDeadline} {
		if err := dataset.WriteJSON(launchPolicy(c, name), struct {
			Schema   string    `json:"schema"`
			Deadline time.Time `json:"deadline"`
		}{"external-limits/v1", until}); err != nil {
			return err
		}
	}
	return nil
}

var errEdgeMismatch = errors.New("full edge multiset differs")
var errAnswerMismatch = errors.New("answer differs")

type parityResult struct {
	Status string
	Error  string
}

// Every resource and workload choice is explicit in the hash-bound operator file.
type config struct {
	Step           string              `json:"step,omitempty"`
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
	Schema    string            `json:"schema"`
	Targets   []targetResult    `json:"targets"`
	Receipts  map[string]string `json:"receipts_sha256"`
	Outputs   map[string]string `json:"outputs_sha256"`
	Complete  bool              `json:"complete"`
	Protected string            `json:"protected_inputs"`
	Error     string            `json:"error,omitempty"`
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
	if c.Step != "" && c.Step != "check4" {
		return c, fmt.Errorf("unsupported linkdelta step %q", c.Step)
	}
	if c.BaseLayout != "c-parquet" {
		return c, fmt.Errorf("measurement requires the C Parquet base")
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
	if err = writePolicies(c, time.Now()); err != nil {
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
		Schema              string
		SourceRevision      string
		Inputs              map[string]string
		Controller          externalResult
		ResultSHA256        string
		Complete            bool
		Error               string
		ControllerLogSHA256 string
		ProtectedInputs     string
	}{Schema: "linkdelta-terminal/v1", SourceRevision: c.SourceRevision, Inputs: meta, Controller: receipt, ResultSHA256: resultPin, Complete: runErr == nil && resultErr == nil}
	logPin, logErr := dataset.Hash(filepath.Join(c.Out, "controller-scope", "command.log"))
	protected, protectedErr := protectedInputs(c)
	final.ControllerLogSHA256 = logPin
	final.ProtectedInputs = protected
	runErr = errors.Join(runErr, logErr, protectedErr)
	final.Complete = runErr == nil && resultErr == nil
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
	policy := launchPolicy(c, name)
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
	case "prepare-link":
		ds, _, e := loadTier(c.Data)
		if e != nil {
			return e
		}
		return prepareLink(ctx, ds, filepath.Join(c.Out, "facts"))
	case "references":
		return prepareReferences(ctx, c)
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
func unfinishedReason(err error) string {
	if err != nil {
		return "Not completed: " + err.Error()
	}
	return "No complete evidence was produced."
}
func control(c config) (err error) {
	summary := runSummary{Schema: "linkdelta-run/v1", Receipts: map[string]string{}, Outputs: map[string]string{}}
	defer func() {
		protected, checkErr := protectedInputs(c)
		summary.Protected = protected
		err = errors.Join(err, checkErr)
		expected := []string{"check4-arrow", "check4-parquet", "check4-delta-selection"}
		if c.Step != "check4" {
			expected = append(expected, "check3-link-peak-and-parity")
		}
		for _, target := range expected {
			found := false
			for _, result := range summary.Targets {
				if result.Target == target {
					found = true
				}
			}
			if !found {
				summary.Targets = append(summary.Targets, targetResult{Target: target, Status: "blocked", Branch: "not-completed", Reason: unfinishedReason(err)})
			}
		}
		for _, name := range []string{"prepare", "prepare-link", "references", "link-global", "check-link-global", "link-package", "check-link-package", "delta-arrow", "delta-parquet", "verify-arrow", "verify-parquet"} {
			p := filepath.Join(c.Out, name+"-scope", "receipt.json")
			if pin, e := dataset.Hash(p); e == nil {
				summary.Receipts[name] = pin
			} else if !os.IsNotExist(e) {
				err = errors.Join(err, e)
			}
		}
		// The live controller scope is owned by the outer terminal receipt.
		outputs, e := outputPins(c.Out)
		summary.Outputs = outputs
		err = errors.Join(err, e)
		summary.Complete = err == nil
		if err != nil {
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
		var policy struct{ Deadline time.Time }
		if e := dataset.ReadJSON(launchPolicy(c, task), &policy); e != nil {
			return externalResult{}, e
		}
		if !time.Now().Before(policy.Deadline) {
			return externalResult{}, fmt.Errorf("worker deadline expired; controller finalization reserve remains")
		}
		approval, e := dataset.Hash(c.Approval)
		if e != nil {
			return externalResult{}, e
		}
		if approval != c.ApprovalSHA256 {
			return externalResult{}, fmt.Errorf("approval changed")
		}
		return launch(c, task, limit, []string{exe, "worker", "-config", cfg, "-config-pin", pin, "-task", task}, filepath.Join(c.Out, "inputs.json"))
	}
	summary.Targets, err = collectTargets(c, run)
	return err
}

// Hash finalized outputs only. The outer receipt owns the live controller scope.
func outputPins(root string) (map[string]string, error) {
	pins := map[string]string{}
	err := filepath.WalkDir(root, func(p string, de os.DirEntry, e error) error {
		if e != nil {
			return e
		}
		if de.IsDir() {
			if p == filepath.Join(root, "controller-scope") || (p != root && slices.Contains([]string{"home", "cache", "config", "tmp", "state", "runtime"}, de.Name())) {
				return filepath.SkipDir
			}
			return nil
		}
		if !de.Type().IsRegular() {
			return fmt.Errorf("unexpected output type")
		}
		if p == filepath.Join(root, "result.json") {
			return nil
		}
		pin, e := dataset.Hash(p)
		if e != nil {
			return e
		}
		rel, e := filepath.Rel(root, p)
		if e != nil {
			return e
		}
		pins[rel] = pin
		return nil
	})
	return pins, err
}

// Only an accounted, removed terminal scope permits an independent successor.
func safeReceipt(r externalResult, limit int64) bool {
	if !r.Removed || r.Memory.LimitBytes != limit || r.Memory.PID <= 0 || r.Memory.PeakBytes <= 0 {
		return false
	}
	switch r.State {
	case "completed":
		return r.Memory.Status == "completed" && r.Memory.ExitCode == 0 && r.Memory.OOMKills == 0
	case "oom":
		return r.Memory.Status == "failed" && r.Memory.OOMKills > 0
	case "failed":
		return r.Memory.Status == "failed" && r.Memory.ExitCode != 0 && r.Memory.OOMKills == 0
	}
	return false
}
func containedFailure(r externalResult, limit int64) bool {
	return r.State != "completed" && safeReceipt(r, limit)
}
func memoryFallback(r externalResult) bool {
	return safeReceipt(r, linkCap) && ((r.State == "oom" && r.Memory.OOMKills > 0) || (r.State == "completed" && r.Memory.PeakBytes > linkCap))
}

type linkMeasurement struct {
	Memory                      runner.ComponentMemory
	Link                        linkResult
	Parity                      parityResult
	Collisions                  collisionStats
	NameScopeCollisionFactShare float64
	OrdinalFactShare            float64
}

func linkTarget(mode string, r externalResult, l linkResult, parity parityResult, collisions collisionStats) targetResult {
	status := "miss"
	if l.Status == "partial" {
		status = "partial"
	}
	if l.Status == "pass" && parity.Status == "pass" && safeReceipt(r, linkCap) && r.State == "completed" && r.Memory.PeakBytes <= linkCap {
		status = "pass"
	}
	m := linkMeasurement{Memory: r.Memory, Link: l, Parity: parity, Collisions: collisions}
	if l.Facts > 0 {
		m.NameScopeCollisionFactShare = float64(collisions.NameScopeFacts) / float64(l.Facts)
		m.OrdinalFactShare = float64(collisions.ExtendedFacts) / float64(l.Facts)
	}
	return targetResult{Target: "check3-link-peak-and-parity", Status: status, Branch: mode, Measured: m, Reason: "Synthetic projected name/scope/kind/span/ordinal linking only; language resolver rules untested."}
}
func collectTargets(c config, run func(string, int64) (externalResult, error)) ([]targetResult, error) {
	var targets []targetResult
	rec, e := run("prepare", c.Limits.Checker)
	if e != nil {
		return targets, e
	}
	if c.Step != "check4" {
		rec, e = run("prepare-link", c.Limits.Checker)
		var collisions collisionStats
		if e == nil {
			e = dataset.ReadJSON(filepath.Join(c.Out, "facts", "collisions.json"), &collisions)
			if e != nil {
				targets = append(targets, targetResult{Target: "check3-link-peak-and-parity", Status: "blocked", Branch: "collision-evidence", Reason: e.Error()})
			}
		} else {
			targets = append(targets, targetResult{Target: "check3-link-peak-and-parity", Status: "blocked", Branch: "link-preparation", Reason: e.Error()})
			if !containedFailure(rec, c.Limits.Checker) {
				return targets, e
			}
		}
		if e == nil {
			attempt := func(mode string) (targetResult, bool, error) {
				r, err := run("link-"+mode, c.LinkLimit)
				var l linkResult
				if err != nil {
					if !containedFailure(r, c.LinkLimit) {
						return targetResult{}, false, err
					}
					status := "blocked"
					if memoryFallback(r) {
						status = "miss"
					}
					return targetResult{Target: "check3-link-peak-and-parity", Status: status, Branch: mode, Measured: r, Reason: err.Error()}, memoryFallback(r), nil
				}
				if err = dataset.ReadJSON(filepath.Join(c.Out, "link-"+mode, "result.json"), &l); err != nil {
					return targetResult{Target: "check3-link-peak-and-parity", Status: "blocked", Branch: mode, Reason: err.Error()}, false, nil
				}
				check, err := run("check-link-"+mode, c.Limits.Checker)
				if err != nil {
					if !containedFailure(check, c.Limits.Checker) {
						return targetResult{}, false, err
					}
					return targetResult{Target: "check3-link-peak-and-parity", Status: "blocked", Branch: mode, Reason: err.Error()}, false, nil
				}
				var parity parityResult
				if err = dataset.ReadJSON(filepath.Join(c.Out, "check-link-"+mode+".json"), &parity); err != nil {
					return targetResult{Target: "check3-link-peak-and-parity", Status: "blocked", Branch: mode, Reason: err.Error()}, false, nil
				}
				return linkTarget(mode, r, l, parity, collisions), memoryFallback(r), nil
			}
			result, fallback, err := attempt("global")
			if err != nil {
				return targets, err
			}
			if fallback {
				global := result
				global.Target = "check3-global-attempt"
				targets = append(targets, global)
				result, _, err = attempt("package")
				if err != nil {
					return targets, err
				}
			}
			targets = append(targets, result)
		}
	}
	// References belong only to Check 4. A semantic Check 3 miss never reaches
	// this dependency or suppresses either physical form.
	rec, e = run("references", c.Limits.Checker)
	if e != nil {
		for _, f := range []string{"arrow", "parquet"} {
			targets = append(targets, targetResult{Target: "check4-" + f, Status: "blocked", Branch: "reference-preparation", Reason: e.Error()})
		}
		if !containedFailure(rec, c.Limits.Checker) {
			return targets, e
		}
		targets = append(targets, selectDelta(map[string]formResult{}))
		return targets, nil
	}
	results := map[string]formResult{}
	for _, format := range []string{"arrow", "parquet"} {
		results[format] = formResult{Status: "blocked"}
		rec, e = run("delta-"+format, c.Limits.Engine)
		if e != nil {
			targets = append(targets, targetResult{Target: "check4-" + format, Status: "blocked", Branch: "engine", Measured: rec, Reason: e.Error()})
			if !containedFailure(rec, c.Limits.Engine) {
				return targets, e
			}
			continue
		}
		engine := rec
		rec, e = run("verify-"+format, c.Limits.Checker)
		if e != nil {
			status := "blocked"
			var verification verifyReport
			if rec.State == "failed" && containedFailure(rec, c.Limits.Checker) && dataset.ReadJSON(filepath.Join(c.Out, "verify-"+format+".json"), &verification) == nil && verification.Status == "miss" && verification.Error != "" {
				status = "miss"
			}
			results[format] = formResult{Status: status}
			targets = append(targets, targetResult{Target: "check4-" + format, Status: status, Branch: "correctness-verification", Measured: rec, Reason: e.Error()})
			if !containedFailure(rec, c.Limits.Checker) {
				return targets, e
			}
			continue
		}
		var report deltaReport
		var verification verifyReport
		e = errors.Join(dataset.ReadJSON(filepath.Join(c.Out, "delta-"+format, "metrics.json"), &report), dataset.ReadJSON(filepath.Join(c.Out, "verify-"+format+".json"), &verification))
		if e != nil || verification.Status != "pass" || verification.Checked == 0 {
			reason := "Verification has no complete pass evidence."
			if e != nil {
				reason = e.Error()
			}
			targets = append(targets, targetResult{Target: "check4-" + format, Status: "blocked", Branch: "missing-or-invalid-evidence", Reason: reason})
			continue
		}
		report.EngineMemory = &engine.Memory
		target := deltaTarget(format, report)
		results[format] = formResult{Status: target.Status, Report: report}
		targets = append(targets, target)
		status := "pass"
		if verification.NativeTopKEqual != verification.NativeTopKComparisons {
			status = "partial"
		}
		targets = append(targets, targetResult{Target: "check4-" + format + "-native-ranking-parity", Status: status, Branch: "measured-diagnostic-not-a-ranking-equivalence-claim", Measured: verification})
	}
	targets = append(targets, selectDelta(results))
	return targets, nil
}
func protectedInputs(c config) (string, error) {
	var f frozen
	if e := dataset.ReadJSON(filepath.Join(c.Out, "frozen.json"), &f); e != nil {
		return "not checked: frozen snapshot unavailable", e
	}
	pins, e := treePins(c.Data)
	if e != nil {
		return "check failed", e
	}
	if !reflect.DeepEqual(pins, f.Protected) {
		return "changed", fmt.Errorf("protected dataset changed")
	}
	return "unchanged", nil
}

func prepare(ctx context.Context, c config) error {
	ds, m, err := loadTier(c.Data)
	if err != nil {
		return err
	}
	if len(ds) == 0 || c.DeltaRepo != ds[0].Repo {
		return fmt.Errorf("delta_repo must be ds[0].Repo for the pinned tier workload")
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
	return u.Utime.Sec*1e9 + int64(u.Utime.Usec)*1e3 + u.Stime.Sec*1e9 + int64(u.Stime.Usec)*1e3, nil
}

type checkpoint struct {
	Size                                   int
	ReadLimitNS, AALimitNS, AADifferenceNS map[string]float64
	ReadVerdict                            map[string]string
	HiddenBaseTextRows                     map[string]int
	BaseP95, WrapperP95, DeltaP95          map[string]float64
	Ratio, AARatio                         map[string]float64
	WorstRatio, AAWorstRatio               float64
	DeltaBytes, BaseBytes                  int64
	ByteRatio                              float64
	MaxEditCPU                             int64
	Visibility                             runner.Distribution
	OpenNS                                 int64
}
type editSample struct {
	Size            int
	CPU, Visibility int64
	TextRebuildCPU  int64
}
type deltaReport struct {
	Format, ReadCostScope string
	EngineMemory          *runner.ComponentMemory `json:"engine_memory,omitempty"`
	MaxEditCPU            int64
	Visibility            runner.Distribution
	EditSamples           []editSample
	Edits                 int
	Checkpoints           []checkpoint
	CompactionBranch      string
	Threshold, LowerBound *int
}
type sample struct {
	Size, Repeat   int
	TimingPosition int `json:"timing_position"`
	Form           string
	Query          workload.Query
	ElapsedNS      int64  `json:"elapsed_ns"`
	AnswerSHA256   string `json:"answer_sha256"`
}

func appendSynced(f *os.File, v any) error {
	if err := json.NewEncoder(f).Encode(v); err != nil {
		return err
	}
	return f.Sync()
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
	edits, err := os.Create(filepath.Join(out, "edits.jsonl"))
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, edits.Close()) }()
	checkpoints, err := os.Create(filepath.Join(out, "checkpoints.jsonl"))
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, checkpoints.Close()) }()
	b := newBackend(format)
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
	cleanRefs := map[bool]adapter.SegmentRef{}
	var baseBytes int64
	for i, d := range ds {
		r, stats, e := b.Build(ctx, d, filepath.Join(out, fmt.Sprintf("base-%d", i)))
		if e != nil {
			return e
		}
		if d.Repo == c.DeltaRepo {
			current = r
			cleanRefs[false] = r
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
	report := deltaReport{Format: format, ReadCostScope: "Both codecs use the same typed indexed merge. Per-query paired A/A timings resolve each operation's own limit; no A/A subtraction is used. Codec comparison is write/seal/open/bytes."}
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
		b.cache.mu.Lock()
		textCPU := b.cache.TextCPU[next.Digest]
		b.cache.mu.Unlock()
		edit := editSample{Size: n, CPU: c1 - c0, Visibility: elapsed, TextRebuildCPU: textCPU}
		if e = appendSynced(edits, edit); e != nil {
			return e
		}
		adapter.Checkpoint("linkdelta-after-edit")
		report.EditSamples = append(report.EditSamples, edit)
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
		clean, ok := cleanRefs[n%2 == 1]
		if !ok {
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
			clean, _, e = b.Build(ctx, source, filepath.Join(out, fmt.Sprintf("compact-%06d", n)))
			if e != nil {
				return e
			}
			cleanRefs[n%2 == 1] = clean
		}
		base, e := b.Open(ctx, clean.Path, clean)
		if e != nil {
			return e
		}
		openStart := time.Now()
		delta, e := b.Open(ctx, current.Path, current)
		if e != nil {
			return errors.Join(e, base.Close())
		}
		cp := checkpoint{HiddenBaseTextRows: map[string]int{"content": delta.(*segment).hiddenTextCount("content"), "symbols": delta.(*segment).hiddenTextCount("symbol")}, Size: n, DeltaBytes: deltaBytes, BaseBytes: baseBytes, ByteRatio: float64(deltaBytes) / float64(baseBytes), MaxEditCPU: report.MaxEditCPU, Visibility: runner.Summarize(visibility), OpenNS: time.Since(openStart).Nanoseconds()}
		timings := map[string]map[string][]int64{"base": {}, "wrapper": {}, "delta": {}}
		// Pair equal queries in balanced order, with no evidence serialization
		// between their timed forms. Whole-workload form blocks confound A/A
		// tails with drift and evidence I/O between equivalent observations.
		views := map[string]map[string]adapter.Segment{}
		for form, selected := range map[string]adapter.Segment{"base": base.(*segment).external, "wrapper": base, "delta": delta} {
			view := map[string]adapter.Segment{}
			for repo, s := range segments {
				view[repo] = s
			}
			view[c.DeltaRepo] = selected
			views[form] = view
		}
		for repeat := -1; repeat < c.Repeats; repeat++ {
			for qi, q := range qs {
				var answers [3]workload.Answer
				var records [3]sample
				for i, form := range timingOrder(repeat + 1 + qi) {
					view := views[form]
					start := time.Now()
					answer, e := workload.Evaluate(ctx, view, adapter.Fanout{Segments: view}, q)
					ns := time.Since(start).Nanoseconds()
					if e != nil {
						return errors.Join(e, base.Close(), delta.Close())
					}
					if repeat >= 0 {
						timings[form][q.Op] = append(timings[form][q.Op], ns)
					}
					answers[i] = answer
					records[i] = sample{Size: n, Repeat: repeat, TimingPosition: i, Form: form, Query: q, ElapsedNS: ns}
				}
				for i, answer := range answers {
					pin, e := storeAnswer(filepath.Join(c.Out, "answer-blobs"), answer)
					if e != nil {
						return errors.Join(e, base.Close(), delta.Close())
					}
					records[i].AnswerSHA256 = pin
				}
				// Verification adopts the clean answer before its dependent forms.
				// TimingPosition retains actual timed order, not this spool order.
				for _, form := range []string{"base", "wrapper", "delta"} {
					for _, record := range records {
						if record.Form != form {
							continue
						}
						if e = enc.Encode(record); e != nil {
							return errors.Join(e, base.Close(), delta.Close())
						}
					}
				}
			}
		}
		if e = errors.Join(base.Close(), delta.Close()); e != nil {
			return e
		}
		cp, e = summarizeTimings(cp, timings)
		if e != nil {
			return e
		}
		if e = spool.Sync(); e != nil {
			return e
		}
		if e = appendSynced(checkpoints, cp); e != nil {
			return e
		}
		adapter.Checkpoint("linkdelta-after-checkpoint")
		report.Checkpoints = append(report.Checkpoints, cp)
	}
	report.Visibility = runner.Summarize(visibility)
	report = threshold(report)
	return dataset.WriteJSON(filepath.Join(out, "metrics.json"), report)
}

// Each form occupies each position twice in six consecutive query triples.
func timingOrder(sequence int) [3]string {
	return [6][3]string{{"base", "wrapper", "delta"}, {"wrapper", "delta", "base"}, {"delta", "base", "wrapper"}, {"delta", "wrapper", "base"}, {"wrapper", "base", "delta"}, {"base", "delta", "wrapper"}}[sequence%6]
}
func readVerdict(base, wrapper, delta float64) (limit, aaLimit, difference float64, status string) {
	limit = max(1.10*base, base+1e6)
	aaLimit = max(.10*base, 1e6)
	difference = math.Abs(wrapper - base)
	status = "pass"
	if difference > aaLimit {
		status = "unresolved"
	} else if delta > limit {
		status = "miss"
	}
	return
}
func summarizeTimings(cp checkpoint, timings map[string]map[string][]int64) (checkpoint, error) {
	cp.BaseP95 = map[string]float64{}
	cp.WrapperP95 = map[string]float64{}
	cp.DeltaP95 = map[string]float64{}
	cp.Ratio = map[string]float64{}
	cp.AARatio = map[string]float64{}
	cp.ReadLimitNS = map[string]float64{}
	cp.AALimitNS = map[string]float64{}
	cp.AADifferenceNS = map[string]float64{}
	cp.ReadVerdict = map[string]string{}
	cp.WorstRatio = 0
	cp.AAWorstRatio = 0
	if len(timings["base"]) == 0 {
		return cp, fmt.Errorf("missing base timings")
	}
	for op, ns := range timings["base"] {
		a := runner.Summarize(ns).P95
		d := runner.Summarize(timings["delta"][op]).P95
		wrapper := runner.Summarize(timings["wrapper"][op]).P95
		if a <= 0 || d <= 0 || wrapper <= 0 || math.IsNaN(a+d+wrapper) || math.IsInf(a+d+wrapper, 0) {
			return cp, fmt.Errorf("nonpositive or nonfinite timing")
		}
		cp.BaseP95[op] = a
		cp.DeltaP95[op] = d
		cp.WrapperP95[op] = wrapper
		cp.Ratio[op] = d / a
		cp.AARatio[op] = wrapper / a
		cp.WorstRatio = max(cp.WorstRatio, d/a)
		cp.AAWorstRatio = max(cp.AAWorstRatio, wrapper/a)
		cp.ReadLimitNS[op], cp.AALimitNS[op], cp.AADifferenceNS[op], cp.ReadVerdict[op] = readVerdict(a, wrapper, d)
	}
	return cp, nil
}
func checkpointReadStatus(p checkpoint) string {
	if len(p.ReadVerdict) == 0 {
		return "unresolved"
	}
	status := "pass"
	for _, v := range p.ReadVerdict {
		switch v {
		case "pass":
		case "miss":
			status = v
		default:
			return "unresolved"
		}
	}
	return status
}
func threshold(r deltaReport) deltaReport {
	r.Threshold = nil
	r.LowerBound = nil
	for i, p := range r.Checkpoints {
		status := checkpointReadStatus(p)
		if status == "unresolved" {
			r.CompactionBranch = "a-a-unresolved-no-threshold"
			return r
		}
		if status == "miss" {
			r.CompactionBranch = "observed-read-bound-crossing"
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
func selectedCheckpoint(r deltaReport) (checkpoint, bool) {
	chosen := r.Threshold
	if chosen == nil {
		chosen = r.LowerBound
	}
	if chosen == nil {
		return checkpoint{}, false
	}
	for _, p := range r.Checkpoints {
		if p.Size == *chosen {
			return p, true
		}
	}
	return checkpoint{}, false
}
func deltaPass(r deltaReport) bool {
	p, ok := selectedCheckpoint(r)
	return ok && p.Size > 0 && p.Visibility.Count == p.Size && p.MaxEditCPU > 0 && p.MaxEditCPU <= 1e9 && p.Visibility.P95 > 0 && p.Visibility.P95 <= 5e9 && checkpointReadStatus(p) == "pass" && p.ByteRatio > 0 && p.ByteRatio <= 2
}
func deltaTarget(format string, r deltaReport) targetResult {
	status := "pass"
	if !deltaPass(r) {
		status = "miss"
	}
	if r.CompactionBranch == "a-a-unresolved-no-threshold" {
		status = "unresolved"
	}
	return targetResult{Target: "check4-" + format, Status: status, Branch: r.CompactionBranch, Measured: r}
}

// Status records whether the other form is measured or still unknown.
type formResult struct {
	Status string
	Report deltaReport
}

func editCheckpoint(r deltaReport) (checkpoint, bool) {
	if p, ok := selectedCheckpoint(r); ok {
		return p, true
	}
	if len(r.Checkpoints) > 0 {
		// Completed checkpoints follow the strictly increasing configured sizes.
		return r.Checkpoints[0], true
	}
	return checkpoint{}, false
}
func selectDelta(results map[string]formResult) targetResult {
	a, p := results["arrow"], results["parquet"]
	known := func(f formResult) bool { return f.Status == "pass" || f.Status == "miss" }
	ap, pp := a.Status == "pass" && deltaPass(a.Report), p.Status == "pass" && deltaPass(p.Report)
	ac, ao := editCheckpoint(a.Report)
	pc, po := editCheckpoint(p.Report)
	selection, status := "no-passing-form", "miss"
	switch {
	case !known(a) && !known(p):
		selection, status = "no-complete-form-evidence", "blocked"
	case known(a) && known(p) && ao && po && ac.MaxEditCPU > 1e9 && pc.MaxEditCPU > 1e9:
		selection = "stop-ask-user-both-edit-bound-misses"
	case ap && pp:
		selection, status = "both-pass-lead-selects-simplest", "partial"
	case ap && !known(p):
		selection, status = "arrow-passes-parquet-blocked", "partial"
	case pp && !known(a):
		selection, status = "parquet-passes-arrow-blocked", "partial"
	case ap:
		selection, status = "arrow", "pass"
	case pp:
		selection, status = "parquet", "pass"
	case !known(a) || !known(p):
		selection, status = "incomplete-form-evidence", "blocked"
	}
	return targetResult{Target: "check4-delta-selection", Status: status, Branch: selection, Measured: map[string]string{"base": "shared C Parquet plus native Bleve", "read_cost": "format-independent indexed merge; raw A/A controls retained", "edit_batch": "alternating MakeChange and original-file replacement", "scope": "full tier query workload plus delta probes; no daemon routing"}}
}

type verifyReport struct {
	Error                                           string `json:"error,omitempty"`
	Status                                          string
	Checked, NativeTopKComparisons, NativeTopKEqual int
	NativeTopKMatchRate                             float64
	DifferencesSHA256                               string
}

type referenceRecord struct {
	Binding  string
	Query    workload.Query
	Expected workload.Expected
	// Separate from Expected: its omitempty tag would erase a valid empty map.
	MatchRepos map[string]string `json:"match_repositories"`
}

func referenceKey(odd bool, q workload.Query) string {
	return hashJSON(struct {
		Odd   bool
		Query workload.Query
	}{odd, q})
}
func referenceBinding(c config) (string, error) {
	pins := map[string]string{}
	for name, p := range map[string]string{"tier": filepath.Join(c.Data, "manifest.json"), "changed": filepath.Join(c.Out, "changed-source", "manifest.json"), "queries": filepath.Join(c.Out, "queries.json"), "changes": filepath.Join(c.Out, "changes.json")} {
		h, e := dataset.Hash(p)
		if e != nil {
			return "", e
		}
		pins[name] = h
	}
	return hashJSON(pins), nil
}
func storeAnswer(dir string, a workload.Answer) (string, error) {
	raw, err := json.Marshal(a)
	if err != nil {
		return "", err
	}
	pin := fmt.Sprintf("%x", sha256.Sum256(raw))
	p := filepath.Join(dir, pin+".json")
	if _, err = os.Stat(p); err == nil {
		return pin, nil
	} else if !os.IsNotExist(err) {
		return "", err
	}
	f, err := os.OpenFile(p, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		return "", err
	}
	_, err = f.Write(raw)
	return pin, errors.Join(err, f.Close())
}
func loadAnswer(dir, pin string) (workload.Answer, error) {
	var a workload.Answer
	if !digest(pin) {
		return a, fmt.Errorf("invalid answer pin")
	}
	p := filepath.Join(dir, pin+".json")
	actual, err := dataset.Hash(p)
	if err != nil {
		return a, err
	}
	if actual != pin {
		return a, fmt.Errorf("answer blob hash mismatch")
	}
	err = dataset.ReadJSON(p, &a)
	return a, err
}
func importPinnedReferences(ctx context.Context, c config, dir, binding string, qs, original []workload.Query, ds []dataset.Dataset) (err error) {
	f, err := os.Open(filepath.Join(c.Data, "expected.json"))
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, f.Close()) }()
	dec := json.NewDecoder(bufio.NewReaderSize(f, 64<<10))
	t, e := dec.Token()
	if e != nil || t != json.Delim('[') {
		return fmt.Errorf("expected array required")
	}
	i := 0
	for dec.More() {
		var want workload.Expected
		if err = dec.Decode(&want); err != nil {
			return err
		}
		if i >= len(original) || i >= len(qs) {
			return fmt.Errorf("extra pinned reference")
		}
		q := original[i]
		q.ExactRank = true
		if want.QueryID != q.ID || !reflect.DeepEqual(q, qs[i]) {
			return fmt.Errorf("pinned query binding mismatch")
		}
		var matches map[string]string
		if workload.Lexical(q.Op) {
			matches, err = matchRepositories(ctx, ds, q)
			if err != nil {
				return err
			}
		}
		want.MatchRepos = nil
		if err = dataset.WriteJSON(filepath.Join(dir, referenceKey(false, q)+".json"), referenceRecord{Binding: binding, Query: q, Expected: want, MatchRepos: matches}); err != nil {
			return err
		}
		i++
	}
	t, err = dec.Token()
	if err != nil || t != json.Delim(']') || i != len(original) {
		return fmt.Errorf("truncated or missing pinned references")
	}
	var trailing any
	if dec.Decode(&trailing) != io.EOF {
		return fmt.Errorf("trailing reference data")
	}
	return nil
}
func prepareReferences(ctx context.Context, c config) (err error) {
	ds, m, err := loadTier(c.Data)
	if err != nil {
		return err
	}
	if !digest(m.SHA256["queries.json"]) || !digest(m.SHA256["expected.json"]) {
		return fmt.Errorf("pinned queries and expected.json are required")
	}
	var qs, original []workload.Query
	if err = dataset.ReadJSON(filepath.Join(c.Out, "queries.json"), &qs); err != nil {
		return err
	}
	if err = dataset.ReadJSON(filepath.Join(c.Data, "queries.json"), &original); err != nil {
		return err
	}
	binding, err := referenceBinding(c)
	if err != nil {
		return err
	}
	dir := filepath.Join(c.Out, "references")
	if err = os.Mkdir(dir, 0700); err != nil {
		return err
	}
	if err = os.Mkdir(filepath.Join(dir, "validated"), 0700); err != nil {
		return err
	}
	if err = os.Mkdir(filepath.Join(c.Out, "answer-blobs"), 0700); err != nil {
		return err
	}
	// Stream the pinned original references; never retain the answer array.
	if err = importPinnedReferences(ctx, c, dir, binding, qs, original, ds); err != nil {
		return err
	}
	changed, err := dataset.OpenSource(filepath.Join(c.Out, "changed-source"))
	if err != nil {
		return err
	}
	odd := slices.Clone(ds)
	for i, d := range odd {
		if d.Repo == c.DeltaRepo {
			odd[i] = changed
		}
	}
	for _, isOdd := range []bool{false, true} {
		sources := ds
		if isOdd {
			sources = odd
		}
		view, _ := oracle.DiskView(sources)
		for i, q := range qs {
			if !isOdd && i < len(original) {
				continue
			}
			if err = ctx.Err(); err != nil {
				return err
			}
			want, e := workload.Evaluate(ctx, view, adapter.Fanout{Segments: view}, q)
			if e != nil {
				return e
			}
			expected := workload.Expected{QueryID: q.ID, Answer: want}
			var matches map[string]string
			if workload.Lexical(q.Op) {
				matches, e = matchRepositories(ctx, sources, q)
				if e != nil {
					return e
				}
			}
			if err = dataset.WriteJSON(filepath.Join(dir, referenceKey(isOdd, q)+".json"), referenceRecord{Binding: binding, Query: q, Expected: expected, MatchRepos: matches}); err != nil {
				return err
			}
		}
	}
	return nil
}

// Same any-token membership rule as oracle's text index. This streams only one
// query's ID/repository matches, not the graph or every answer into RAM.
func matchRepositories(ctx context.Context, ds []dataset.Dataset, q workload.Query) (map[string]string, error) {
	terms := oracle.Tokens(q.Key)
	matches := map[string]string{}
	add := func(id, body, repo string) {
		tokens := oracle.Tokens(body)
		for _, term := range terms {
			if slices.Contains(tokens, term) {
				matches[id] = repo
				return
			}
		}
	}
	for _, d := range ds {
		if q.Repo != "" && q.Repo != d.Repo {
			continue
		}
		if strings.HasSuffix(q.Op, "content") {
			for v, e := range d.ContentRows() {
				if e != nil {
					return nil, e
				}
				if e = ctx.Err(); e != nil {
					return nil, e
				}
				add(v.ChunkID, v.Body, d.Repo)
			}
		} else {
			for v, e := range d.SymbolRows() {
				if e != nil {
					return nil, e
				}
				if e = ctx.Err(); e != nil {
					return nil, e
				}
				add(v.NodeID, v.Tokens, d.Repo)
			}
		}
	}
	return matches, nil
}
func compareReference(q workload.Query, want workload.Expected, matches map[string]string, got workload.Answer) error {
	if workload.Lexical(q.Op) {
		if matches == nil {
			return fmt.Errorf("lexical reference %s lacks prepared match sets", q.ID)
		}
		q.NativeScoring = true
		q.MatchRepos = matches
		q.ExactRank = q.K < 0 || q.K >= len(matches) || strings.HasPrefix(q.ID, "delta-marker-")
		q.MinOverlap = 0
	} else {
		q.CutoffTies = want.Ties
	}
	c := workload.Compare(q, want.Answer, got)
	if c.Mismatch {
		return fmt.Errorf("%w: query %s: %s", errAnswerMismatch, q.ID, c.Detail)
	}
	if workload.Lexical(q.Op) {
		// Native ranking may differ; an underfilled result is still incorrect.
		count := len(want.Answer.Hits)
		if q.K >= 0 {
			count = min(q.K, count)
		}
		if len(got.Hits) != count {
			return fmt.Errorf("%w: query %s: native hit count %d, expected %d", errAnswerMismatch, q.ID, len(got.Hits), count)
		}
	}
	return nil
}
func hitIDs(hits []adapter.Hit) []string {
	out := make([]string, len(hits))
	for i, h := range hits {
		out[i] = h.ID
	}
	return out
}
func verifyDeltas(ctx context.Context, c config, format string) (err error) {
	report := verifyReport{Status: "blocked"}
	defer func() {
		if err != nil {
			report.Status = "blocked"
			if errors.Is(err, errAnswerMismatch) {
				report.Status = "miss"
			}
			report.Error = err.Error()
		}
		err = errors.Join(err, dataset.WriteJSON(filepath.Join(c.Out, "verify-"+format+".json"), report))
	}()
	binding, err := referenceBinding(c)
	if err != nil {
		return err
	}
	var qs []workload.Query
	if err = dataset.ReadJSON(filepath.Join(c.Out, "queries.json"), &qs); err != nil {
		return err
	}
	queries := map[string]workload.Query{}
	for _, q := range qs {
		if _, ok := queries[q.ID]; ok {
			return fmt.Errorf("duplicate query ID")
		}
		queries[q.ID] = q
	}
	scratch, err := os.MkdirTemp(c.Out, "verify-native-"+format+"-")
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, os.RemoveAll(scratch)) }()
	diffPath := filepath.Join(c.Out, "native-rank-differences-"+format+".jsonl")
	diff, err := os.Create(diffPath)
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, diff.Close()) }()
	enc := json.NewEncoder(diff)
	referencePins := map[string]string{}
	seen := map[string]bool{}
	checked := map[string]bool{}
	refs := filepath.Join(c.Out, "references")
	blobs := filepath.Join(c.Out, "answer-blobs")
	err = eachJSON(filepath.Join(c.Out, "delta-"+format, "answers.jsonl"), func(s sample) error {
		if e := ctx.Err(); e != nil {
			return e
		}
		q, ok := queries[s.Query.ID]
		if !ok || !reflect.DeepEqual(q, s.Query) || !slices.Contains(c.Sizes, s.Size) || s.Repeat < -1 || s.Repeat >= c.Repeats || !slices.Contains([]string{"base", "wrapper", "delta"}, s.Form) || s.ElapsedNS <= 0 {
			return fmt.Errorf("invalid observation binding")
		}
		observation := hashJSON(struct {
			Size, Repeat int
			Form, Query  string
		}{s.Size, s.Repeat, s.Form, q.ID})
		if seen[observation] {
			return fmt.Errorf("duplicate query observation")
		}
		seen[observation] = true
		key := referenceKey(s.Size%2 == 1, q)
		var ref referenceRecord
		if referencePins[key] == "" {
			if e := dataset.ReadJSON(filepath.Join(refs, key+".json"), &ref); e != nil {
				return e
			}
			if ref.Binding != binding || !reflect.DeepEqual(ref.Query, q) || ref.Expected.QueryID != q.ID {
				return fmt.Errorf("reference source/query binding mismatch")
			}
			if workload.Lexical(q.Op) && ref.MatchRepos == nil {
				return fmt.Errorf("lexical reference %s lacks prepared match sets", q.ID)
			}
			referencePins[key] = hashJSON(ref)
		}
		memo := hashJSON(struct{ Reference, Answer string }{referencePins[key], s.AnswerSHA256})
		marker := filepath.Join(refs, "validated", memo+".json")
		cleanPath := filepath.Join(refs, "clean-"+key+".json")
		var cleanPin string
		cleanErr := dataset.ReadJSON(cleanPath, &cleanPin)
		if s.Form != "base" && cleanErr != nil {
			return fmt.Errorf("missing accepted clean baseline: %w", cleanErr)
		}
		if s.Form == "wrapper" && cleanPin != s.AnswerSHA256 {
			// A missing/corrupt blob is incomplete evidence, not answer inequality.
			if _, e := loadAnswer(blobs, cleanPin); e != nil {
				return e
			}
			if _, e := loadAnswer(blobs, s.AnswerSHA256); e != nil {
				return e
			}
			return fmt.Errorf("%w: zero-layer wrapper differs from raw C", errAnswerMismatch)
		}
		_, e := os.Stat(marker)
		if e != nil && !os.IsNotExist(e) {
			return e
		}
		if !checked[memo] && os.IsNotExist(e) {
			if ref.Expected.QueryID == "" {
				if e = dataset.ReadJSON(filepath.Join(refs, key+".json"), &ref); e != nil {
					return e
				}
			}
			a, e := loadAnswer(blobs, s.AnswerSHA256)
			if e != nil {
				return e
			}
			if s.Form == "base" {
				if e = compareReference(q, ref.Expected, ref.MatchRepos, a); e != nil {
					return e
				}
			} else {
				clean, e := loadAnswer(blobs, cleanPin)
				if e != nil {
					return e
				}
				expected := ref.Expected
				expected.Answer = clean
				if e = compareReference(q, expected, ref.MatchRepos, a); e != nil {
					return e
				}
			}
			if e = dataset.WriteJSON(marker, struct{ Reference, Answer string }{referencePins[key], s.AnswerSHA256}); e != nil {
				return e
			}
		}
		checked[memo] = true
		if s.Form == "base" {
			if cleanErr != nil && !os.IsNotExist(cleanErr) {
				return cleanErr
			}
			if os.IsNotExist(cleanErr) {
				if e = dataset.WriteJSON(cleanPath, s.AnswerSHA256); e != nil {
					return e
				}
			}
		}
		report.Checked++
		if !workload.Lexical(q.Op) || s.Repeat < 0 || s.Form == "wrapper" {
			return nil
		}
		pairKey := hashJSON(struct {
			Size, Repeat int
			ID           string
		}{s.Size, s.Repeat, q.ID})
		other := "base"
		if s.Form == "base" {
			other = "delta"
		}
		pairPath := filepath.Join(scratch, pairKey+"-"+other+".json")
		var paired sample
		e = dataset.ReadJSON(pairPath, &paired)
		if os.IsNotExist(e) {
			return dataset.WriteJSON(filepath.Join(scratch, pairKey+"-"+s.Form+".json"), s)
		}
		if e != nil {
			return e
		}
		a, b := paired, s
		if a.Form != "base" {
			a, b = b, a
		}
		clean, e := loadAnswer(blobs, a.AnswerSHA256)
		if e != nil {
			return e
		}
		delta, e := loadAnswer(blobs, b.AnswerSHA256)
		if e != nil {
			return e
		}
		report.NativeTopKComparisons++
		if slices.Equal(hitIDs(clean.Hits), hitIDs(delta.Hits)) {
			report.NativeTopKEqual++
		} else {
			if e = enc.Encode(struct {
				Size, Repeat int
				Query        workload.Query
				Clean, Delta []adapter.Hit
			}{s.Size, s.Repeat, q, clean.Hits, delta.Hits}); e != nil {
				return e
			}
		}
		return os.Remove(pairPath)
	})
	if err != nil {
		return err
	}
	if report.Checked != len(qs)*len(c.Sizes)*(c.Repeats+1)*3 {
		return fmt.Errorf("incomplete answer/timing spool")
	}
	entries, err := os.ReadDir(scratch)
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
	report.Status = "pass"
	return nil
}
