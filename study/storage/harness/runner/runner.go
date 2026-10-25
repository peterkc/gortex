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
	"math"
	"math/rand/v2"
	"os"
	"os/exec"
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
	ArtifactBytes int64              `json:"artifact_bytes"`
	Repo          string             `json:"repo"`
	DurationNS    int64              `json:"duration_ns"`
	Stats         adapter.BuildStats `json:"stats"`
	Ref           adapter.SegmentRef `json:"ref"`
}
type ConcurrentResult struct {
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
	Data          string
	Out           string
	Run           int
	Trials        int
	SafetyTimeout time.Duration
	TimingOnly    bool
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
			err = RunCatalogBenchmark(b, factory, os.Args[2:])
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
					err = runChild(b, factory, req)
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
	safetyTimeout := fs.Duration("safety-timeout", defaultSafetyTimeout, "safety deadline for each commit-race check and crash child")
	if err := fs.Parse(os.Args[1:]); err != nil {
		return err
	}
	if *data == "" || *out == "" || *runs < 1 || *trials < 1 {
		return fmt.Errorf("data, out and positive runs/trials required")
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
	for run := first; run <= *runs; run++ {
		path := filepath.Join(*out, fmt.Sprintf("request-%02d.json", run))
		if err = dataset.WriteJSON(path, runRequest{Data: *data, Out: *out, Run: run, Trials: *trials, SafetyTimeout: *safetyTimeout, TimingOnly: *timingOnly}); err != nil {
			return err
		}
		cmd := exec.Command(exe, "__measure_run", path)
		cmd.Stdout = os.Stdout
		cmd.Stderr = os.Stderr
		if err = cmd.Run(); err != nil {
			return fmt.Errorf("fresh-process run %d: %w", run, err)
		}
	}
	return nil
}
func loadTier(dir string) ([]dataset.Dataset, []workload.Query, []workload.Expected, string, error) {
	var meta dataset.Manifest
	if e := dataset.ReadJSON(filepath.Join(dir, "manifest.json"), &meta); e != nil {
		return nil, nil, nil, "", e
	}
	for p, want := range meta.SHA256 {
		got, e := dataset.Hash(filepath.Join(dir, p))
		if e != nil {
			return nil, nil, nil, "", e
		}
		if got != want {
			return nil, nil, nil, "", fmt.Errorf("tier checksum mismatch %s", p)
		}
	}
	var ds []dataset.Dataset
	for _, id := range meta.Repos {
		d, e := dataset.Load(filepath.Join(dir, id))
		if e != nil {
			return nil, nil, nil, "", e
		}
		ds = append(ds, d)
	}
	var qs []workload.Query
	var want []workload.Expected
	if e := dataset.ReadJSON(filepath.Join(dir, "queries.json"), &qs); e != nil {
		return nil, nil, nil, "", e
	}
	if e := dataset.ReadJSON(filepath.Join(dir, "expected.json"), &want); e != nil {
		return nil, nil, nil, "", e
	}
	if len(ds) == 0 || len(qs) != len(want) {
		return nil, nil, nil, "", fmt.Errorf("empty tier or mismatched workload")
	}
	for i, q := range qs {
		if q.ID != want[i].QueryID {
			return nil, nil, nil, "", fmt.Errorf("query IDs differ")
		}
	}
	digest, e := dataset.Hash(filepath.Join(dir, "manifest.json"))
	return ds, qs, want, digest, e
}
func runChild(original adapter.Backend, factory adapter.CatalogFactory, req runRequest) error {
	if req.SafetyTimeout < 0 {
		return fmt.Errorf("safety-timeout must be positive")
	}
	if req.SafetyTimeout == 0 {
		req.SafetyTimeout = defaultSafetyTimeout
	}
	ctx := context.Background()
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
	summarizeOverlap(&r)
	collectFailures(&r)
	r.Success = len(r.Failures) == 0
	if req.Run > 0 {
		if e = dataset.WriteJSON(filepath.Join(req.Out, fmt.Sprintf("%s-%s-%02d.json", original.Name(), r.Tier, req.Run)), r); e != nil {
			return e
		}
	} else {
		status := map[string]any{"pid": r.PID, "success": r.Success, "failures": r.Failures}
		if r.TimingOnly {
			status["timing_only"] = true
		}
		if e = dataset.WriteJSON(filepath.Join(dir, "warmup-status.json"), status); e != nil {
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
	samples := map[string][]int64{}
	checks := make([]workload.Check, 0, len(qs))
	for i, q := range qs {
		q = withPolicy(q, caps)
		if why := unsupported(caps, q); why != "" {
			checks = append(checks, workload.Check{QueryID: q.ID, Operation: q.Op, Status: "unsupported", Detail: why})
			continue
		}
		queryMulti, inputs := traceNativeFanout(m, q)
		start := time.Now()
		got, e := workload.Evaluate(ctx, s, queryMulti, q)
		ns := time.Since(start).Nanoseconds()
		if e != nil {
			if errors.Is(e, adapter.ErrUnsupported) && strings.HasPrefix(q.Op, "native_") {
				checks = append(checks, workload.Check{QueryID: q.ID, Operation: q.Op, Status: "unsupported", Detail: e.Error()})
				continue
			}
			checks = append(checks, workload.Check{QueryID: q.ID, Operation: q.Op, Status: "failed", Mismatch: true, Detail: e.Error()})
			continue
		}
		metric := q.Op
		if q.Op == "in" || q.Op == "out" || q.Op == "multi_in" {
			metric += "_" + q.Kind
		}
		samples[metric] = append(samples[metric], ns)
		checks = append(checks, compareRankedAnswer(q, want[i], got, inputs))
	}
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
	_, warm, e := measuredQueries(ctx, s, m, warmQ, warmW, caps)
	if e != nil {
		return nil, warm, e
	}
	measuredQ, measuredW := shuffled(qs, want, seed+4002)
	ns, checks, e := measuredQueries(ctx, s, m, measuredQ, measuredW, caps)
	for _, c := range warm {
		if c.Mismatch {
			c.QueryID = "warmup_" + c.QueryID
			checks = append(checks, c)
		}
	}
	return ns, checks, e
}
func runOnce(ctx context.Context, b *trackedBackend, factory adapter.CatalogFactory, ds []dataset.Dataset, qs []workload.Query, want []workload.Expected, dir string, trials int, safetyTimeout time.Duration, r *Result) (err error) {
	var referenceErr error
	want, referenceErr = oracle.ReferenceAnswers(ds, qs, want)
	if referenceErr != nil {
		return referenceErr
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
		r.Builds = append(r.Builds, BuildRecord{ArtifactBytes: bytes, Repo: d.Repo, DurationNS: ns, Stats: stats, Ref: ref})
		refs[d.Repo] = ref
		buildTimes = append(buildTimes, ns)
	}
	r.Metrics["build_seal"] = Summarize(buildTimes)
	if e := resource(ctx, b.Backend, r, "build"); e != nil {
		return e
	}
	preparePhase()
	start := time.Now()
	first, e := openAll(ctx, b, refs)
	if e != nil {
		return e
	}
	r.Metrics["open_first_no_cache_flush"] = Summarize([]int64{time.Since(start).Nanoseconds()})
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
	r.Metrics["open_warm"] = Summarize([]int64{time.Since(start).Nanoseconds()})
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
	r.Delta = BuildRecord{ArtifactBytes: bytes, Repo: ds[0].Repo, DurationNS: nsDelta, Stats: stats, Ref: delta}
	r.Metrics["delta_build"] = Summarize([]int64{nsDelta})
	changed := adapter.ApplyChange(ds[0], change)
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
