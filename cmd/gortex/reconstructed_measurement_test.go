//go:build acceptance

package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestReconstructedMeasurementWarmupOrder(t *testing.T) {
	for _, enriched := range []bool{false, true} {
		var order []string
		got := reconstructedPrepare(func() bool { order = append(order, "enrichment"); return enriched },
			func() bool { order = append(order, "views"); return true },
			func() bool { order = append(order, "quiet"); return true })
		want := []string{"enrichment"}
		if enriched {
			want = append(want, "views", "quiet")
		}
		if got != enriched || !reflect.DeepEqual(order, want) {
			t.Fatalf("prepared=%v order=%v want %v", got, order, want)
		}
	}
}

func TestReconstructedMeasurementEnrichment(t *testing.T) {
	for _, tc := range []struct {
		name, log string
		pass      bool
	}{
		{"ready_is_not_enriched", `{"msg":"daemon: graph queryable","ready":true}`, false},
		{"successful_search_is_not_enriched", `{"msg":"search succeeded"}`, false},
		{"partial_log_is_not_enriched", `{"msg":"daemon: enrichment complete`, false},
		{"full_tail", `{"msg":"daemon: enrichment complete"}`, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "daemon.log")
			if err := os.WriteFile(path, []byte(tc.log+"\n"), 0600); err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			if !tc.pass {
				cancel()
			}
			err := reconstructedWaitEnrichment(ctx, path)
			if (err == nil) != tc.pass {
				t.Fatalf("error=%v pass=%v", err, tc.pass)
			}
		})
	}
	t.Run("expired_full_warmup_deadline", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "daemon.log")
		if err := os.WriteFile(path, []byte(`{"msg":"daemon: enrichment complete"}`), 0600); err != nil {
			t.Fatal(err)
		}
		ctx, cancel := context.WithDeadline(t.Context(), time.Unix(1, 0))
		defer cancel()
		if err := reconstructedWaitEnrichment(ctx, path); err == nil {
			t.Fatal("expired warm-up deadline accepted late completion")
		}
	})
}

func TestReconstructedMeasurementTerminalResult(t *testing.T) {
	for _, tc := range []struct {
		name, raw string
		status    int
		pass      bool
	}{
		{"terminal_success", `{"content":[{"type":"text","text":"Indexed and analyzed"}]}`, 200, true},
		{"explicit_false", `{"content":[{"type":"text","text":"Indexed"}],"isError":false}`, 200, true},
		{"HTTP_200_tool_error", `{"content":[{"type":"text","text":"failed"}],"isError":true}`, 200, false},
		{"HTTP_200_without_terminal", `{"status":"accepted"}`, 200, false},
		{"missing_content", `{"isError":false}`, 200, false},
		{"empty_content", `{"content":[],"isError":false}`, 200, false},
		{"null", `null`, 200, false},
		{"malformed_JSON", `{`, 200, false},
		{"trailing_JSON", `{"content":[{"type":"text","text":"ok"}]} {}`, 200, false},
		{"malformed_error_flag", `{"content":[{"type":"text","text":"ok"}],"isError":"false"}`, 200, false},
		{"null_error_flag", `{"content":[{"type":"text","text":"ok"}],"isError":null}`, 200, false},
		{"malformed_content", `{"content":[{}]}`, 200, false},
		{"HTTP_error", `{"content":[{"type":"text","text":"ok"}]}`, 500, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method != "POST" || r.URL.Path != "/v1/tools/index_repository" {
					t.Errorf("request=%s %s", r.Method, r.URL.Path)
				}
				var body struct {
					Arguments struct {
						Path string `json:"path"`
					} `json:"arguments"`
				}
				if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body.Arguments.Path != "/private/repo" {
					t.Errorf("body=%+v error=%v", body, err)
				}
				w.WriteHeader(tc.status)
				_, _ = w.Write([]byte(tc.raw))
			}))
			defer server.Close()
			got := reconstructedHTTPRebuild(t.Context(), server.URL, "/private/repo")
			if (got.Error == "") != tc.pass || got.Response != tc.raw || !got.End.After(got.Start) {
				t.Fatalf("window=%+v pass=%v", got, tc.pass)
			}
		})
	}
	t.Run("headers_do_not_close_window", func(t *testing.T) {
		headers, release := make(chan struct{}), make(chan struct{})
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(200)
			w.(http.Flusher).Flush()
			close(headers)
			<-release
			_, _ = w.Write([]byte(`{"content":[{"type":"text","text":"complete"}]}`))
		}))
		defer server.Close()
		done := make(chan reconstructedRebuild, 1)
		go func() { done <- reconstructedHTTPRebuild(t.Context(), server.URL, "/private/repo") }()
		<-headers
		select {
		case result := <-done:
			close(release)
			t.Fatalf("premature terminal result %+v", result)
		default:
		}
		close(release)
		if result := <-done; result.Error != "" {
			t.Fatal(result.Error)
		}
	})
}

