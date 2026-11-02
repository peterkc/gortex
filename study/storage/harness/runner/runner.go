// Package runner is linked into each adapter's separate command binary.
package runner

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"gxstudy/adapter"
	"gxstudy/dataset"
	"gxstudy/oracle"
	"gxstudy/workload"
	"io"
	"math"
	"math/rand/v2"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"runtime"
	"runtime/debug"
	"slices"
	"strings"
	"sync"
	"syscall"
	"time"
)

type Distribution struct {
	P50   float64 `json:"p50_ns"`
	P95   float64 `json:"p95_ns"`
	P99   float64 `json:"p99_ns"`
	Mean  float64 `json:"mean_ns"`
	Count int     `json:"count"`
}

func Summarize(ns []int64) Distribution {
	if len(ns) == 0 {
		return Distribution{}
	}
	ns = slices.Clone(ns)
	slices.Sort(ns)
	sum := 0.
	for _, n := range ns {
		sum += float64(n)
	}
	pct := func(p float64) float64 { return float64(ns[max(0, int(math.Ceil(float64(len(ns))*p))-1)]) }
	return Distribution{P50: pct(.5), P95: pct(.95), P99: pct(.99), Mean: sum / float64(len(ns)), Count: len(ns)}
}

type BuildRecord struct {
	CPU           int64              `json:"cpu_ns,omitempty"`
	ArtifactBytes int64              `json:"artifact_bytes"`
	Repo          string             `json:"repo"`
	DurationNS    int64              `json:"duration_ns"`
	Stats         adapter.BuildStats `json:"stats"`
	Ref           adapter.SegmentRef `json:"ref"`
}
type ConcurrentResult struct {
	CheckedSamples int               `json:"checked_samples"`
	SampleSHA256   map[string]string `json:"sample_sha256,omitempty"`

	OpenNS              int64          `json:"open_ns"`
	PrecommitNS         int64          `json:"precommit_ns"`
	NewSamplesPerReader []int64        `json:"new_samples_per_reader"`
	CommitRaceWindows   map[string]int `json:"commit_race_windows"`
	CommitRaceMinimum   int            `json:"commit_race_minimum"`
	PinOpenAfterPublish Distribution   `json:"pin_open_after_publish"`

	DuringBuild          Distribution `json:"during_build"`
	PinOpenDuringPublish Distribution `json:"pin_open_during_publish"`
	PublishNS            int64        `json:"publish_ns"`
	ReaderPasses         int          `json:"reader_passes"`

	Idle           Distribution     `json:"idle"`
	DuringPublish  Distribution     `json:"during_publish"`
	AfterPublish   Distribution     `json:"after_publish"`
	BuildPublishNS int64            `json:"build_publish_ns"`
	Checks         []workload.Check `json:"checks"`
}
type OverlapSummary struct {
	Min   float64 `json:"min"`
	Mean  float64 `json:"mean"`
	P50   float64 `json:"p50"`
	Count int     `json:"count"`
}
type Result struct {
	LocalQuality  map[string]OverlapSummary `json:"local_quality"`
	Overlap       map[string]OverlapSummary `json:"overlap"`
	GlobalQuality map[string]OverlapSummary `json:"global_quality"`

	PID         int                              `json:"pid"`
	ServerUsage map[string]adapter.ResourceUsage `json:"server_usage,omitempty"`

	Adapter        string                  `json:"adapter"`
	Tier           string                  `json:"tier"`
	Run            int                     `json:"run"`
	Started        string                  `json:"started"`
	Uptime         string                  `json:"uptime"`
	GoVersion      string                  `json:"go_version"`
	CPUs           int                     `json:"cpus"`
	ManifestSHA256 string                  `json:"manifest_sha256"`
	Capabilities   adapter.Capabilities    `json:"capabilities"`
	Builds         []BuildRecord           `json:"builds"`
	Metrics        map[string]Distribution `json:"metrics"`
	Checks         []workload.Check        `json:"checks"`
	Delta          BuildRecord             `json:"delta"`
	DeltaChecks    []workload.Check        `json:"delta_checks"`
	Concurrent     ConcurrentResult        `json:"concurrent"`
	Crash          []CrashTrial            `json:"crash"`
	PeakRSSBytes   int64                   `json:"peak_rss_bytes"`
	Failures       []string                `json:"failures"`
	Success        bool                    `json:"success"`
	TimingOnly     bool                    `json:"timing_only,omitempty"`
	QueryLatencies map[string]string       `json:"query_latency_sha256,omitempty"`
}

// MarshalJSON leaves the full protocol unchanged and omits unrun timing phases.
func (r Result) MarshalJSON() ([]byte, error) {
	type result Result
	if !r.TimingOnly {
		return json.Marshal(result(r))
	}
	return json.Marshal(struct {
		result
		Delta         *BuildRecord      `json:"delta,omitempty"`
		DeltaChecks   []workload.Check  `json:"delta_checks,omitempty"`
		Concurrent    *ConcurrentResult `json:"concurrent,omitempty"`
		Crash         []CrashTrial      `json:"crash,omitempty"`
		SkippedPhases []string          `json:"skipped_phases"`
	}{result: result(r), SkippedPhases: []string{"delta", "concurrent_readers", "commit_race", "crash_trials"}})
}

