//go:build acceptance

package main

import (
	"encoding/json"
	"math"
	"os"
	"slices"
	"strings"
	"testing"
	"time"
)

func reconstructedCaseSamples(kind string, values ...float64) []reconstructedSample {
	samples := make([]reconstructedSample, len(values))
	for i, value := range values {
		samples[i] = reconstructedSample{Kind: kind, DurationMS: value, Passed: true}
	}
	return samples
}

func TestReconstructedScorer(t *testing.T) {
	// The expected ranks are written out here, not obtained from the scorer:
	// 1..20 -> ranks 10 and 19. 2,4,..60 -> rank 29 = 58 at p95.
	var twenty, thirty []float64
	for i := 1; i <= 20; i++ {
		twenty = append(twenty, float64(i))
	}
	for i := 1; i <= 30; i++ {
		thirty = append(thirty, float64(2*i))
	}
	window := reconstructedRebuild{Start: time.Unix(100, 0), End: time.Unix(200, 0), DurationMS: 100000}
	fill := func(idle, loaded []float64) reconstructedRun {
		run := reconstructedRun{Label: "reconstructed", Workload: "R-Fill", Quiet: true, Rebuilds: []reconstructedRebuild{window}}
		run.Samples = reconstructedCaseSamples("idle", idle...)
		for _, value := range loaded {
			run.Samples = append(run.Samples, reconstructedSample{Kind: "load", Start: time.Unix(110, 0), End: time.Unix(111, 0), DurationMS: value, Passed: true})
		}
		return run
	}
	thresholdLoad := make([]float64, 30)
	for i := range thresholdLoad {
		thresholdLoad[i] = 200
	}
	thresholdFill := fill([]float64{100}, thresholdLoad)
	outsideFill := fill([]float64{100}, thresholdLoad)
	for _, bounds := range [][2]int64{{90, 99}, {201, 202}, {99, 101}, {199, 201}} {
		outsideFill.Samples = append(outsideFill.Samples, reconstructedSample{Kind: "load", Start: time.Unix(bounds[0], 0), End: time.Unix(bounds[1], 0), DurationMS: 999999, Passed: true})
	}
	erroredFill := fill([]float64{100}, thresholdLoad)
	erroredFill.Rebuilds = []reconstructedRebuild{{Start: window.Start, End: window.End, DurationMS: window.DurationMS, Error: "index_repository: deadline"}}
	timeoutSamples := reconstructedCaseSamples("probe", 1, 2, 3, 4, 5, 6, 7, 8, 9)
	timeoutSamples = append(timeoutSamples, reconstructedSample{Kind: "probe", DurationMS: 1, TimeoutMS: 60000, TimedOut: true})
	allEdits := reconstructedCaseSamples("probe", twenty...)
	allEdits[len(allEdits)-1].DurationMS = 5001 // max fails even though p95 is 19.
	zeroJobs, oneJob := 0, 1
	processBytes, growth := uint64(12345), int64(678)
	tests := []struct {
		name      string
		run       reconstructedRun
		status    string
		p50, p95  float64
		job       string
		jobP95    float64
		ratio     float64
		loaded    int
		excluded  int
		timeouts  int
		failures  int
		errorText string
	}{
		{name: "nearest_rank_percentiles", run: reconstructedRun{Workload: "R-DE", Samples: reconstructedCaseSamples("probe", twenty...), JobMS: twenty}, status: "pass", p50: 10, p95: 19, job: "supported", jobP95: 19},
		{name: "C2_exact_threshold", run: reconstructedRun{Workload: "R-C2", Samples: reconstructedCaseSamples("probe", 5000)}, status: "pass", p50: 5000, p95: 5000},
		{name: "C2_every_edit_not_p95", run: reconstructedRun{Workload: "R-C2", Samples: allEdits}, status: "fail", p50: 10, p95: 19, failures: 1},
		{name: "DE_exact_threshold_job_only", run: reconstructedRun{Workload: "R-DE", Samples: reconstructedCaseSamples("probe", 60000), JobMS: []float64{700}}, status: "pass", p50: 60000, p95: 60000, job: "supported", jobP95: 700},
		{name: "DE_above_threshold", run: reconstructedRun{Workload: "R-DE", Samples: reconstructedCaseSamples("probe", 1), JobMS: []float64{701}}, status: "fail", p50: 1, p95: 1, job: "supported", jobP95: 701},
		{name: "Fill_hand_computed_ratio", run: fill(twenty, thirty), status: "fail", p50: 17, p95: 56, ratio: 58.0 / 19.0, loaded: 30},
		{name: "Fill_exact_threshold", run: thresholdFill, status: "pass", p50: 200, p95: 200, ratio: 2, loaded: 30},
		{name: "Fill_outside_windows_excluded", run: outsideFill, status: "pass", p50: 200, p95: 200, ratio: 2, loaded: 30, excluded: 4},
		{name: "Fill_fewer_than_30_insufficient", run: fill([]float64{100}, thresholdLoad[:29]), status: "insufficient", p50: 200, p95: 200, loaded: 29},
		{name: "Fresh_exact_threshold", run: reconstructedRun{Workload: "R-Fresh", Samples: reconstructedCaseSamples("fresh", 180000)}, status: "pass", p50: 180000, p95: 180000},
		{name: "Fresh_above_threshold", run: reconstructedRun{Workload: "R-Fresh", Samples: reconstructedCaseSamples("fresh", 180001)}, status: "fail", p50: 180001, p95: 180001, failures: 1},
		{name: "timeout_counts_at_60_seconds", run: reconstructedRun{Workload: "R-C2", Samples: timeoutSamples}, status: "fail", p50: 5, p95: 60000, timeouts: 1, failures: 1},
		{name: "Fresh_timeout_counts_at_900_seconds", run: reconstructedRun{Workload: "R-Fresh", Samples: []reconstructedSample{{Kind: "fresh", TimedOut: true, TimeoutMS: 900000, DurationMS: 1}}}, status: "fail", p50: 900000, p95: 900000, timeouts: 1, failures: 1},
		{name: "missing_telemetry_unsupported", run: reconstructedRun{Workload: "R-DE", Samples: reconstructedCaseSamples("probe", 10)}, status: "unsupported", p50: 10, p95: 10, job: "unsupported"},
		{name: "Restart_exact_threshold_zero_jobs", run: reconstructedRun{Workload: "R-Restart", Samples: reconstructedCaseSamples("ready", 100), ProcessBytes: &processBytes, StoreGrowth: &growth, JobCount: &zeroJobs}, status: "pass", p50: 100, p95: 100, job: "supported"},
		{name: "Restart_nonzero_jobs", run: reconstructedRun{Workload: "R-Restart", Samples: reconstructedCaseSamples("ready", 100), ProcessBytes: &processBytes, StoreGrowth: &growth, JobCount: &oneJob}, status: "fail", p50: 100, p95: 100, job: "supported"},
		{name: "Restart_missing_telemetry_unsupported", run: reconstructedRun{Workload: "R-Restart", Samples: reconstructedCaseSamples("ready", 100), ProcessBytes: &processBytes, StoreGrowth: &growth}, status: "unsupported", p50: 100, p95: 100},
		{name: "empty_sample_set_error", run: reconstructedRun{Workload: "R-C2"}, errorText: "empty sample set"},
		{name: "Fill_rebuild_error_not_insufficient", run: erroredFill, status: "fail", p50: 100, p95: 100, excluded: 30},
		{name: "driver_error_without_samples_fails", run: reconstructedRun{Workload: "R-C2", Error: "ready probe failed"}, status: "fail"},
		{name: "failed_call_probe_counts_at_60_seconds", run: reconstructedRun{Workload: "R-C2", Samples: append(reconstructedCaseSamples("probe", 1, 2, 3, 4, 5, 6, 7, 8, 9), reconstructedSample{Kind: "probe", DurationMS: 60000, TimeoutMS: 60000, TimedOut: true, Error: "search: context deadline exceeded"})}, status: "fail", p50: 5, p95: 60000, timeouts: 1, failures: 1},
		{name: "invalid_duration_error", run: reconstructedRun{Workload: "R-C2", Samples: reconstructedCaseSamples("probe", -1)}, errorText: "invalid sample duration"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			tc.run.Label, tc.run.Quiet = "reconstructed", true
			got, err := scoreReconstructed(tc.run)
			if tc.errorText != "" {
				if err == nil || !strings.Contains(err.Error(), tc.errorText) {
					t.Fatalf("error = %v, want %q", err, tc.errorText)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			job := tc.job
			if job == "" {
				job = "unsupported"
			}
			if got.Label != "reconstructed" || got.Status != tc.status || got.ClientP50MS != tc.p50 || got.ClientP95MS != tc.p95 || got.Job != job || got.Loaded != tc.loaded || got.Excluded != tc.excluded || got.Timeouts != tc.timeouts || len(got.Failures) != tc.failures {
				t.Fatalf("score = %+v; want status=%s p50=%v p95=%v job=%s loaded=%d excluded=%d timeouts=%d failures=%d", got, tc.status, tc.p50, tc.p95, job, tc.loaded, tc.excluded, tc.timeouts, tc.failures)
			}
			for _, sample := range got.Failures {
				if sample.TimedOut && sample.DurationMS != sample.TimeoutMS {
					t.Fatalf("timed-out failure duration = %v, want %v", sample.DurationMS, sample.TimeoutMS)
				}
			}
			if tc.jobP95 != 0 && (got.JobP95MS == nil || *got.JobP95MS != tc.jobP95) {
				t.Fatalf("job p95 = %v, want %v", got.JobP95MS, tc.jobP95)
			}
			if job == "unsupported" && (got.JobP95MS != nil || got.JobCount != nil) {
				t.Fatal("unsupported telemetry must not contain a numeric zero")
			}
			if tc.ratio != 0 && (got.Ratio == nil || math.Abs(*got.Ratio-tc.ratio) > 1e-12) {
				t.Fatalf("ratio = %v, want %v", got.Ratio, tc.ratio)
			}
			if tc.run.Workload == "R-Restart" && tc.run.JobCount != nil && (got.JobCount == nil || *got.JobCount != *tc.run.JobCount) {
				t.Fatalf("restart job count = %v", got.JobCount)
			}
			// Exercise the real JSONL boundary, not just direct struct calls.
			raw, err := json.Marshal(tc.run)
			if err != nil {
				t.Fatal(err)
			}
			runs, err := readReconstructed(strings.NewReader(string(raw) + "\n"))
			if err != nil || len(runs) != 1 || runs[0].Score.Label != "reconstructed" || runs[0].Score.Status != tc.status {
				t.Fatalf("JSONL round trip: runs=%+v error=%v", runs, err)
			}
		})
	}
	t.Run("job_log_duration_units_and_count", func(t *testing.T) {
		for _, tc := range []struct {
			name, log    string
			count        int
			supported    bool
			milliseconds []float64
		}{
			{name: "missing_telemetry", log: "not JSON\n{\"msg\":\"another message\",\"ts\":150}\n"},
			{name: "duration_seconds_to_ms", log: "{\"msg\":\"contract analysis job\",\"ts\":150,\"elapsed\":0.7}\n", count: 1, supported: true, milliseconds: []float64{700}},
			{name: "in_flight_job_counts", log: "{\"msg\":\"contract analysis job started\",\"ts\":150}\n", count: 1, supported: true},
			{name: "start_and_end_count_once", log: "{\"msg\":\"contract analysis job started\",\"ts\":149}\n{\"msg\":\"contract analysis job\",\"ts\":150,\"elapsed\":0.7}\n", count: 1, supported: true, milliseconds: []float64{700}},
			{name: "outside_measurement_window", log: "{\"msg\":\"contract analysis job started\",\"ts\":90}\n{\"msg\":\"contract analysis job\",\"ts\":91,\"elapsed\":0.7}\n", supported: true},
			{name: "missing_duration_does_not_become_zero", log: "{\"msg\":\"contract analysis job\",\"ts\":150}\n", count: 1, supported: true},
		} {
			t.Run(tc.name, func(t *testing.T) {
				path := t.TempDir() + "/jobs.log"
				if err := os.WriteFile(path, []byte(tc.log), 0600); err != nil {
					t.Fatal(err)
				}
				ms, count, supported, err := reconstructedJobs(path, time.Unix(100, 0), time.Unix(200, 0))
				if err != nil || count != tc.count || supported != tc.supported || len(ms) != len(tc.milliseconds) {
					t.Fatalf("durations=%v count=%d supported=%v error=%v", ms, count, supported, err)
				}
				for i, value := range ms {
					if value != tc.milliseconds[i] {
						t.Fatalf("job duration = %v, want %v", value, tc.milliseconds[i])
					}
				}
			})
		}
	})
	t.Run("quiet_never_arrived_insufficient", func(t *testing.T) {
		got, err := scoreReconstructed(reconstructedRun{Label: "reconstructed", Workload: "R-Fill"})
		if err != nil || got.Status != "insufficient" {
			t.Fatalf("score=%+v error=%v", got, err)
		}
	})
	for name, input := range map[string]string{"empty_result_set_error": "", "malformed_JSON_error": "{", "unlabelled_result_error": `{"workload":"R-Fresh"}`} {
		t.Run(name, func(t *testing.T) {
			if _, err := readReconstructed(strings.NewReader(input)); err == nil {
				t.Fatal("invalid input must be an error, never a pass")
			}
		})
	}
}

func TestReconstructedSearchSymbols(t *testing.T) {
	declarations := map[string][]string{
		"a/agents.go":       {"Adapter", "Run"},
		"b/adapter.go":      {"Adapter", "Build"},
		"b/adapter_test.go": {"Build"},
	}
	got := reconstructedSearchSymbols([]string{"a/agents.go", "b/adapter.go"}, declarations, 2)
	want := []reconstructedSymbol{{Name: "Run", File: "a/agents.go"}}
	if !slices.Equal(got, want) {
		t.Fatalf("reconstructedSearchSymbols() = %v; want %v", got, want)
	}
}

// Fork CI can rescore a saved JSONL file without starting any daemon.
func TestReconstructedScorerFile(t *testing.T) {
	input := os.Getenv("GORTEX_RECON_SCORE_INPUT")
	if input == "" {
		t.Skip("set GORTEX_RECON_SCORE_INPUT to rescore saved JSONL")
	}
	f, err := os.Open(input)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	runs, err := readReconstructed(f)
	if err != nil {
		t.Fatal(err)
	}
	for _, run := range runs {
		raw, err := json.Marshal(run.Score)
		if err != nil {
			t.Fatal(err)
		}
		t.Log(string(raw))
	}
}
