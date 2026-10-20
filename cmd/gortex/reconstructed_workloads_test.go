//go:build acceptance

package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"
)

// reconstructedSetupReadyTimeout bounds the unmeasured ready probes before a
// workload. It matches the R-Fresh deadline.
const reconstructedSetupReadyTimeout = 900 * time.Second

type reconstructedSymbol struct {
	Name string `json:"name"`
	File string `json:"file"`
}

type reconstructedDriver struct {
	f       *issue767Fixture
	run     reconstructedRun
	symbols []reconstructedSymbol
	files   []string
	fresh   reconstructedSymbol
}

func newReconstructedDriver(t *testing.T, workload string) *reconstructedDriver {
	t.Helper()
	binary, corpus := os.Getenv("GORTEX_RECON_TEST_BINARY"), os.Getenv("GORTEX_RECON_CORPUS")
	if binary == "" || corpus == "" {
		t.Skip("set GORTEX_RECON_TEST_BINARY and GORTEX_RECON_CORPUS; reconstructed drivers run only in fork CI")
	}
	if os.Getenv("GORTEX_RECON_OUT") == "" {
		t.Fatal("GORTEX_RECON_OUT must name a JSONL output file")
	}
	var err error
	binary, err = filepath.Abs(binary)
	if err != nil {
		t.Fatal(err)
	}
	corpus, err = filepath.Abs(corpus)
	if err != nil {
		t.Fatal(err)
	}
	d := &reconstructedDriver{run: reconstructedRun{Label: "reconstructed", Workload: workload, Parameters: map[string]any{
		"corpus_commit": "91a5c951", "corpus_recipe": "git archive, private repository with one commit",
		"binary": binary, "poll_ms": 100, "probe_timeout_s": 60, "fresh_timeout_s": 900,
		"setup_ready_timeout_s": reconstructedSetupReadyTimeout.Seconds(), "Fresh_target_s": 180,
		"quiet_window_s": 10, "quiet_write_limit_bytes_exclusive": 1 << 20, "quiet_timeout_s": 300,
		"reconcile_interval": issue767DefaultTestReconcileInterval, "janitor": "fixture default",
		"oracle":     "askSymbol + issue767Verdict; one gortex call search per search",
		"percentile": "nearest rank, failures included", "C2_worktrees": 2, "C2_edits": 10,
		"C2_edits_per_worktree": 5, "C2_burst_limit_ms": 100, "C2_target_ms": 5000,
		"DE_saves": 150, "DE_save_interval_s": 2, "DE_probe_every": 7, "DE_probes": 20,
		"DE_job_p95_target_ms": 700, "Fill_idle_searches": 100, "Fill_symbol_count": 20,
		"Fill_loaded_searches": 100, "Fill_min_loaded_searches": 30, "Fill_max_rebuilds": 3,
		"Fill_ratio_target": 2, "Restart_after_ready_s": 60,
		"Fresh_file_rule":   "lexicographically last non-test .go file, byte order of repository-relative path",
		"Fresh_symbol_rule": "first exported top-level declaration in source order",
	}}}
	count, bytes := 0, int64(0)
	d.f = newIssue767FixtureWithCorpus(t, binary, func(f *issue767Fixture) {
		err := filepath.WalkDir(corpus, func(path string, entry fs.DirEntry, walkErr error) error {
			if walkErr != nil {
				return walkErr
			}
			rel, err := filepath.Rel(corpus, path)
			if err != nil {
				return err
			}
			if entry.IsDir() {
				if entry.Name() == ".git" {
					return filepath.SkipDir
				}
				return nil
			}
			info, err := entry.Info()
			if err != nil {
				return err
			}
			if !info.Mode().IsRegular() {
				return fmt.Errorf("exported corpus has non-regular file %s", rel)
			}
			content, err := os.ReadFile(path)
			if err != nil {
				return err
			}
			f.write(filepath.Join(f.primary, rel), string(content))
			count++
			bytes += int64(len(content))
			if strings.HasSuffix(rel, ".go") && !strings.HasSuffix(rel, "_test.go") {
				d.files = append(d.files, filepath.ToSlash(rel))
			}
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
	})
	sort.Strings(d.files)
	if len(d.files) < 5 {
		t.Fatal("exported corpus needs at least five existing non-test .go files")
	}
	seen := make(map[string]bool)
	for _, file := range d.files {
		if len(d.symbols) == 20 && file != d.files[len(d.files)-1] {
			continue
		}
		declarations, err := reconstructedDeclarations(filepath.Join(d.f.primary, file))
		if err != nil {
			t.Fatal(err)
		}
		for _, name := range declarations {
			if len(d.symbols) < 20 && !seen[name] {
				d.symbols = append(d.symbols, reconstructedSymbol{Name: name, File: file})
				seen[name] = true
			}
		}
		if file == d.files[len(d.files)-1] {
			if len(declarations) == 0 {
				t.Fatalf("last non-test .go file %s has no exported top-level declaration", file)
			}
			d.fresh = reconstructedSymbol{Name: declarations[0], File: file}
		}
	}
	if len(d.symbols) != 20 {
		t.Fatal("exported corpus needs 20 distinct exported corpus symbol names")
	}
	d.run.Parameters["search_spelling"] = issue767AsPrimary.String()
	if workload == "R-C2" {
		d.run.Parameters["search_spelling"] = issue767AsAutomaticWorktree.String()
	}
	d.run.Parameters["job_telemetry"] = "private JSON daemon logs: contract analysis job elapsed seconds; starts count jobs when available; missing telemetry unsupported"
	d.run.Parameters["corpus_file_count"], d.run.Parameters["corpus_bytes"] = count, bytes
	d.run.Parameters["search_symbols"] = d.symbols
	d.run.Parameters["search_symbol_rule"] = "first 20 distinct exported top-level names, sorted non-test .go paths then source order"
	d.run.Parameters["Fresh_file"], d.run.Parameters["Fresh_symbol"] = d.fresh.File, d.fresh.Name
	t.Cleanup(func() { d.finish(t) })
	return d
}

func reconstructedDeclarations(path string) ([]string, error) {
	file, err := parser.ParseFile(token.NewFileSet(), path, nil, 0)
	if err != nil {
		return nil, err
	}
	var names []string
	for _, decl := range file.Decls {
		switch decl := decl.(type) {
		case *ast.FuncDecl:
			if decl.Recv == nil && ast.IsExported(decl.Name.Name) {
				names = append(names, decl.Name.Name)
			}
		case *ast.GenDecl:
			for _, spec := range decl.Specs {
				switch spec := spec.(type) {
				case *ast.TypeSpec:
					if ast.IsExported(spec.Name.Name) {
						names = append(names, spec.Name.Name)
					}
				case *ast.ValueSpec:
					for _, name := range spec.Names {
						if ast.IsExported(name.Name) {
							names = append(names, name.Name)
						}
					}
				}
			}
		}
	}
	return names, nil
}

func (d *reconstructedDriver) finish(t *testing.T) {
	if !d.f.stopAndJoin() {
		d.run.Error = "fixture child did not join"
	}
	d.run.Resources = d.f.processResources()
	score, err := scoreReconstructed(d.run)
	if err != nil {
		// Keep the driver's own failure: it is the cause the report records.
		if d.run.Error != "" {
			d.run.Error += "; "
		}
		d.run.Error += "scorer: " + err.Error()
		t.Errorf("reconstructed scorer: %v", err)
	} else {
		d.run.Score = &score
	}
	out, err := os.OpenFile(os.Getenv("GORTEX_RECON_OUT"), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0600)
	if err != nil {
		t.Error(err)
		return
	}
	defer out.Close()
	if err := json.NewEncoder(out).Encode(d.run); err != nil {
		t.Error(err)
	}
}

// Probe start is the stimulus time, not the time this goroutine gets scheduled.
func (d *reconstructedDriver) probe(root string, symbol reconstructedSymbol, spelling issue767Spelling, start time.Time, timeout time.Duration, kind string) reconstructedSample {
	sample := reconstructedSample{Kind: kind, Symbol: symbol.Name, File: filepath.Join(root, symbol.File), Start: start, TimeoutMS: float64(timeout) / float64(time.Millisecond)}
	deadline := start.Add(timeout)
	// A failed or slow CLI call is not an answer. The probe keeps polling, so
	// a probe that never passes counts at the timeout, not at the elapsed time.
	var callErr string
	for time.Now().Before(deadline) {
		sample.Polls++
		answer, err := d.f.askSymbol(root, symbol.Name, sample.File, spelling)
		sample.Answer = answer
		if err != nil {
			callErr = err.Error()
		} else {
			passed, err := issue767Verdict(answer, spelling)
			if err != nil && !issue767Inexact(err) {
				sample.Error = err.Error()
				break
			}
			if passed && time.Now().Before(deadline) {
				sample.Passed = true
				break
			}
		}
		timer := time.NewTimer(min(100*time.Millisecond, time.Until(deadline)))
		select {
		case <-d.f.t.Context().Done():
			timer.Stop()
			sample.Error = d.f.t.Context().Err().Error()
		case <-timer.C:
		}
		if sample.Error != "" {
			break
		}
	}
	sample.End = time.Now()
	sample.DurationMS = sample.End.Sub(start).Seconds() * 1000
	if !sample.Passed && sample.Error == "" {
		sample.TimedOut, sample.DurationMS, sample.Error = true, sample.TimeoutMS, callErr
	}
	return sample
}

func (d *reconstructedDriver) readyAndQuiet() bool {
	d.f.start()
	symbol := d.symbols[0]
	// Setup readiness is a cold start, not a measured probe: give it the
	// fresh-start deadline so a slow cold index is not a harness failure.
	ready := d.probe(d.f.primary, symbol, issue767AsPrimary, time.Now(), reconstructedSetupReadyTimeout, "ready")
	d.run.Parameters["ready_probe"] = ready
	if !ready.Passed {
		d.run.Error = "initial ready probe failed: " + ready.Error
		return false
	}
	return d.quiet()
}

func (d *reconstructedDriver) quiet() bool {
	ctx, cancel := context.WithTimeout(d.f.t.Context(), 300*time.Second)
	defer cancel()
	type observation struct {
		at time.Time
		io issue767ProcessIO
	}
	var history []observation
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	for {
		io, err := d.f.sampleProcessIO(ctx, d.f.pid())
		if err != nil {
			d.run.Parameters["quiet_error"] = err.Error()
			return false
		}
		now := time.Now()
		history = append(history, observation{at: now, io: io})
		for len(history) > 1 && now.Sub(history[1].at) >= 10*time.Second {
			history = history[1:]
		}
		first := history[0]
		if now.Sub(first.at) >= 10*time.Second && io.StartTicks == first.io.StartTicks && io.BytesWritten >= first.io.BytesWritten && io.BytesWritten-first.io.BytesWritten < 1<<20 {
			d.run.Quiet = true
			d.run.Parameters["quiet_observed_window_s"] = now.Sub(first.at).Seconds()
			d.run.Parameters["quiet_observed_write_bytes"] = io.BytesWritten - first.io.BytesWritten
			return true
		}
		select {
		case <-ctx.Done():
			d.run.Parameters["quiet_error"] = ctx.Err().Error()
			return false
		case <-ticker.C:
		}
	}
}

func reconstructedAppend(path, name string) (time.Time, error) {
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_APPEND, 0)
	if err != nil {
		return time.Time{}, err
	}
	start := time.Now()
	_, writeErr := fmt.Fprintf(file, "\nfunc %s() {}\n", name)
	closeErr := file.Close()
	return start, errors.Join(writeErr, closeErr)
}

func TestReconstructedC2(t *testing.T) {
	d := newReconstructedDriver(t, "R-C2")
	roots := []string{d.f.linked, filepath.Join(d.f.root, "linked-two")}
	for i, root := range roots {
		d.f.git(d.f.primary, "worktree", "add", "-b", fmt.Sprintf("reconstructed-%d", i), root)
	}
	if !d.readyAndQuiet() {
		return
	}
	for _, root := range roots {
		ready := d.probe(root, d.symbols[0], issue767AsAutomaticWorktree, time.Now(), reconstructedSetupReadyTimeout, "ready")
		if !ready.Passed {
			d.run.Error = "automatic worktree ready probe failed: " + ready.Error
			return
		}
	}
	// Readiness requests can create work. Re-establish quiet before the burst.
	d.run.Quiet = false
	if !d.quiet() {
		return
	}
	type edit struct {
		root   string
		symbol reconstructedSymbol
		start  time.Time
	}
	var edits []edit
	burst := time.Now()
	for i, root := range roots {
		for j := 0; j < 5; j++ {
			symbol := reconstructedSymbol{Name: fmt.Sprintf("ReconstructedC2WT%dEdit%d", i, j), File: d.files[j]}
			start, err := reconstructedAppend(filepath.Join(root, symbol.File), symbol.Name)
			if err != nil {
				d.run.Error = err.Error()
				return
			}
			edits = append(edits, edit{root: root, symbol: symbol, start: start})
		}
	}
	burstDuration := time.Since(burst)
	d.run.Parameters["C2_observed_burst_ms"] = burstDuration.Seconds() * 1000
	if burstDuration > 100*time.Millisecond {
		d.run.Error = "ten writes exceeded the 100 ms burst limit"
	}
	results := make(chan reconstructedSample, 10)
	var probes sync.WaitGroup
	for _, edit := range edits {
		probes.Add(1)
		go func() {
			defer probes.Done()
			results <- d.probe(edit.root, edit.symbol, issue767AsAutomaticWorktree, edit.start, time.Minute, "probe")
		}()
	}
	probes.Wait()
	close(results)
	for sample := range results {
		d.run.Samples = append(d.run.Samples, sample)
	}
	sort.Slice(d.run.Samples, func(i, j int) bool { return d.run.Samples[i].Start.Before(d.run.Samples[j].Start) })
}

func TestReconstructedDE(t *testing.T) {
	d := newReconstructedDriver(t, "R-DE")
	if !d.readyAndQuiet() {
		return
	}
	results := make(chan reconstructedSample, 20)
	var probes sync.WaitGroup
	cadence := time.Now()
	var saveTimes []time.Time
	for save := 1; save <= 150; save++ {
		if save > 1 {
			timer := time.NewTimer(time.Until(cadence.Add(time.Duration(save-1) * 2 * time.Second)))
			select {
			case <-t.Context().Done():
				timer.Stop()
				d.run.Error = t.Context().Err().Error()
			case <-timer.C:
			}
			if d.run.Error != "" {
				break
			}
		}
		symbol := reconstructedSymbol{Name: fmt.Sprintf("ReconstructedDESave%03d", save), File: d.files[0]}
		start, err := reconstructedAppend(filepath.Join(d.f.primary, symbol.File), symbol.Name)
		if err != nil {
			d.run.Error = err.Error()
			break
		}
		saveTimes = append(saveTimes, start)
		if save%7 != 0 || save > 140 {
			continue
		}
		probes.Add(1)
		go func() {
			defer probes.Done()
			results <- d.probe(d.f.primary, symbol, issue767AsPrimary, start, time.Minute, "probe")
		}()
	}
	probes.Wait()
	close(results)
	for sample := range results {
		d.run.Samples = append(d.run.Samples, sample)
	}
	sort.Slice(d.run.Samples, func(i, j int) bool { return d.run.Samples[i].Start.Before(d.run.Samples[j].Start) })
	d.run.Parameters["DE_observed_save_times"] = saveTimes
	jobs, _, _, err := reconstructedJobs(d.f.logPath(), cadence, time.Now())
	if err != nil {
		d.run.Error = err.Error()
		return
	}
	d.run.JobMS = jobs
}

func (d *reconstructedDriver) search(kind string, index int) reconstructedSample {
	symbol := d.symbols[index%20]
	sample := reconstructedSample{Kind: kind, Symbol: symbol.Name, File: filepath.Join(d.f.primary, symbol.File), Start: time.Now()}
	answer, err := d.f.askSymbol(d.f.primary, symbol.Name, sample.File, issue767AsPrimary)
	sample.End = time.Now()
	sample.DurationMS = sample.End.Sub(sample.Start).Seconds() * 1000
	sample.Answer = answer
	if err != nil {
		sample.Error = err.Error()
		return sample
	}
	sample.Passed, err = issue767Verdict(answer, issue767AsPrimary)
	if err != nil && !issue767Inexact(err) {
		sample.Error = err.Error()
	}
	return sample
}

func TestReconstructedFill(t *testing.T) {
	d := newReconstructedDriver(t, "R-Fill")
	if !d.readyAndQuiet() {
		return
	}
	for i := 0; i < 100; i++ {
		d.run.Samples = append(d.run.Samples, d.search("idle", i))
	}
	payload, err := json.Marshal(map[string]string{"path": d.f.primary})
	if err != nil {
		t.Fatal(err)
	}
	loaded, index := 0, 0
	for attempt := 0; attempt < 3 && loaded < 100; attempt++ {
		started := make(chan struct{})
		finished := make(chan reconstructedRebuild, 1)
		go func() {
			rebuild := reconstructedRebuild{Start: time.Now()}
			close(started)
			output, err := d.f.tryCommand(900*time.Second, d.f.primary, "call", "index_repository", "--index", d.f.primary, "--json", string(payload), "--format", "json")
			rebuild.End = time.Now()
			rebuild.DurationMS = rebuild.End.Sub(rebuild.Start).Seconds() * 1000
			if err != nil {
				rebuild.Error = fmt.Sprintf("index_repository: %v: %s", err, output)
			} else {
				var value any
				if err := json.Unmarshal(output, &value); err != nil {
					rebuild.Error = fmt.Sprintf("index_repository response: %v", err)
				} else if _, failed := issue767JSONEvidence(value, ""); failed {
					rebuild.Error = fmt.Sprintf("index_repository returned a tool error: %s", output)
				}
			}
			finished <- rebuild
		}()
		<-started
		first := len(d.run.Samples)
		var rebuild reconstructedRebuild
	searches:
		for len(d.run.Samples)-first+loaded < 100 {
			select {
			case rebuild = <-finished:
				break searches
			default:
			}
			sample := d.search("load", index)
			index++
			d.run.Samples = append(d.run.Samples, sample)
			if sample.Error != "" {
				// Preserve failures, but stop a transport failure from spinning
				// through thousands of searches while the rebuild winds down.
				// The rebuild command owns its 900 s deadline and is always joined.
				d.run.Error = sample.Error
				break
			}
		}
		if rebuild.End.IsZero() {
			rebuild = <-finished
		}
		d.run.Rebuilds = append(d.run.Rebuilds, rebuild)
		loaded = 0
		for _, sample := range d.run.Samples {
			if sample.Kind == "load" && reconstructedLoaded(sample, d.run.Rebuilds) {
				loaded++
			}
		}
		if rebuild.Error != "" || d.run.Error != "" {
			break
		}
	}
}

func TestReconstructedFresh(t *testing.T) {
	d := newReconstructedDriver(t, "R-Fresh")
	start := time.Now()
	d.f.start()
	d.run.Samples = append(d.run.Samples, d.probe(d.f.primary, d.fresh, issue767AsPrimary, start, 900*time.Second, "fresh"))
}

func TestReconstructedRestart(t *testing.T) {
	d := newReconstructedDriver(t, "R-Restart")
	start := time.Now()
	d.f.start()
	fresh := d.probe(d.f.primary, d.fresh, issue767AsPrimary, start, 900*time.Second, "fresh")
	d.run.Parameters["fresh_preparation_probe"] = fresh
	if !fresh.Passed {
		d.run.Error = "R-Fresh preparation failed: " + fresh.Error
		return
	}
	if !d.quiet() {
		return
	}
	if !d.f.stopAndJoin() {
		d.run.Error = "initial daemon did not join before restart"
		return
	}
	_, _, telemetry, err := reconstructedJobs(d.f.logPath(), start, time.Now())
	if err != nil {
		d.run.Error = err.Error()
		return
	}
	before, err := reconstructedStoreBytes(d.f.store)
	if err != nil {
		d.run.Error = err.Error()
		return
	}
	start = time.Now()
	d.f.start()
	ready := d.probe(d.f.primary, d.fresh, issue767AsPrimary, start, time.Minute, "ready")
	d.run.Samples = append(d.run.Samples, ready)
	if !ready.Passed {
		d.run.Error = "restart ready probe failed: " + ready.Error
		return
	}
	timer := time.NewTimer(60 * time.Second)
	select {
	case <-t.Context().Done():
		timer.Stop()
		d.run.Error = t.Context().Err().Error()
		return
	case <-timer.C:
	}
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	io, err := d.f.sampleProcessIO(ctx, d.f.pid())
	cancel()
	if err != nil {
		d.run.Error = "restart process I/O: " + err.Error()
		return
	}
	d.run.ProcessBytes = &io.BytesWritten // cumulative since this process was born
	after, err := reconstructedStoreBytes(d.f.store)
	if err != nil {
		d.run.Error = err.Error()
		return
	}
	growth := after - before
	d.run.StoreGrowth = &growth
	end := time.Now()
	d.run.Parameters["restart_measurement_start"], d.run.Parameters["restart_measurement_end"] = start, end
	_, jobs, restartTelemetry, err := reconstructedJobs(d.f.logPath(), start, end)
	if err != nil {
		d.run.Error = err.Error()
		return
	}
	if telemetry || restartTelemetry {
		d.run.JobCount = &jobs
	}
}

func reconstructedStoreBytes(store string) (int64, error) {
	var bytes int64
	for _, path := range []string{store, store + "-wal"} {
		info, err := os.Stat(path)
		if errors.Is(err, fs.ErrNotExist) {
			continue
		}
		if err != nil {
			return 0, err
		}
		bytes += info.Size()
	}
	return bytes, nil
}

// The fixture uses the production JSON zap encoder: ts and elapsed are seconds.
// Exact start/end messages are telemetry. No lines means unsupported,
// not zero. R-Restart may establish support from its preceding fresh process.
func reconstructedJobs(path string, start, end time.Time) ([]float64, int, bool, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, 0, false, err
	}
	defer file.Close()
	scanner := bufio.NewScanner(file)
	scanner.Buffer(make([]byte, 4096), 4<<20)
	var durations []float64
	count, starts, supported, hasStartLines := 0, 0, false, false
	missingDuration := false
	for scanner.Scan() {
		var line struct {
			Message string   `json:"msg"`
			Time    float64  `json:"ts"`
			Elapsed *float64 `json:"elapsed"`
		}
		if err := json.Unmarshal(scanner.Bytes(), &line); err != nil {
			continue // Other CLI output is not a structured log line.
		}
		if line.Message != "contract analysis job" && line.Message != "contract analysis job started" {
			continue
		}
		supported = true
		if line.Message == "contract analysis job started" {
			hasStartLines = true
		}
		at := time.Unix(0, int64(line.Time*1e9))
		if at.Before(start) || at.After(end) {
			continue
		}
		if line.Message == "contract analysis job started" {
			starts++
			continue
		}
		count++
		if line.Elapsed == nil {
			missingDuration = true
			continue
		}
		durations = append(durations, *line.Elapsed*1000)
	}
	if err := scanner.Err(); err != nil {
		return nil, 0, false, err
	}
	if missingDuration {
		durations = nil // Do not report a biased subset as the job p95.
	}
	if hasStartLines {
		count = starts // Count in-flight jobs too; never double-count start + end.
	}
	return durations, count, supported, nil
}