// ReferenceCatalog supplies the same atomic JSON reference catalog for all
// non-catalog candidates. Validation uses that candidate's own Open contract.
type referenceCatalog struct {
	adapter.Catalog
	b        adapter.Backend
	mu       sync.Mutex
	verified map[adapter.SegmentRef]bool
}

func (c *referenceCatalog) VerifyPublication(ctx context.Context, ref adapter.SegmentRef) error {
	c.mu.Lock()
	ok := c.verified[ref]
	c.mu.Unlock()
	if ok {
		return nil
	}
	s, e := c.b.Open(ctx, ref.Path, ref)
	if e != nil {
		return e
	}
	if e = s.Close(); e != nil {
		return e
	}
	c.mu.Lock()
	c.verified[ref] = true
	c.mu.Unlock()
	return nil
}
func ReferenceCatalog(b adapter.Backend) adapter.CatalogFactory {
	return func(dir string) (adapter.Catalog, error) {
		c := &referenceCatalog{b: b, verified: map[adapter.SegmentRef]bool{}}
		base, e := oracle.NewCatalogWithVerifier(dir, func(ref adapter.SegmentRef) error { return c.VerifyPublication(context.Background(), ref) })
		if e != nil {
			return nil, e
		}
		c.Catalog = base
		return c, nil
	}
}

const defaultSafetyTimeout = 30 * time.Second

type runRequest struct {
	Adapter             string
	Data                string
	Out                 string
	Run                 int
	Trials              int
	SafetyTimeout       time.Duration
	TimingOnly          bool
	QueryMinimumSamples int
	CatalogArgs         []string
	CatalogResult       string
	Binding             RunBinding
	RunnerScope         string
	CheckerScope        string
	EngineScope         string
	checker             *processClient
	engine              *processClient
}

// trackedBackend owns all refs produced by this run, including crash fixtures.
type trackedBackend struct {
	adapter.Backend
	refs []adapter.SegmentRef
}