func TestReconstructedMeasurementOversizedTerminal(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"content":[{"type":"text","text":"complete"}]}` + strings.Repeat(" ", 16<<20)))
	}))
	defer server.Close()
	result := reconstructedHTTPRebuild(t.Context(), server.URL, "/private/repo")
	if !strings.Contains(result.Error, "exceeds 16 MiB") {
		t.Fatalf("error=%q response bytes=%d", result.Error, len(result.Response))
	}
}

func TestReconstructedMeasurementLoadNoReplay(t *testing.T) {
	for _, failure := range []string{"uncertain_timeout", "tool_error", "malformed_terminal"} {
		t.Run(failure, func(t *testing.T) {
			var requests atomic.Int32
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				requests.Add(1)
				switch failure {
				case "uncertain_timeout":
					cancel()
				case "tool_error":
					_, _ = w.Write([]byte(`{"content":[{"type":"text","text":"error"}],"isError":true}`))
				case "malformed_terminal":
					_, _ = w.Write([]byte(`{"status":"accepted"}`))
				}
			}))
			defer server.Close()
			_, windows, err := reconstructedLoadWindows(func() reconstructedRebuild { return reconstructedHTTPRebuild(ctx, server.URL, "/private/repo") },
				func(int) reconstructedSample {
					return reconstructedSample{Kind: "load", Start: time.Now(), End: time.Now(), Passed: true}
				})
			if err == nil || len(windows) != 1 || requests.Load() != 1 || windows[0].Error == "" {
				t.Fatalf("requests=%d windows=%+v error=%v", requests.Load(), windows, err)
			}
		})
	}
}

func TestReconstructedMeasurementDemandOwner(t *testing.T) {
	entered, release := make(chan struct{}), make(chan struct{})
	var calls atomic.Int32
	owner := &reconstructedDemandOwner{request: func(route string) reconstructedDemand {
		calls.Add(1)
		close(entered)
		<-release
		return reconstructedDemand{Route: route, Start: time.Unix(100, 0), End: time.Unix(110, 0)}
	}}
	owner.tick(time.Unix(100, 0), "/first")
	<-entered
	owner.tick(time.Unix(114, 0), "/second")
	owner.tick(time.Unix(128, 0), "/third")
	if calls.Load() != 1 || len(owner.skipped) != 2 {
		t.Fatalf("calls=%d skipped=%v", calls.Load(), owner.skipped)
	}
	close(release)
	owner.collect()
	if owner.pending != nil || len(owner.records) != 1 || owner.records[0].Route != "/first" {
		t.Fatalf("owner=%+v", owner)
	}
}

func TestReconstructedMeasurementDemandGaps(t *testing.T) {
	for _, tc := range []struct {
		name         string
		starts       []int64
		end          int64
		insufficient bool
	}{
		{"continuous", []int64{100, 114, 128, 142}, 155, false},
		{"exact_interest_expiry", []int64{100, 160}, 161, true},
		{"end_gap", []int64{100}, 160, true},
		{"absent", nil, 110, true},
		{"out_of_order", []int64{114, 100}, 115, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var records []reconstructedDemand
			for _, at := range tc.starts {
				records = append(records, reconstructedDemand{Start: time.Unix(at, 0)})
			}
			_, got := reconstructedDemandGaps(records, time.Unix(100, 0), time.Unix(tc.end, 0))
			if got != tc.insufficient {
				t.Fatalf("insufficient=%v want=%v", got, tc.insufficient)
			}
		})
	}
}

func TestReconstructedMeasurementContractAnswer(t *testing.T) {
	good := `{"by_repo":{"issue767":{"contracts":{"http":[{"id":"http::GET::/reconstructed-save-007","type":"http","role":"provider","file_path":"issue767/reconstructed_http/route.go"}]}}},"contract_analysis":{"state":"complete"}}`
	for _, tc := range []struct {
		name, raw string
		pass      bool
	}{
		{"current", good, true},
		{"stale", strings.ReplaceAll(good, "save-007", "save-006"), false},
		{"newer_while_saves_continue", strings.ReplaceAll(good, "save-007", "save-008"), true},
		{"incomplete", strings.ReplaceAll(good, "complete", "incomplete"), false},
		{"wrong_file", strings.ReplaceAll(good, "reconstructed_http/route.go", "unrelated.go"), false},
		{"not_a_contract", `{"message":"/reconstructed-save-007"}`, false},
		{"empty", `{}`, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if err := reconstructedContractAnswer([]byte(tc.raw), reconstructedRoute(7)); (err == nil) != tc.pass {
				t.Fatalf("error=%v pass=%v", err, tc.pass)
			}
		})
	}
}

func TestReconstructedMeasurementJobEvidence(t *testing.T) {
	elapsed := 0.7
	start := reconstructedJobEvent{Message: "contract analysis job started", Job: "one", Repo: "issue767", Mode: "cold"}
	finish := reconstructedJobEvent{Message: "contract analysis job", Mode: "cold", Outcome: "published", Elapsed: &elapsed}
	second := start
	second.Job = "two"
	failed := finish
	failed.Outcome = "failed"
	for _, tc := range []struct {
		name           string
		events         []reconstructedJobEvent
		status         string
		positive       bool
		starts, bodies int
	}{
		{"absent_is_unsupported", nil, "unsupported", false, 0, 0},
		{"positive_attachment", []reconstructedJobEvent{start, finish}, "supported", true, 1, 1},
		{"failed_body_retained", []reconstructedJobEvent{start, failed}, "supported", false, 1, 1},
		{"unfinished", []reconstructedJobEvent{start}, "insufficient", false, 1, 0},
		{"orphan_finish", []reconstructedJobEvent{finish}, "insufficient", false, 0, 0},
		{"ambiguous_overlap", []reconstructedJobEvent{start, second, finish, finish}, "insufficient", false, 2, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := reconstructedPairJobs(tc.events)
			if got.Status != tc.status || got.Positive != tc.positive || got.Starts != tc.starts || len(got.BodyMS["cold"]) != tc.bodies || len(got.Events) != len(tc.events) {
				t.Fatalf("evidence=%+v", got)
			}
		})
	}
	t.Run("modes_not_pooled", func(t *testing.T) {
		deltaStart, deltaFinish := start, finish
		deltaStart.Mode, deltaFinish.Mode = "delta", "delta"
		got := reconstructedPairJobs([]reconstructedJobEvent{start, finish, deltaStart, deltaFinish})
		if len(got.BodyMS) != 2 || got.BodyMS["cold"][0] != 700 || got.BodyMS["delta"][0] != 700 {
			t.Fatalf("bodies=%v", got.BodyMS)
		}
	})
}

func TestReconstructedMeasurementForcedPreparation(t *testing.T) {
	for _, tc := range []struct {
		joined bool
		forced int
		pass   bool
	}{{true, 0, true}, {true, 1, false}, {false, 0, false}} {
		starts := 0
		if reconstructedCleanPreparation(tc.joined, tc.forced) == nil {
			starts++
		}
		if (starts == 1) != tc.pass {
			t.Fatalf("joined=%v forced=%d second starts=%d", tc.joined, tc.forced, starts)
		}
	}
}

func TestReconstructedMeasurementScorerErrors(t *testing.T) {
	for _, run := range []reconstructedRun{
		{Label: "reconstructed", Workload: "R-Restart", Quiet: true, Error: "forced preparation stop", Samples: []reconstructedSample{{Passed: true}}},
		{Label: "reconstructed", Workload: "R-Fill", Quiet: true, Error: "uncertain load", Samples: []reconstructedSample{{Kind: "load", Passed: true}}},
		{Label: "reconstructed", Workload: "R-DE", Quiet: true, Error: "contract answer failed", JobStatus: "insufficient", Samples: []reconstructedSample{{Passed: true}}},
	} {
		got, err := scoreReconstructed(run)
		if err != nil || got.Status != "fail" || got.Reason != run.Error {
			t.Fatalf("score=%+v error=%v", got, err)
		}
	}
	if _, err := scoreReconstructed(reconstructedRun{Label: "reconstructed", Workload: "R-Fresh"}); err == nil {
		t.Fatal("unexplained empty set passed")
	}
}

func TestReconstructedMeasurementScorerJobs(t *testing.T) {
	for _, status := range []string{"unsupported", "insufficient"} {
		run := reconstructedRun{Label: "reconstructed", Workload: "R-DE", Quiet: true, JobStatus: status, JobReason: "no complete telemetry", Samples: []reconstructedSample{{Passed: true, DurationMS: 1}}}
		score, err := scoreReconstructed(run)
		if err != nil || score.Status != status || score.Job != status || score.JobP95MS != nil || score.JobP95ByMode != nil {
			t.Fatalf("score=%+v error=%v", score, err)
		}
	}
	run := reconstructedRun{Label: "reconstructed", Workload: "R-DE", Quiet: true, JobStatus: "supported", Jobs: &reconstructedJobEvidence{BodyMS: map[string][]float64{"cold": {700}, "delta": {900}}}, Samples: []reconstructedSample{{Passed: true, DurationMS: 1}}}
	score, err := scoreReconstructed(run)
	if err != nil || score.Status != "fail" || score.JobP95MS != nil || score.JobP95ByMode["cold"] != 700 || score.JobP95ByMode["delta"] != 900 {
		t.Fatalf("score=%+v error=%v", score, err)
	}
}

func TestReconstructedMeasurementPathSizes(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "data"), []byte("abc"), 0600); err != nil {
		t.Fatal(err)
	}
	got := reconstructedPathSizes(map[string]string{"cache": dir, "missing": filepath.Join(dir, "absent")})
	if got["cache"].(map[string]int64)["bytes"] != 3 || got["missing"].(map[string]string)["unavailable"] == "" {
		t.Fatalf("sizes=%v", got)
	}
}
