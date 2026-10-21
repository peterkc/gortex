//go:build acceptance

package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"io"
	"io/fs"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"
)

// reconstructedSetupReadyTimeout bounds readiness and full enrichment separately.
// Both use the approved R-Fresh driver deadline, not its 180 s target.
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
	started time.Time
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
		"Fill_ratio_target": 2, "Restart_after_enrichment_s": 60,
		"Fresh_file_rule":   "lexicographically last non-test .go file outside testdata, byte order of repository-relative path",
		"Fresh_symbol_rule": "first exported top-level declaration in source order",
	}}}
	var opts []issue767FixtureOption
	if workload == "R-Fill" {
		opts = append(opts, issue767WithLoopbackHTTP)
	}
	count, bytes := 0, int64(0)
	var goFiles []string
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
			// Gortex classifies testdata files as fixtures, so their symbols
			// never answer the oracle. Stimuli and probes use indexed source only.
			if strings.HasSuffix(rel, ".go") && !slices.Contains(strings.Split(filepath.ToSlash(rel), "/"), "testdata") {
				goFiles = append(goFiles, filepath.ToSlash(rel))
				if !strings.HasSuffix(rel, "_test.go") {
					d.files = append(d.files, filepath.ToSlash(rel))
				}
			}
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
		f.write(filepath.Join(f.primary, reconstructedRouteFile), reconstructedRouteSource(reconstructedRoute(0)))
	}, opts...)
	d.f.env = append(d.f.env, "GORTEX_TOOLS=full")
	d.run.Parameters["contract_route_fixture"] = reconstructedRouteSource(reconstructedRoute(0))
	d.run.Parameters["contract_route_fixture_bytes"] = len(reconstructedRouteSource(reconstructedRoute(0)))
	d.run.Parameters["contract_route_file"] = reconstructedRouteFile
	d.run.Parameters["fixture_HOME"] = filepath.Join(d.f.root, "home")
	d.run.Parameters["http_addr"] = d.f.httpAddr
	d.run.Parameters["http_tool_timeout"] = "off (R-Fill only)"
	// A slow shutdown is a product result recorded in the row (forced_stops),
	// not a harness failure.
	d.f.forcedStopIsResult = true
	sort.Strings(d.files)
	if len(d.files) < 5 {
		t.Fatal("exported corpus needs at least five existing non-test .go files")
	}
	declarations := make(map[string][]string, len(goFiles))
	for _, file := range goFiles {
		names, err := reconstructedDeclarations(filepath.Join(d.f.primary, file))
		if err != nil {
			t.Fatal(err)
		}
		declarations[file] = names
	}
	d.symbols = reconstructedSearchSymbols(d.files, declarations, 20)
	if len(d.symbols) != 20 {
		t.Fatal("exported corpus needs 20 exported corpus symbol names declared exactly once")
	}
	last := d.files[len(d.files)-1]
	if len(declarations[last]) == 0 {
		t.Fatalf("last non-test .go file %s has no exported top-level declaration", last)
	}
	d.fresh = reconstructedSymbol{Name: declarations[last][0], File: last}
	d.run.Parameters["search_spelling"] = issue767AsPrimary.String()
	if workload == "R-C2" {
		d.run.Parameters["search_spelling"] = issue767AsAutomaticWorktree.String()
	}
	d.run.Parameters["job_telemetry"] = "private JSON daemon logs: contract analysis job elapsed seconds; starts count jobs when available; missing telemetry unsupported"
	d.run.Parameters["corpus_file_count"], d.run.Parameters["corpus_bytes"] = count, bytes
	d.run.Parameters["search_symbols"] = d.symbols
	d.run.Parameters["search_symbol_rule"] = "first 20 exported top-level names declared exactly once among .go files outside testdata (tests included), taken from sorted non-test paths, then source order"
	d.run.Parameters["Fresh_file"], d.run.Parameters["Fresh_symbol"] = d.fresh.File, d.fresh.Name
	t.Cleanup(func() { d.finish(t) })
	return d
}