func (b *trackedBackend) Build(ctx context.Context, d dataset.Dataset, dir string) (adapter.SegmentRef, adapter.BuildStats, error) {
	r, s, e := b.Backend.Build(ctx, d, dir)
	if e == nil {
		b.refs = append(b.refs, r)
	}
	return r, s, e
}
func (b *trackedBackend) BuildDelta(ctx context.Context, base adapter.SegmentRef, c adapter.FileChange, dir string) (adapter.SegmentRef, adapter.BuildStats, error) {
	r, s, e := b.Backend.BuildDelta(ctx, base, c, dir)
	if e == nil {
		b.refs = append(b.refs, r)
	}
	return r, s, e
}
func preparePhase() { runtime.GC(); debug.FreeOSMemory() }
func resource(ctx context.Context, b adapter.Backend, r *Result, phase string) error {
	if h, ok := b.(adapter.ServerHooks); ok {
		u, e := h.ResourceUsage(ctx)
		if e != nil {
			return e
		}
		r.ServerUsage[phase] = u
	}
	return nil
}
func Main(b adapter.Backend, factory adapter.CatalogFactory) {
	var err error
	if len(os.Args) > 1 {
		switch os.Args[1] {
		case "catalog-bench":
			err = RunLimitedCatalog(b, factory, os.Args[2:])
		case "__engine", "__checker":
			err = processServe(b, factory, os.Args[1] == "__checker")
		case "__catalog_bench_run":
			if len(os.Args) != 3 {
				err = fmt.Errorf("catalog benchmark request required")
			} else {
				err = catalogRunChild(b, factory, os.Args[2])
			}
		case "__catalog_bench_read":
			if len(os.Args) != 3 {
				err = fmt.Errorf("catalog recovery child requires request")
			} else {
				err = catalogReadChild(b.Name(), factory, os.Args[2])
			}
		case "__catalog_publish":
			err = publishChild(factory, os.Args[2:])
		case "__measure_run":
			if len(os.Args) != 3 {
				err = fmt.Errorf("run child requires request")
			} else {
				var req runRequest
				err = dataset.ReadJSON(os.Args[2], &req)
				if err == nil {
					err = isolatedRunChild(b, factory, req)
				}
			}
		default:
			err = RunCLI(b, factory)
		}
	} else {
		err = RunCLI(b, factory)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
func RunCLI(b adapter.Backend, factory adapter.CatalogFactory) error {
	fs := flag.NewFlagSet(b.Name(), flag.ContinueOnError)
	data := fs.String("data", "", "tier directory")
	out := fs.String("out", "", "fresh result directory")
	runs := fs.Int("runs", 5, "measured fresh-process runs")
	trials := fs.Int("crash-trials", 12, "hook trials plus this many external-time trials")
	warm := fs.Bool("warmup", true, "discard one full fresh-process warm-up")
	timingOnly := fs.Bool("timing-only", false, "time the base workload only; skip update and publication correctness phases")
	minimumSamples := fs.Int("min-query-samples", 1, "minimum per judged stratum per timing process; repetitions planned from discarded warmup")
	safetyTimeout := fs.Duration("safety-timeout", defaultSafetyTimeout, "safety deadline for each commit-race check and crash child")
	bindingPath := fs.String("binding", "", "required source/harness/data/recipe/host/command/limit receipt")
	cgroupRoot := fs.String("cgroup-root", "", "required delegated Linux cgroup-v2 subtree")
	if err := fs.Parse(os.Args[1:]); err != nil {
		return err
	}
	if *data == "" || *out == "" || *runs < 1 || *trials < 1 {
		return fmt.Errorf("data, out and positive runs/trials required")
	}
	if *minimumSamples < 1 {
		return fmt.Errorf("min-query-samples must be positive")
	}
	if *safetyTimeout <= 0 {
		return fmt.Errorf("safety-timeout must be positive")
	}
	var err error
	*data, err = filepath.Abs(*data)
	if err != nil {
		return err
	}
	*out, err = filepath.Abs(*out)
	if err != nil {
		return err
	}
	if factory == nil {
		return fmt.Errorf("catalog factory required")
	}
	if *bindingPath == "" || *cgroupRoot == "" {
		return fmt.Errorf("binding and cgroup-root required; no unbounded measurement mode")
	}
	binding, err := readBinding(*bindingPath, *data, os.Args[1:])
	if err != nil {
		return err
	}
	if _, err = os.Stat(*out); err == nil {
		return fmt.Errorf("output directory exists")
	}
	if err = os.MkdirAll(*out, 0755); err != nil {
		return err
	}
	exe, err := os.Executable()
	if err != nil {
		return err
	}
	first := 1
	if *warm {
		first = 0
	}
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	for run := first; run <= *runs; run++ {
		req := runRequest{Adapter: b.Name(), Data: *data, Out: *out, Run: run, Trials: *trials, SafetyTimeout: *safetyTimeout, TimingOnly: *timingOnly, QueryMinimumSamples: *minimumSamples, Binding: binding}
		if err = controlledRun(ctx, exe, "__measure_run", *cgroupRoot, req); err != nil {
			return err
		}
	}
	return nil
}
func loadTier(dir string) ([]dataset.Dataset, []workload.Query, []workload.Expected, string, error) {
	var meta dataset.Manifest
	manifestPath, pathErr := dataset.SafeFile(dir, "manifest.json")
	if pathErr != nil {
		return nil, nil, nil, "", pathErr
	}
	if e := dataset.ReadJSON(manifestPath, &meta); e != nil {
		return nil, nil, nil, "", e
	}
	if meta.Version != dataset.GeneratorVersion {
		return nil, nil, nil, "", fmt.Errorf("unsupported tier version")
	}
	seenRepos := map[string]bool{}
	for _, id := range meta.Repos {
		if id == "" || id == "." || filepath.Base(id) != id || seenRepos[id] {
			return nil, nil, nil, "", fmt.Errorf("invalid or duplicate repository %q", id)
		}
		seenRepos[id] = true
		for _, name := range append(slices.Clone(dataset.RecordFiles), "manifest.json") {
			if meta.SHA256[filepath.Join(id, name)] == "" {
				return nil, nil, nil, "", fmt.Errorf("unbound tier file")
			}
		}
	}
	if meta.SHA256["queries.json"] == "" || meta.SHA256["expected.json"] == "" {
		return nil, nil, nil, "", fmt.Errorf("unbound workload")
	}
	for p, want := range meta.SHA256 {
		path, e := dataset.SafeFile(dir, p)
		if e != nil {
			return nil, nil, nil, "", e
		}
		got, e := dataset.Hash(path)
		if e != nil {
			return nil, nil, nil, "", e
		}
		if got != want {
			return nil, nil, nil, "", fmt.Errorf("tier checksum mismatch %s", p)
		}
	}
	var ds []dataset.Dataset
	for _, id := range meta.Repos {
		d, e := dataset.OpenSource(filepath.Join(dir, id))
		if e != nil {
			return nil, nil, nil, "", e
		}
		if d.Repo != id {
			return nil, nil, nil, "", fmt.Errorf("repository identity mismatch")
		}
		ds = append(ds, d)
	}
	if e := dataset.ValidateTierIDs(ds); e != nil {
		return nil, nil, nil, "", e
	}
	var qs []workload.Query
	var want []workload.Expected
	if e := dataset.ReadJSON(filepath.Join(dir, "queries.json"), &qs); e != nil {
		return nil, nil, nil, "", e
	}
	file, e := os.Open(filepath.Join(dir, "expected.json"))
	if e != nil {
		return nil, nil, nil, "", e
	}
	decoder := json.NewDecoder(file)
	if token, e := decoder.Token(); e != nil || token != json.Delim('[') {
		file.Close()
		return nil, nil, nil, "", fmt.Errorf("expected array required")
	}
	for decoder.More() {
		var row workload.Expected
		if e := decoder.Decode(&row); e != nil {
			file.Close()
			return nil, nil, nil, "", e
		}
		want = append(want, workload.Expected{QueryID: row.QueryID, Sources: ds})
	}
	if token, e := decoder.Token(); e != nil || token != json.Delim(']') {
		file.Close()
		return nil, nil, nil, "", fmt.Errorf("truncated expected array")
	}
	var trailing any
	if e := decoder.Decode(&trailing); e != io.EOF {
		file.Close()
		return nil, nil, nil, "", fmt.Errorf("trailing expected data")
	}
	if e := file.Close(); e != nil {
		return nil, nil, nil, "", e
	}
	if len(ds) == 0 || len(qs) == 0 || len(qs) != len(want) {
		return nil, nil, nil, "", fmt.Errorf("empty tier or mismatched workload")
	}
	counts := dataset.Counts{}
	for _, d := range ds {
		counts = dataset.Sum(counts, d.Meta.Counts)
	}
	if counts != meta.Counts {
		return nil, nil, nil, "", fmt.Errorf("tier counts differ from repositories")
	}
	seenQueries := map[string]bool{}
	for i, q := range qs {
		op := strings.TrimPrefix(q.Op, "native_")
		if !slices.Contains([]string{"id", "name", "file", "in", "out", "traverse2", "traverse3", "symbols", "content", "vector", "multi_name", "multi_in", "multi_symbols", "multi_vector", "multi_traverse2", "multi_traverse3"}, op) {
			return nil, nil, nil, "", fmt.Errorf("unknown query operation %q", q.Op)
		}
		if strings.HasSuffix(op, "vector") {
			if len(q.Vec) != dataset.Dimensions {
				return nil, nil, nil, "", fmt.Errorf("query vector dimensions")
			}
			norm := 0.
			for _, value := range q.Vec {
				if math.IsNaN(float64(value)) || math.IsInf(float64(value), 0) {
					return nil, nil, nil, "", fmt.Errorf("nonfinite query vector")
				}
				norm += float64(value) * float64(value)
			}
			if norm == 0 {
				return nil, nil, nil, "", fmt.Errorf("zero query vector")
			}
		}
		if strings.Contains(op, "traverse") && q.Depth != 2 && q.Depth != 3 {
			return nil, nil, nil, "", fmt.Errorf("unsupported traversal depth")
		}
		if q.ID == "" || seenQueries[q.ID] {
			return nil, nil, nil, "", fmt.Errorf("duplicate or empty query id")
		}
		seenQueries[q.ID] = true
		if !strings.HasPrefix(op, "multi_") && !seenRepos[q.Repo] {
			return nil, nil, nil, "", fmt.Errorf("unknown query repository")
		}
		if q.ID != want[i].QueryID {
			return nil, nil, nil, "", fmt.Errorf("query IDs differ")
		}
	}
	digest, e := dataset.Hash(filepath.Join(dir, "manifest.json"))
	return ds, qs, want, digest, e
}
func runChild(original adapter.Backend, factory adapter.CatalogFactory, req runRequest) error {
	if req.QueryMinimumSamples < 0 {
		return fmt.Errorf("min-query-samples must be positive")
	}
	if req.QueryMinimumSamples == 0 {
		req.QueryMinimumSamples = 1
	}

	if req.SafetyTimeout < 0 {
		return fmt.Errorf("safety-timeout must be positive")
	}
	if req.SafetyTimeout == 0 {
		req.SafetyTimeout = defaultSafetyTimeout
	}
	ctx := context.WithValue(context.Background(), queryMinimumKey{}, req.QueryMinimumSamples)
	if req.checker != nil {
		ctx = context.WithValue(ctx, checkerContextKey{}, req.checker)
		ctx = context.WithValue(ctx, sampleDirectoryKey{}, filepath.Join(req.Out, fmt.Sprintf("run-%02d", req.Run)))
	}
	if req.EngineScope != "" {
		ctx = context.WithValue(ctx, engineScopeKey{}, req.EngineScope)
	}
	load, e := exec.Command("uptime").CombinedOutput()
	if e != nil {
		return e
	}
	ds, qs, want, digest, e := loadTier(req.Data)
	if e != nil {
		return e
	}
	dir := filepath.Join(req.Out, fmt.Sprintf("run-%02d", req.Run))
	if e = os.MkdirAll(dir, 0755); e != nil {
		return e
	}
	r := Result{TimingOnly: req.TimingOnly, PID: os.Getpid(), Adapter: original.Name(), Tier: filepath.Base(req.Data), Run: req.Run, Started: time.Now().Format(time.RFC3339), Uptime: strings.TrimSpace(string(load)), GoVersion: runtime.Version(), CPUs: runtime.NumCPU(), ManifestSHA256: digest, Capabilities: original.Capabilities(), Metrics: map[string]Distribution{}, ServerUsage: map[string]adapter.ResourceUsage{}, Failures: []string{}}
	b := &trackedBackend{Backend: original}
	hooks, server := original.(adapter.ServerHooks)
	if server {
		if e = hooks.Reset(ctx); e != nil {
			return e
		}
	}
	e = runOnce(ctx, b, factory, ds, qs, want, dir, req.Trials, req.SafetyTimeout, &r)
	if e != nil {
		r.Failures = append(r.Failures, e.Error())
	}
	if server {
		if e = resource(ctx, original, &r, "before_release"); e != nil {
			r.Failures = append(r.Failures, e.Error())
		}
		for _, ref := range b.refs {
			if e = hooks.Release(ctx, ref); e != nil {
				r.Failures = append(r.Failures, e.Error())
			}
		}
		if e = hooks.Reset(ctx); e != nil {
			r.Failures = append(r.Failures, e.Error())
		}
		if e = resource(ctx, original, &r, "after_reset"); e != nil {
			r.Failures = append(r.Failures, e.Error())
		}
	}
	var usage syscall.Rusage
	if e = syscall.Getrusage(syscall.RUSAGE_SELF, &usage); e != nil {
		return e
	}
	r.PeakRSSBytes = usage.Maxrss
	if runtime.GOOS == "linux" {
		r.PeakRSSBytes *= 1024
	}
	var finalMeta dataset.Manifest
	if e = dataset.ReadJSON(filepath.Join(req.Data, "manifest.json"), &finalMeta); e != nil {
		r.Failures = append(r.Failures, e.Error())
	} else {
		for name, hash := range finalMeta.SHA256 {
			path, pathErr := dataset.SafeFile(req.Data, name)
			if pathErr != nil {
				r.Failures = append(r.Failures, pathErr.Error())
				continue
			}
			got, hashErr := dataset.Hash(path)
			if hashErr != nil {
				r.Failures = append(r.Failures, hashErr.Error())
			} else if got != hash {
				r.Failures = append(r.Failures, "input changed during run: "+name)
			}
		}
	}
	finalDigest, digestErr := dataset.Hash(filepath.Join(req.Data, "manifest.json"))
	if digestErr != nil {
		r.Failures = append(r.Failures, digestErr.Error())
	} else if finalDigest != digest {
		r.Failures = append(r.Failures, "tier manifest changed during run")
	}
	summarizeOverlap(&r)
	collectFailures(&r)
	r.Success = len(r.Failures) == 0
	if req.Run > 0 {
		resultPath := filepath.Join(req.Out, fmt.Sprintf("%s-%s-%02d.json", original.Name(), r.Tier, req.Run))
		if req.engine != nil {
			resultPath += ".partial"
		}
		if e = dataset.WriteJSON(resultPath, r); e != nil {
			return e
		}
	} else {
		status := map[string]any{"pid": r.PID, "success": r.Success, "failures": r.Failures}
		if r.TimingOnly {
			status["timing_only"] = true
		}
		statusPath := filepath.Join(dir, "warmup-status.json")
		if req.engine != nil {
			statusPath += ".partial"
		}
		if e = dataset.WriteJSON(statusPath, status); e != nil {
			return e
		}
	}
	fmt.Printf("%s %s fresh run %d pid=%d success=%v failures=%d\n", r.Adapter, r.Tier, req.Run, r.PID, r.Success, len(r.Failures))
	if !r.Success {
		return fmt.Errorf("run failed: %v", r.Failures)
	}
	return nil
}
func collectFailures(r *Result) {
	for _, checks := range [][]workload.Check{r.Checks, r.DeltaChecks, r.Concurrent.Checks} {
		for _, c := range checks {
			if c.Mismatch {
				r.Failures = append(r.Failures, c.QueryID+": "+c.Detail)
			}
		}
	}
	for _, t := range r.Crash {
		if !t.OK {
			r.Failures = append(r.Failures, "crash: "+t.Error)
		}
	}
}
func openAll(ctx context.Context, b adapter.Backend, refs map[string]adapter.SegmentRef) (map[string]adapter.Segment, error) {
	s := map[string]adapter.Segment{}
	ids := make([]string, 0, len(refs))
	for id := range refs {
		ids = append(ids, id)
	}
	slices.Sort(ids)
	for _, id := range ids {
		v, e := b.Open(ctx, refs[id].Path, refs[id])
		if e != nil {
			return nil, errors.Join(e, closeAll(s))
		}
		s[id] = v
	}
	return s, nil
}
func closeAll(s map[string]adapter.Segment) error {
	var errs []error
	for _, v := range s {
		errs = append(errs, v.Close())
	}
	return errors.Join(errs...)
}
func unsupported(c adapter.Capabilities, q workload.Query) string {
	if reason := c.Unsupported[q.Op]; reason != "" {
		return reason
	}
	if strings.HasPrefix(q.Op, "native_") {
		return ""
	} // native operations own their declarations
	base := strings.TrimPrefix(q.Op, "multi_")
	if reason := c.Unsupported[base]; reason != "" {
		return reason
	}
	if strings.Contains(base, "traverse") && c.Unsupported["in"] != "" {
		return "emulated traversal requires in: " + c.Unsupported["in"]
	}
	return ""
}
func withPolicy(q workload.Query, c adapter.Capabilities) workload.Query {
	p, declared := c.Ranking[q.Op]
	// Default fan-out uses the same scorer as its per-repository input. Native
	// multi APIs still declare their own independent operation policy.
	if !declared && q.Op == "multi_symbols" && c.Ranking["symbols"].Mode == "native" {
		p.Mode = "native"
	}
	q.NativeScoring = p.Mode == "native"
	if q.NativeScoring {
		return q
	} // retain deterministic query requirements
	if workload.Ranked(q.Op) {
		q.MinOverlap = max(q.MinOverlap, workload.MinimumRankedOverlap)
	}
	if q.Op == "multi_symbols" {
		q.ExactRank = true
	}
	q.ExactRank = q.ExactRank || p.Exact
	q.MinOverlap = max(q.MinOverlap, p.MinOverlap)
	return q
}
func compareExpected(q workload.Query, w workload.Expected, got workload.Answer) workload.Check {
	q.MatchRepos = w.MatchRepos
	q.CutoffTies = w.Ties
	c := workload.Compare(q, w.Answer, got)
	if workload.Ranked(q.Op) {
		local := w.Answer
		if w.Local != nil {
			local = *w.Local
		}
		localValue := workload.Overlap(local.Hits, got.Hits)
		c.LocalOverlap = &localValue
		global := w.Answer
		if w.Global != nil {
			global = *w.Global
		}
		value := workload.Overlap(global.Hits, got.Hits)
		c.GlobalOverlap = &value
	}
	return c
}
func globalExpected(w workload.Expected) workload.Expected {
	if w.Global != nil {
		if w.Local == nil {
			local := w.Answer
			w.Local = &local
		}
		w.Answer = *w.Global
		w.Ties = w.GlobalTies
		w.Global = nil
	}
	return w
}
func measuredQueries(ctx context.Context, s map[string]adapter.Segment, m adapter.MultiRepo, qs []workload.Query, want []workload.Expected, caps adapter.Capabilities) (map[string][]int64, []workload.Check, error) {
	return queryRun(ctx, s, m, qs, want, caps, 0)
}

// queryRun spools bounded answers during sampling and validates only after all
// native operations, including discarded warm-up operations, have finished.
func queryRun(ctx context.Context, s map[string]adapter.Segment, m adapter.MultiRepo, qs []workload.Query, want []workload.Expected, caps adapter.Capabilities, warmup int) (samples map[string][]int64, checks []workload.Check, err error) {
	spool, err := os.CreateTemp("", "gxstudy-query-checks-*")
	if err != nil {
		return nil, nil, err
	}
	defer func() { err = errors.Join(err, spool.Close(), os.Remove(spool.Name())) }()
	samples = map[string][]int64{}
	var warmFailures []workload.Check
	addCheck := func(check workload.Check, isWarm bool) {
		if isWarm {
			if check.Mismatch {
				check.QueryID = "warmup_" + check.QueryID
				warmFailures = append(warmFailures, check)
			}
		} else {
			checks = append(checks, check)
		}
	}
	var latencies *os.File
	if output, ok := ctx.Value(queryLatencyKey{}).(queryLatencyOutput); ok {
		latencies, err = os.CreateTemp(output.dir, "query-latencies-*.jsonl")
		if err != nil {
			return nil, nil, err
		}
		defer func() {
			err = errors.Join(err, latencies.Close())
			hash, hashErr := dataset.Hash(latencies.Name())
			err = errors.Join(err, hashErr)
			if hashErr == nil {
				if output.result.QueryLatencies == nil {
					output.result.QueryLatencies = map[string]string{}
				}
				output.result.QueryLatencies[filepath.Base(latencies.Name())] = hash
			}
		}()
	}
	warmCounts := map[string]int{}
	encoder := json.NewEncoder(spool)
	for i, q := range qs {
		q = withPolicy(q, caps)
		isWarm := i < warmup
		if why := unsupported(caps, q); why != "" {
			check := workload.Check{QueryID: q.ID, Operation: q.Op, Status: "unsupported", Detail: why}
			if e := encoder.Encode(pendingCheck{Ready: &check, Warmup: isWarm}); e != nil {
				return samples, checks, e
			}
			continue
		}
		repeats := 1
		for attempt := 0; attempt < repeats; attempt++ {
			got, inputs, _, _, ns, e := evaluateTimed(ctx, s, m, q)
			if e != nil {
				check := workload.Check{QueryID: q.ID, Operation: q.Op, Status: "failed", Mismatch: true, Detail: e.Error(), FailureKind: "operation_error"}
				if errors.Is(e, adapter.ErrUnsupported) && strings.HasPrefix(q.Op, "native_") {
					check.Status = "unsupported"
					check.Mismatch = false
				}
				if e := encoder.Encode(pendingCheck{Ready: &check, Warmup: isWarm}); e != nil {
					return samples, checks, e
				}
				break
			}
			if attempt == 0 {
				stratum := sampleStratum(q, got)
				if isWarm {
					warmCounts[stratum]++
				} else if minimum, ok := ctx.Value(queryMinimumKey{}).(int); ok {
					repeats = sampleRepetitions(stratum, warmCounts[stratum], minimum)
				}
			}
			if !isWarm {
				metric := q.Op
				if q.Op == "in" || q.Op == "out" || q.Op == "multi_in" {
					metric += "_" + q.Kind
				}
				samples[metric] = append(samples[metric], ns)
			}
			if e := encoder.Encode(pendingCheck{NS: ns, Query: q, Expected: want[i], Sources: want[i].Sources, NativeScoring: q.NativeScoring, MatchRepos: q.MatchRepos, Answer: got, Inputs: inputs, Warmup: isWarm}); e != nil {
				return samples, checks, e
			}
		}
	}
	if _, err = spool.Seek(0, 0); err != nil {
		return samples, checks, err
	}
	memo, err := newReaderCheckMemo(filepath.Dir(spool.Name()))
	if err != nil {
		return samples, checks, err
	}
	defer func() { err = errors.Join(err, memo.close()) }()
	decoder := json.NewDecoder(spool)
	for {
		var row pendingCheck
		e := decoder.Decode(&row)
		if e == io.EOF {
			break
		}
		if e != nil {
			return samples, checks, e
		}
		semantic := row
		semantic.NS = 0
		semantic.Warmup = false
		check, e := memo.check(ctx, semantic)
		if e != nil {
			return samples, checks, e
		}
		if latencies != nil && row.Ready == nil {
			latency, latencyErr := latencyFromPending(row, check)
			if latencyErr != nil {
				return samples, checks, latencyErr
			}
			if latencyErr = json.NewEncoder(latencies).Encode(latency); latencyErr != nil {
				return samples, checks, latencyErr
			}
		}
		addCheck(check, row.Warmup)
	}
	checks = append(checks, warmFailures...)
	return samples, checks, nil
}
func shuffled(qs []workload.Query, want []workload.Expected, seed uint64) ([]workload.Query, []workload.Expected) {
	rng := rand.New(rand.NewPCG(seed, 17))
	order := rng.Perm(len(qs))
	q := make([]workload.Query, len(qs))
	w := make([]workload.Expected, len(qs))
	for i, j := range order {
		q[i] = qs[j]
		w[i] = want[j]
	}
	return q, w
}
func warmedQueries(ctx context.Context, s map[string]adapter.Segment, m adapter.MultiRepo, qs []workload.Query, want []workload.Expected, caps adapter.Capabilities, seed uint64) (map[string][]int64, []workload.Check, error) {
	warmQ, warmW := shuffled(qs, want, seed)
	measuredQ, measuredW := shuffled(qs, want, seed+4002)
	return queryRun(ctx, s, m, append(warmQ, measuredQ...), append(warmW, measuredW...), caps, len(warmQ))
}
func runOnce(ctx context.Context, b *trackedBackend, factory adapter.CatalogFactory, ds []dataset.Dataset, qs []workload.Query, want []workload.Expected, dir string, trials int, safetyTimeout time.Duration, r *Result) (err error) {
	ctx = context.WithValue(ctx, queryLatencyKey{}, queryLatencyOutput{dir: dir, result: r})
	var referenceErr error
	if len(ds) > 0 && ds[0].Source == nil {
		want, referenceErr = oracle.ReferenceAnswers(ds, qs, want)
		if referenceErr != nil {
			return referenceErr
		}
	}
	refs := map[string]adapter.SegmentRef{}
	var buildTimes []int64
	// The runner owns the segments parent; adapters create only their leaf.
	if e := os.MkdirAll(filepath.Join(dir, "segments"), 0o755); e != nil {
		return e
	}
	for _, d := range ds {
		preparePhase()
		start := time.Now()
		ref, stats, e := b.Build(ctx, d, filepath.Join(dir, "segments", d.Repo))
		ns := time.Since(start).Nanoseconds()
		cpu := int64(0)
		if c := backendClient(b.Backend); c != nil {
			c.mu.Lock()
			ns = c.lastBuild
			cpu = c.lastBuildCPU
			c.mu.Unlock()
		}
		if e != nil {
			return e
		}
		if ref.Repo != d.Repo {
			return fmt.Errorf("wrong build repository")
		}
		bytes, e := localBytes(filepath.Join(dir, "segments", d.Repo))
		if e != nil {
			return e
		}
		if backendClient(b.Backend) != nil {
			if e = checkBuildCPU(cpu, bytes); e != nil {
				return e
			}
		}
		r.Builds = append(r.Builds, BuildRecord{CPU: cpu, ArtifactBytes: bytes, Repo: d.Repo, DurationNS: ns, Stats: stats, Ref: ref})
		refs[d.Repo] = ref
		buildTimes = append(buildTimes, ns)
	}
	r.Metrics["build_seal"] = Summarize(buildTimes)
	if e := resource(ctx, b.Backend, r, "build"); e != nil {
		return e
	}
	preparePhase()
	start := time.Now()
	client := backendClient(b.Backend)
	openBefore := int64(0)
	if client != nil {
		client.mu.Lock()
		openBefore = client.openNS
		client.mu.Unlock()
	}
	first, e := openAll(ctx, b, refs)
	if e != nil {
		return e
	}
	openElapsed := time.Since(start).Nanoseconds()
	if client != nil {
		client.mu.Lock()
		openElapsed = client.openNS - openBefore
		openBefore = client.openNS
		client.mu.Unlock()
	}
	r.Metrics["open_first_no_cache_flush"] = Summarize([]int64{openElapsed})
	if e = closeAll(first); e != nil {
		return e
	}
	first = nil
	preparePhase()
	start = time.Now()
	segments, e := openAll(ctx, b, refs)
	if e != nil {
		return e
	}
	defer func() { err = errors.Join(err, closeAll(segments)) }()
	openElapsed = time.Since(start).Nanoseconds()
	if client != nil {
		client.mu.Lock()
		openElapsed = client.openNS - openBefore
		client.mu.Unlock()
	}
	r.Metrics["open_warm"] = Summarize([]int64{openElapsed})
	if e = resource(ctx, b.Backend, r, "open"); e != nil {
		return e
	}
	fanout := adapter.Fanout{Segments: segments}
	preparePhase()
	samples, checks, e := warmedQueries(ctx, segments, fanout, qs, want, r.Capabilities, uint64(r.Run+1009))
	if e != nil {
		return e
	}
	r.Checks = append(r.Checks, checks...)
	for op, ns := range samples {
		r.Metrics[op] = Summarize(ns)
	}
	var nq []workload.Query
	var nw []workload.Expected
	for i, q := range qs {
		if q.Op == "traverse2" || q.Op == "traverse3" {
			q.Op = "native_" + q.Op
			q.ID = "native_" + q.ID
			nq = append(nq, q)
			nw = append(nw, want[i])
		}
	}
	ns, checks, e := warmedQueries(ctx, segments, fanout, nq, nw, r.Capabilities, uint64(r.Run+2011))
	if e != nil {
		return e
	}
	r.Checks = append(r.Checks, checks...)
	for op, v := range ns {
		r.Metrics[op] = Summarize(v)
	}
	if native, ok := b.Backend.(adapter.NativeMulti); ok {
		m, e := native.OpenMulti(ctx, segments)
		if e != nil {
			return e
		}
		nq = nil
		nw = nil
		for i, q := range qs {
			if strings.HasPrefix(q.Op, "multi_") && !strings.Contains(q.Op, "traverse") {
				q.Op = "native_" + q.Op
				q.ID = "native_" + q.ID
				nq = append(nq, q)
				nw = append(nw, globalExpected(want[i]))
			}
		}
		preparePhase()
		ns, checks, e = warmedQueries(ctx, segments, m, nq, nw, r.Capabilities, uint64(r.Run+3011))
		if e != nil {
			return e
		}
		r.Checks = append(r.Checks, checks...)
		for op, v := range ns {
			r.Metrics[op] = Summarize(v)
		}
		nq = nil
		nw = nil
		for i, q := range qs {
			if strings.HasPrefix(q.Op, "multi_traverse") {
				q.Op = "native_" + q.Op
				q.ID = "native_" + q.ID
				nq = append(nq, q)
				nw = append(nw, want[i])
			}
		}
		ns, checks, e = warmedQueries(ctx, segments, m, nq, nw, r.Capabilities, uint64(r.Run+4011))
		if e != nil {
			return e
		}
		r.Checks = append(r.Checks, checks...)
		for op, v := range ns {
			r.Metrics[op] = Summarize(v)
		}
	}
	if e = resource(ctx, b.Backend, r, "queries"); e != nil {
		return e
	}
	if r.TimingOnly {
		return nil
	}
	change, e := MakeChange(ds[0])
	if e != nil {
		return e
	}
	preparePhase()
	start = time.Now()
	delta, stats, e := b.BuildDelta(ctx, refs[ds[0].Repo], change, filepath.Join(dir, "delta"))
	nsDelta := time.Since(start).Nanoseconds()
	cpuDelta := int64(0)
	if c := backendClient(b.Backend); c != nil {
		c.mu.Lock()
		nsDelta = c.lastBuild
		cpuDelta = c.lastBuildCPU
		c.mu.Unlock()
	}
	if e != nil {
		return e
	}
	if stats.Mode != "full" && stats.Mode != "delta" {
		return fmt.Errorf("delta mode must be full or delta")
	}
	bytes, e := localBytes(filepath.Join(dir, "delta"))
	if e != nil {
		return e
	}
	if backendClient(b.Backend) != nil {
		if e = checkBuildCPU(cpuDelta, bytes); e != nil {
			return e
		}
	}
	r.Delta = BuildRecord{CPU: cpuDelta, ArtifactBytes: bytes, Repo: ds[0].Repo, DurationNS: nsDelta, Stats: stats, Ref: delta}
	r.Metrics["delta_build"] = Summarize([]int64{nsDelta})
	changed, e := adapter.ChangeSource(ds[0], change)
	if e != nil {
		return e
	}
	r.DeltaChecks, e = checkDelta(ctx, b, delta, ds[0], changed, change)
	if e != nil {
		return e
	}
	if e = resource(ctx, b.Backend, r, "delta"); e != nil {
		return e
	}
	preparePhase()
	r.Concurrent, e = concurrent(ctx, b, factory, segments, ds, refs[ds[0].Repo], changed, qs, want, filepath.Join(dir, "concurrent"), safetyTimeout)
	if e != nil {
		return e
	}
	if e = resource(ctx, b.Backend, r, "concurrent"); e != nil {
		return e
	}
	preparePhase()
	r.Crash, e = crashTests(ctx, b, factory, ds[0], refs[ds[0].Repo], delta, filepath.Join(dir, "crash"), trials, safetyTimeout)
	if e != nil {
		return e
	}
	return resource(ctx, b.Backend, r, "crash")
}
func localBytes(dir string) (int64, error) {
	n, e := oracle.DiskBytes(dir)
	if os.IsNotExist(e) {
		return 0, nil
	}
	return n, e
}

func summarizeOverlap(r *Result) {
	gate, local, quality := map[string][]float64{}, map[string][]float64{}, map[string][]float64{}
	for _, c := range r.Checks {
		if c.Overlap != nil {
			gate[c.Operation] = append(gate[c.Operation], *c.Overlap)
		}
		if c.LocalOverlap != nil {
			local[c.Operation] = append(local[c.Operation], *c.LocalOverlap)
		}
		if c.GlobalOverlap != nil {
			quality[c.Operation] = append(quality[c.Operation], *c.GlobalOverlap)
		}
	}
	summarize := func(all map[string][]float64) map[string]OverlapSummary {
		out := map[string]OverlapSummary{}
		for op, values := range all {
			slices.Sort(values)
			total := 0.
			for _, v := range values {
				total += v
			}
			out[op] = OverlapSummary{Min: values[0], Mean: total / float64(len(values)), P50: values[(len(values)-1)/2], Count: len(values)}
		}
		return out
	}
	r.Overlap = summarize(gate)
	r.LocalQuality = summarize(local)
	r.GlobalQuality = summarize(quality)
}