// reconstructedSearchSymbols returns the first count names, in file order, that
// are declared exactly once across declarations. A name declared in several
// files lets a name-only search rank another definition above the expected one.
func reconstructedSearchSymbols(files []string, declarations map[string][]string, count int) []reconstructedSymbol {
	uses := make(map[string]int)
	for _, names := range declarations {
		for _, name := range names {
			uses[name]++
		}
	}
	var symbols []reconstructedSymbol
	for _, file := range files {
		for _, name := range declarations[file] {
			if uses[name] == 1 && len(symbols) < count {
				symbols = append(symbols, reconstructedSymbol{Name: name, File: file})
			}
		}
	}
	return symbols
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
	d.run.Parameters["forced_stops"] = d.f.forcedStops
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

func (d *reconstructedDriver) awaitEnrichment() bool {
	start := time.Now()
	ctx, cancel := context.WithDeadline(d.f.t.Context(), d.started.Add(reconstructedSetupReadyTimeout))
	defer cancel()
	err := reconstructedWaitEnrichment(ctx, d.f.logPath())
	d.run.Parameters[fmt.Sprintf("enrichment_run_%d_ms", d.f.runIndex())] = time.Since(start).Seconds() * 1000
	if err != nil {
		d.run.Error = "enrichment completion: " + err.Error()
		return false
	}
	return true
}

func (d *reconstructedDriver) readyAndQuiet(roots ...string) bool {
	d.started = time.Now()
	d.f.start()
	ready := d.probe(d.f.primary, d.symbols[0], issue767AsPrimary, time.Now(), reconstructedSetupReadyTimeout, "ready")
	d.run.Parameters["ready_probe"] = ready
	if !ready.Passed {
		d.run.Error = "initial ready probe failed: " + ready.Error
		return false
	}
	return reconstructedPrepare(d.awaitEnrichment, func() bool {
		for _, root := range roots {
			ready := d.probe(root, d.symbols[0], issue767AsAutomaticWorktree, time.Now(), reconstructedSetupReadyTimeout, "ready")
			if !ready.Passed {
				d.run.Error = "automatic worktree ready probe failed: " + ready.Error
				return false
			}
		}
		return true
	}, d.quiet)
}

// Enrichment opens the view-build gate; quiet before routing views is premature.
func reconstructedPrepare(enrichment, views, quiet func() bool) bool {
	return enrichment() && views() && quiet()
}

func reconstructedWaitEnrichment(ctx context.Context, path string) error {
	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		for _, raw := range bytes.Split(data, []byte("\n")) {
			var line struct {
				Message string `json:"msg"`
			}
			if json.Unmarshal(raw, &line) == nil && line.Message == "daemon: enrichment complete" {
				return nil
			}
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
		}
	}
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
	if !d.readyAndQuiet(roots...) {
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
	owner := &reconstructedDemandOwner{request: d.demand}
	initial := d.demand(reconstructedRoute(0))
	owner.records = append(owner.records, initial)
	measurementStart := initial.Start
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
		if err := os.WriteFile(filepath.Join(d.f.primary, reconstructedRouteFile), []byte(reconstructedRouteSource(reconstructedRoute(save))), 0600); err != nil {
			d.run.Error = err.Error()
			break
		}
		if save%7 == 0 || save == 150 {
			owner.tick(time.Now(), reconstructedRoute(save))
		}
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
	owner.collect()
	end := time.Now()
	evidence, err := reconstructedReadJobs(d.f.logPath(), measurementStart, end)
	if err != nil {
		d.run.Error = err.Error()
		return
	}
	d.run.Jobs = &evidence
	d.run.JobStatus, d.run.JobReason = evidence.Status, evidence.Reason
	d.recordDemand(owner, measurementStart, end)
	if len(owner.records) == 0 || owner.records[len(owner.records)-1].Route != reconstructedRoute(150) {
		d.run.JobStatus, d.run.JobReason = "insufficient", "final save contract demand tick was skipped"
	}
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
	samples, windows, err := reconstructedLoadWindows(func() reconstructedRebuild {
		ctx, cancel := context.WithTimeout(t.Context(), 900*time.Second)
		defer cancel()
		return reconstructedHTTPRebuild(ctx, "http://"+d.f.httpAddr, d.f.primary)
	}, func(index int) reconstructedSample { return d.search("load", index) })
	d.run.Samples = append(d.run.Samples, samples...)
	d.run.Rebuilds = windows
	if err != nil {
		d.run.Error = err.Error()
	}
}

// The one owner is always joined. An uncertain/failed terminal result ends the
// campaign, even if no search qualified for its window.
func reconstructedLoadWindows(load func() reconstructedRebuild, search func(int) reconstructedSample) ([]reconstructedSample, []reconstructedRebuild, error) {
	var samples []reconstructedSample
	var windows []reconstructedRebuild
	loaded, index := 0, 0
	for attempt := 0; attempt < 3 && loaded < 100; attempt++ {
		started := make(chan struct{})
		finished := make(chan reconstructedRebuild, 1)
		go func() { close(started); finished <- load() }()
		<-started
		first := len(samples)
		var window reconstructedRebuild
		var searchErr error
	searches:
		for len(samples)-first+loaded < 100 {
			select {
			case window = <-finished:
				break searches
			default:
			}
			sample := search(index)
			index++
			samples = append(samples, sample)
			if sample.Error != "" {
				searchErr = errors.New(sample.Error)
				break
			}
		}
		if window.End.IsZero() {
			window = <-finished
		}
		windows = append(windows, window)
		if window.Error != "" {
			return samples, windows, errors.New(window.Error)
		}
		if searchErr != nil {
			return samples, windows, searchErr
		}
		loaded = 0
		for _, sample := range samples {
			if reconstructedLoaded(sample, windows) {
				loaded++
			}
		}
	}
	return samples, windows, nil
}

func reconstructedHTTPRebuild(ctx context.Context, endpoint, repo string) (window reconstructedRebuild) {
	window.Start = time.Now()
	defer func() {
		window.End = time.Now()
		window.DurationMS = window.End.Sub(window.Start).Seconds() * 1000
	}()
	payload, err := json.Marshal(map[string]any{"arguments": map[string]string{"path": repo}})
	if err != nil {
		window.Error = err.Error()
		return
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint+"/v1/tools/index_repository", bytes.NewReader(payload))
	if err != nil {
		window.Error = err.Error()
		return
	}
	request.Header.Set("Content-Type", "application/json")
	// No proxy or socket transport: the fixture owns this loopback listener.
	client := &http.Client{Transport: &http.Transport{}}
	defer client.CloseIdleConnections()
	response, err := client.Do(request)
	if err != nil {
		window.Error = "index_repository: " + err.Error()
		return
	}
	raw, readErr := io.ReadAll(io.LimitReader(response.Body, (16<<20)+1))
	if len(raw) > 16<<20 {
		readErr = errors.New("terminal response exceeds 16 MiB")
	}
	window.Response = string(raw)
	err = errors.Join(readErr, response.Body.Close(), reconstructedTerminalResult(response.StatusCode, raw))
	if err != nil {
		window.Error = "index_repository terminal result: " + err.Error()
	}
	return
}

func reconstructedTerminalResult(status int, raw []byte) error {
	if status != http.StatusOK {
		return fmt.Errorf("HTTP status %d", status)
	}
	var result struct {
		Content []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"content"`
		IsError json.RawMessage `json:"isError"`
		Error   json.RawMessage `json:"error"`
	}
	if err := json.Unmarshal(raw, &result); err != nil {
		return err
	}
	if len(result.Error) != 0 || len(result.Content) == 0 {
		return errors.New("missing terminal tool content or error response")
	}
	if len(result.IsError) != 0 && string(result.IsError) != "false" {
		return errors.New("tool isError is true or malformed")
	}
	for _, content := range result.Content {
		if content.Type != "text" || content.Text == "" {
			return errors.New("malformed terminal tool content")
		}
	}
	return nil
}

func TestReconstructedFresh(t *testing.T) {
	d := newReconstructedDriver(t, "R-Fresh")
	start := time.Now()
	d.started = start
	d.f.start()
	d.run.Samples = append(d.run.Samples, d.probe(d.f.primary, d.fresh, issue767AsPrimary, start, 900*time.Second, "fresh"))
	if !d.awaitEnrichment() {
		return
	}
	d.run.Parameters["full_preparation_ms"] = time.Since(start).Seconds() * 1000
	size, err := reconstructedStoreBytes(d.f.store)
	if err != nil {
		d.run.Error = err.Error()
		return
	}
	d.run.Parameters["settled_store_plus_wal_bytes"] = size
}

func TestReconstructedRestart(t *testing.T) {
	d := newReconstructedDriver(t, "R-Restart")
	preparationStart := time.Now()
	if !d.readyAndQuiet() {
		return
	}
	attachment := d.demand(reconstructedRoute(0))
	d.run.Parameters["preparation_contract_demand"] = attachment
	if attachment.Error != "" {
		d.run.Error = "preparation contract attachment: " + attachment.Error
		return
	}
	// The required consumer may add work; re-establish quiet after attachment.
	d.run.Quiet = false
	if !d.quiet() {
		return
	}
	if err := reconstructedCleanPreparation(d.f.stopAndJoin(), d.f.forcedStops); err != nil {
		d.run.Error = err.Error()
		return
	}
	preparation, err := reconstructedReadJobs(d.f.logPath(), preparationStart, time.Now())
	if err != nil {
		d.run.Error = err.Error()
		return
	}
	d.run.Parameters["preparation_job_evidence"] = preparation
	before, err := reconstructedStoreBytes(d.f.store)
	if err != nil {
		d.run.Error = err.Error()
		return
	}
	start := time.Now()
	d.started = start
	d.f.start()
	owner := &reconstructedDemandOwner{request: d.demand}
	owner.tick(time.Now(), reconstructedRoute(0))
	// Keep interest alive while readiness and enrichment can take minutes.
	stopDemand := make(chan struct{})
	demandDone := make(chan struct{})
	go func() {
		defer close(demandDone)
		ticker := time.NewTicker(14 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-stopDemand:
				owner.collect()
				return
			case at := <-ticker.C:
				owner.tick(at, reconstructedRoute(0))
			}
		}
	}()
	var demandJoin sync.Once
	joinDemand := func() { demandJoin.Do(func() { close(stopDemand); <-demandDone }) }
	defer joinDemand()
	ready := d.probe(d.f.primary, d.fresh, issue767AsPrimary, start, time.Minute, "ready")
	d.run.Samples = append(d.run.Samples, ready)
	if !ready.Passed {
		d.run.Error = "restart ready probe failed: " + ready.Error
		return
	}
	if !d.awaitEnrichment() {
		return
	}
	enriched := time.Now()
	d.run.Parameters["restart_enrichment_ms"] = enriched.Sub(start).Seconds() * 1000
	timer := time.NewTimer(time.Until(enriched.Add(60 * time.Second)))
	select {
	case <-t.Context().Done():
		timer.Stop()
		d.run.Error = t.Context().Err().Error()
		return
	case <-timer.C:
	}
	joinDemand()
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	io, err := d.f.sampleProcessIO(ctx, d.f.pid())
	cancel()
	if err != nil {
		d.run.Error = "restart process I/O: " + err.Error()
		return
	}
	d.run.ProcessBytes = &io.BytesWritten
	after, err := reconstructedStoreBytes(d.f.store)
	if err != nil {
		d.run.Error = err.Error()
		return
	}
	growth := after - before
	d.run.StoreGrowth = &growth
	end := time.Now()
	d.run.Parameters["restart_measurement_start"], d.run.Parameters["restart_measurement_end"] = start, end
	d.run.Parameters["restart_sizes"] = reconstructedPathSizes(map[string]string{
		"store": d.f.store, "wal": d.f.store + "-wal", "log": d.f.logPath(), "cache": filepath.Join(d.f.root, "cache"),
	})
	evidence, err := reconstructedReadJobs(d.f.logPath(), start, end)
	if err != nil {
		d.run.Error = err.Error()
		return
	}
	d.run.Jobs = &evidence
	d.run.JobStatus, d.run.JobReason = "unsupported", "preparation did not establish positive job telemetry"
	if preparation.Positive && preparation.Status == "supported" {
		d.run.JobStatus, d.run.JobReason = evidence.Status, evidence.Reason
		if len(evidence.Events) == 0 {
			d.run.JobStatus = "supported"
		}
		d.run.JobCount = &evidence.Starts
	}
	d.recordDemand(owner, start, end)
	// Restart must still answer the unchanged graph query at the final boundary.
	answer := d.search("restart_final", 0)
	d.run.Parameters["restart_final_graph_query"] = answer
	if !answer.Passed || answer.Error != "" {
		d.run.Error = "restart final graph query failed: " + answer.Error
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

// Legacy phase-0 parser retained for the original scorer fixtures. New drivers
// use reconstructedReadJobs: they must prove pairing and preparation support.
// The production JSON zap encoder reports ts and elapsed in seconds.
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

const reconstructedRouteFile = "reconstructed_http/route.go"

func reconstructedRoute(save int) string { return fmt.Sprintf("/reconstructed-save-%03d", save) }

func reconstructedRouteSource(route string) string {
	return "package fixture\ntype Router interface { GET(string, func()) }\nfunc register(r Router) { r.GET(\"" + route + "\", users) }\nfunc users() {}\n"
}

type reconstructedDemand struct {
	Start      time.Time `json:"start"`
	End        time.Time `json:"end"`
	Route      string    `json:"expected_route"`
	Response   string    `json:"response"`
	DurationMS float64   `json:"client_duration_ms"`
	Error      string    `json:"error,omitempty"`
}

type reconstructedDemandOwner struct {
	request func(string) reconstructedDemand
	pending <-chan reconstructedDemand
	records []reconstructedDemand
	skipped []time.Time
}

func (o *reconstructedDemandOwner) collect() {
	if o.pending == nil {
		return
	}
	o.records = append(o.records, <-o.pending)
	o.pending = nil
}

func (o *reconstructedDemandOwner) tick(at time.Time, route string) {
	if o.pending != nil {
		select {
		case result := <-o.pending:
			o.records = append(o.records, result)
			o.pending = nil
		default:
		}
	}
	if o.pending != nil {
		o.skipped = append(o.skipped, at)
		return
	}
	result := make(chan reconstructedDemand, 1)
	o.pending = result
	go func() { result <- o.request(route) }()
}

func (d *reconstructedDriver) demand(route string) reconstructedDemand {
	sample := reconstructedDemand{Start: time.Now(), Route: route}
	raw, err := d.f.tryCommand(time.Minute, d.f.primary, "call", "contracts", "--arg", "action=list", "--arg", "type=http", "--arg", "limit=1000", "--format=json")
	sample.End, sample.Response = time.Now(), string(raw)
	sample.DurationMS = sample.End.Sub(sample.Start).Seconds() * 1000
	if err == nil {
		err = reconstructedContractAnswer(raw, route)
	}
	if err != nil {
		sample.Error = err.Error()
	}
	return sample
}

func reconstructedContractAnswer(raw []byte, route string) error {
	// `call --format=json` unwraps tool text into the contracts payload.
	var result struct {
		ByRepo map[string]struct {
			Contracts map[string][]struct {
				ID   string `json:"id"`
				Type string `json:"type"`
				Role string `json:"role"`
				File string `json:"file_path"`
			} `json:"contracts"`
		} `json:"by_repo"`
		Analysis *struct {
			State string `json:"state"`
		} `json:"contract_analysis"`
	}
	if err := json.Unmarshal(raw, &result); err != nil {
		return err
	}
	if result.Analysis != nil && result.Analysis.State != "complete" {
		return errors.New("contract attachment incomplete")
	}
	first := -1
	for save := 0; save <= 150; save++ {
		if reconstructedRoute(save) == route {
			first = save
			break
		}
	}
	if first < 0 {
		return fmt.Errorf("unknown requested route %q", route)
	}
	for _, repo := range result.ByRepo {
		for _, contract := range repo.Contracts["http"] {
			if contract.Type != "http" || contract.Role != "provider" || !strings.HasSuffix(contract.File, "/"+reconstructedRouteFile) {
				continue
			}
			// Saves continue during demand. A newer route is fresh too.
			for save := first; save <= 150; save++ {
				if contract.ID == "http::GET::"+reconstructedRoute(save) {
					return nil
				}
			}
		}
	}
	return fmt.Errorf("missing current route %s from %s", route, reconstructedRouteFile)
}

func reconstructedDemandGaps(records []reconstructedDemand, start, end time.Time) ([]float64, bool) {
	previous := start
	var gaps []float64
	insufficient := len(records) == 0
	for _, record := range records {
		gap := record.Start.Sub(previous).Seconds()
		gaps = append(gaps, gap)
		if gap >= 60 || gap < 0 {
			insufficient = true
		}
		previous = record.Start
	}
	gap := end.Sub(previous).Seconds()
	gaps = append(gaps, gap)
	return gaps, insufficient || gap >= 60 || gap < 0
}

func (d *reconstructedDriver) recordDemand(o *reconstructedDemandOwner, start, end time.Time) {
	gaps, insufficient := reconstructedDemandGaps(o.records, start, end)
	d.run.Parameters["contract_demand"] = o.records
	d.run.Parameters["contract_demand_skipped_ticks"] = o.skipped
	d.run.Parameters["contract_demand_gaps_s"] = gaps
	if insufficient {
		d.run.JobStatus, d.run.JobReason = "insufficient", "contract demand interest gap >=60 s or absent demand"
	}
	for _, record := range o.records {
		if record.Error != "" {
			d.run.Error = "contract demand: " + record.Error
			return
		}
	}
}

type reconstructedJobEvent struct {
	Message    string   `json:"msg"`
	Time       float64  `json:"ts"`
	Job        string   `json:"job,omitempty"`
	Repo       string   `json:"repo,omitempty"`
	Checkout   string   `json:"checkout,omitempty"`
	Mode       string   `json:"mode"`
	Outcome    string   `json:"outcome,omitempty"`
	Elapsed    *float64 `json:"elapsed,omitempty"`
	QueueMS    *float64 `json:"since_trigger_ms,omitempty"`
	AcceptedMS *float64 `json:"since_accept_ms,omitempty"`
	CaptureMS  *float64 `json:"capture_ms,omitempty"`
	DispatchMS *float64 `json:"dispatch_ms,omitempty"`
}

type reconstructedJobEvidence struct {
	Events   []reconstructedJobEvent `json:"events"`
	Starts   int                     `json:"starts"`
	BodyMS   map[string][]float64    `json:"body_ms_by_mode,omitempty"`
	Status   string                  `json:"status"`
	Reason   string                  `json:"reason,omitempty"`
	Positive bool                    `json:"positive_attachment_telemetry"`
}

func reconstructedReadJobs(path string, start, end time.Time) (reconstructedJobEvidence, error) {
	file, err := os.Open(path)
	if err != nil {
		return reconstructedJobEvidence{}, err
	}
	defer file.Close()
	scanner := bufio.NewScanner(file)
	scanner.Buffer(make([]byte, 4096), 4<<20)
	var events []reconstructedJobEvent
	for scanner.Scan() {
		var event reconstructedJobEvent
		if json.Unmarshal(scanner.Bytes(), &event) != nil {
			continue
		}
		if event.Message != "contract analysis job started" && event.Message != "contract analysis job" {
			continue
		}
		at := time.Unix(0, int64(event.Time*1e9))
		if !at.Before(start) && !at.After(end) {
			events = append(events, event)
		}
	}
	if err := scanner.Err(); err != nil {
		return reconstructedJobEvidence{}, err
	}
	return reconstructedPairJobs(events), nil
}

func reconstructedPairJobs(events []reconstructedJobEvent) reconstructedJobEvidence {
	evidence := reconstructedJobEvidence{Events: events, Status: "unsupported", BodyMS: map[string][]float64{}}
	active := 0
	ambiguous := false
	for _, event := range events {
		evidence.Status = "supported"
		if event.Mode != "cold" && event.Mode != "fill" && event.Mode != "delta" {
			ambiguous = true
		}
		if event.Message == "contract analysis job started" {
			evidence.Starts++
			active++
			if active > 1 || event.Job == "" || event.Repo == "" {
				ambiguous = true
			}
			continue
		}
		if active != 1 || event.Elapsed == nil || (event.Elapsed != nil && *event.Elapsed < 0) || event.Outcome == "" {
			ambiguous = true
		}
		if !ambiguous && event.Elapsed != nil {
			evidence.BodyMS[event.Mode] = append(evidence.BodyMS[event.Mode], *event.Elapsed*1000)
			if event.Outcome == "published" {
				evidence.Positive = true
			}
		}
		if active > 0 {
			active--
		}
	}
	if ambiguous || active != 0 {
		evidence.Status, evidence.Reason = "insufficient", "ambiguous, malformed or unfinished contract job pairing"
		evidence.BodyMS, evidence.Positive = nil, false
	}
	return evidence
}

func reconstructedCleanPreparation(joined bool, forcedStops int) error {
	if !joined {
		return errors.New("initial daemon did not join before restart")
	}
	if forcedStops != 0 {
		return errors.New("initial daemon did not stop cleanly before restart")
	}
	return nil
}

// These are same-tick sizes, not cumulative process-write attribution.
func reconstructedPathSizes(paths map[string]string) map[string]any {
	values := map[string]any{"observed_at": time.Now()}
	for name, path := range paths {
		var size int64
		err := filepath.WalkDir(path, func(_ string, entry fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if entry.Type().IsRegular() {
				info, err := entry.Info()
				if err != nil {
					return err
				}
				size += info.Size()
			}
			return nil
		})
		if err != nil {
			values[name] = map[string]string{"unavailable": err.Error()}
			continue
		}
		values[name] = map[string]int64{"bytes": size}
	}
	values["finished_at"] = time.Now()
	return values
}
