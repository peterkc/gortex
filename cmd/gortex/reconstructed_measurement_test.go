//go:build acceptance

package main

import (
	"context"
	"encoding/json"
	"errors"
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

func TestReconstructedMeasurementDeadlines(t *testing.T) {
	for _, tc := range []struct {
		name      string
		got, want time.Duration
	}{
		{"warmup", reconstructedSetupReadyTimeout, 1800 * time.Second},
		{"rebuild", reconstructedRebuildTimeout, 1800 * time.Second},
		{"fresh_first_answer", reconstructedFreshTimeout, 900 * time.Second},
		{"quiet", reconstructedQuietTimeout, 300 * time.Second},
		{"cleanup", issue767StopTimeout, 30 * time.Second},
		{"probe", reconstructedProbeTimeout, 60 * time.Second},
		{"search", issue767SearchTimeout, 10 * time.Second},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if tc.got != tc.want {
				t.Fatalf("deadline = %s, want %s", tc.got, tc.want)
			}
		})
	}
}

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
			ctx, cancel := context.WithTimeout(t.Context(), 300*time.Millisecond)
			defer cancel()
			err := reconstructedWaitEnrichment(ctx, path)
			if !tc.pass && !errors.Is(err, context.DeadlineExceeded) {
				t.Fatalf("error=%v, want live deadline exceeded", err)
			}
			if (err == nil) != tc.pass {
				t.Fatalf("error=%v pass=%v", err, tc.pass)
			}
		})
	}
	t.Run("incremental_partial_line", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "daemon.log")
		if err := os.WriteFile(path, []byte("{\"msg\":\"daemon: graph queryable\"}\n{\"msg\":\"daemon: enrichment "), 0600); err != nil {
			t.Fatal(err)
		}
		ctx, cancel := context.WithTimeout(t.Context(), time.Second)
		defer cancel()
		done := make(chan error, 1)
		go func() { done <- reconstructedWaitEnrichment(ctx, path) }()
		file, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0600)
		if err != nil {
			t.Fatal(err)
		}
		defer file.Close()
		for _, tail := range []string{"complete\"}", "\n"} {
			select {
			case err := <-done:
				t.Fatalf("accepted partial line: %v", err)
			case <-time.After(150 * time.Millisecond):
			}
			if _, err := file.WriteString(tail); err != nil {
				t.Fatal(err)
			}
		}
		if err := <-done; err != nil {
			t.Fatal(err)
		}
	})
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

func TestReconstructedMeasurementSearchTimeout(t *testing.T) {
	for _, tc := range []struct {
		name, script string
		timedOut     bool
	}{
		{"own_deadline_kills_search", "exec /bin/sleep 30", true},
		{"unrelated_kill", "kill -KILL $$", false},
		{"deadline_text_is_not_deadline", "echo 'signal: killed: CLI context deadline exceeded'; exit 1", false},
		{"malformed_answer", "echo 'not JSON'", false},
		{"exit_status", "exit 2", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			binary := filepath.Join(dir, "search")
			if err := os.WriteFile(binary, []byte("#!/bin/sh\n"+tc.script+"\n"), 0700); err != nil {
				t.Fatal(err)
			}
			f := &issue767Fixture{t: t, binary: binary, primary: dir, env: os.Environ()}
			d := &reconstructedDriver{f: f, symbols: []reconstructedSymbol{{Name: "Target", File: "target.go"}}}
			sample := d.search("load", 0)
			if sample.Passed || sample.Error == "" || sample.Answer.Error == "" || sample.TimedOut != tc.timedOut || sample.TimeoutMS != 10000 {
				t.Fatalf("sample=%+v", sample)
			}
			if tc.timedOut && sample.DurationMS != 10000 {
				t.Fatalf("duration=%v, want censored 10000 ms", sample.DurationMS)
			}
			if sample.Start.IsZero() || !sample.End.After(sample.Start) || f.clientCallCount() != 1 {
				t.Fatalf("sample=%+v calls=%d", sample, f.clientCallCount())
			}
		})
	}
}

func TestReconstructedMeasurementLoadSearchError(t *testing.T) {
	for _, timedOut := range []bool{true, false} {
		name := "generic_error_aborts"
		if timedOut {
			name = "killed_at_limit_continues"
		}
		t.Run(name, func(t *testing.T) {
			release := make(chan struct{})
			var calls atomic.Int32
			samples, windows, err := reconstructedLoadWindows(func() reconstructedRebuild {
				call := calls.Add(1)
				if call == 1 {
					<-release
					return reconstructedRebuild{Start: time.Unix(100, 0), End: time.Unix(120, 0)}
				}
				return reconstructedRebuild{Start: time.Unix(200, 0), End: time.Unix(220, 0)}
			}, func(index int) reconstructedSample {
				if index == 0 {
					close(release)
					return reconstructedSample{Kind: "load", Start: time.Unix(110, 0), End: time.Unix(121, 0),
						TimedOut: timedOut, TimeoutMS: 10000, DurationMS: 10000, Error: "search command failed"}
				}
				return reconstructedSample{Kind: "load", Start: time.Unix(201, 0), End: time.Unix(202, 0), Passed: true}
			})
			if len(samples) == 0 || samples[0].TimedOut != timedOut || samples[0].Error == "" {
				t.Fatalf("samples=%+v", samples)
			}
			if timedOut {
				if err != nil || len(windows) < 2 || calls.Load() < 2 {
					t.Fatalf("windows=%+v calls=%d error=%v", windows, calls.Load(), err)
				}
				if reconstructedLoaded(samples[0], windows) {
					t.Fatal("search crossing the rebuild boundary qualified as loaded")
				}
			} else if err == nil || err.Error() != samples[0].Error || len(windows) != 1 || calls.Load() != 1 || len(samples) != 1 {
				t.Fatalf("samples=%+v windows=%+v calls=%d error=%v", samples, windows, calls.Load(), err)
			}
		})
	}
}

func TestReconstructedMeasurementScorerSearchTimeout(t *testing.T) {
	timeout := reconstructedSample{Kind: "idle", Symbol: "Target", TimedOut: true, TimeoutMS: 10000, DurationMS: 1, Error: "search killed at CLI deadline"}
	run := reconstructedRun{Label: "reconstructed", Workload: "R-Fill", Quiet: true, Samples: []reconstructedSample{timeout},
		Rebuilds: []reconstructedRebuild{{Start: time.Unix(100, 0), End: time.Unix(200, 0)}}}
	for i := 0; i < 30; i++ {
		sample := reconstructedSample{Kind: "load", Start: time.Unix(110, 0), End: time.Unix(120, 0), Passed: true, DurationMS: 100}
		if i >= 28 {
			sample = timeout
			sample.Kind, sample.Start, sample.End = "load", time.Unix(110, 0), time.Unix(120, 0)
		}
		run.Samples = append(run.Samples, sample)
	}
	// A boundary-crossing timeout still fails the row, but is not a loaded latency.
	outside := timeout
	outside.Kind, outside.Start, outside.End = "load", time.Unix(195, 0), time.Unix(205, 0)
	run.Samples = append(run.Samples, outside)
	score, err := scoreReconstructed(run)
	if err != nil || run.Error != "" || score.Status != "fail" || score.Timeouts != 4 || len(score.Failures) != 4 || score.Loaded != 30 || score.Excluded != 1 || score.Samples != 31 {
		t.Fatalf("score=%+v run error=%q scorer error=%v", score, run.Error, err)
	}
	if score.IdleP95MS == nil || *score.IdleP95MS != 10000 || score.LoadedP95MS == nil || *score.LoadedP95MS != 10000 || score.Ratio == nil || *score.Ratio != 1 || score.ClientP95MS != 10000 {
		t.Fatalf("censored percentiles/ratio: %+v", score)
	}
	if score.Reason != `idle search "Target" failed: search killed at CLI deadline` {
		t.Fatalf("reason=%q", score.Reason)
	}
	for _, failure := range score.Failures {
		if !failure.TimedOut || failure.Passed || failure.TimeoutMS != 10000 || failure.DurationMS != 10000 || failure.Error != timeout.Error {
			t.Fatalf("failure=%+v", failure)
		}
	}
	t.Run("excluded_timeout_still_fails_row", func(t *testing.T) {
		run.Samples = []reconstructedSample{{Kind: "idle", Passed: true, DurationMS: 100}, outside}
		score, err := scoreReconstructed(run)
		if err != nil || run.Error != "" || score.Status != "fail" || score.Timeouts != 1 || len(score.Failures) != 1 || score.Loaded != 0 || score.Excluded != 1 || score.Reason != `load search "Target" failed: search killed at CLI deadline` {
			t.Fatalf("score=%+v run error=%q scorer error=%v", score, run.Error, err)
		}
	})
	t.Run("excluded_wrong_answer_stays_excluded", func(t *testing.T) {
		wrong := outside
		wrong.TimedOut, wrong.TimeoutMS, wrong.Error, wrong.DurationMS = false, 0, "", 100
		run.Samples = []reconstructedSample{{Kind: "idle", Passed: true, DurationMS: 100}, wrong}
		score, err := scoreReconstructed(run)
		if err != nil || score.Status == "fail" || len(score.Failures) != 0 || score.Excluded != 1 {
			t.Fatalf("score=%+v scorer error=%v", score, err)
		}
	})
	t.Run("wrong_answer_reason_not_empty", func(t *testing.T) {
		wrong := reconstructedSample{Kind: "idle", Symbol: "Target", DurationMS: 100}
		run.Samples = []reconstructedSample{wrong}
		score, err := scoreReconstructed(run)
		if err != nil || score.Status != "fail" || score.Reason != `idle search "Target" failed: did not pass` {
			t.Fatalf("score=%+v scorer error=%v", score, err)
		}
	})
	for _, workload := range []string{"R-C2", "R-DE"} {
		t.Run(workload+"_rejects_search_limit_on_probe", func(t *testing.T) {
			probe := timeout
			probe.Kind = "probe"
			_, err := scoreReconstructed(reconstructedRun{Label: "reconstructed", Workload: workload, Quiet: true, Samples: []reconstructedSample{probe}})
			if err == nil || !strings.Contains(err.Error(), "want 60000") {
				t.Fatalf("error=%v, want rejection of a 10 s probe timeout", err)
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
	pending := `{"by_repo":{},"contract_analysis":{"state":"incomplete"}}`
	for _, tc := range []struct {
		name, raw   string
		state       reconstructedContractState
		pass        bool
		newest, lag int
	}{
		{"current", good, reconstructedContractState{7, -1, false}, true, 7, 0},
		{"previous_save_just_after_write", strings.ReplaceAll(good, "save-007", "save-006"), reconstructedContractState{7, 5, false}, true, 6, 1},
		{"newer_while_saves_continue", strings.ReplaceAll(good, "save-007", "save-008"), reconstructedContractState{8, 7, false}, true, 8, 0},
		{"regression", strings.ReplaceAll(good, "save-007", "save-006"), reconstructedContractState{7, 7, false}, false, 6, 1},
		{"unknown_route", strings.ReplaceAll(good, "save-007", "unknown"), reconstructedContractState{7, -1, false}, false, -1, 0},
		{"incomplete", strings.ReplaceAll(good, "complete", "incomplete"), reconstructedContractState{7, -1, false}, false, -1, 0},
		{"wrong_file", strings.ReplaceAll(good, "reconstructed_http/route.go", "unrelated.go"), reconstructedContractState{7, -1, false}, false, -1, 0},
		{"not_a_contract", `{"message":"/reconstructed-save-007"}`, reconstructedContractState{7, -1, false}, false, -1, 0},
		{"empty", `{}`, reconstructedContractState{7, -1, false}, false, -1, 0},
		{"malformed", `{`, reconstructedContractState{7, -1, false}, false, -1, 0},
		{"restart_before_enrichment", pending, reconstructedContractState{0, -1, true}, true, -1, 1},
		{"restart_after_enrichment_missing_route", `{"by_repo":{},"contract_analysis":{"state":"complete"}}`, reconstructedContractState{0, -1, false}, false, -1, 1},
		{"restart_route_0", strings.ReplaceAll(good, "save-007", "save-000"), reconstructedContractState{0, -1, false}, true, 0, 0},
		{"restart_warmup_regression", pending, reconstructedContractState{0, 0, true}, false, -1, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := reconstructedContractAnswer([]byte(tc.raw), tc.state)
			if (err == nil) != tc.pass || (tc.pass && (got.NewestSave != tc.newest || got.LagSaves != tc.lag || got.Route0 != (tc.newest == 0))) {
				t.Fatalf("observation=%+v error=%v pass=%v", got, err, tc.pass)
			}
		})
	}
	notTracked := "Error: the gortex daemon does not track /private/repo — add it with `gortex track /private/repo`\n"
	incomplete := "Error: required_capability_incomplete: graph.contracts did not become complete within this request: context deadline exceeded; retry after 5s\n"
	good0 := strings.ReplaceAll(good, "save-007", "save-000")
	for _, tc := range []struct {
		name, raw, commandError string
		before, pass            bool
	}{
		{"startup_not_tracked", notTracked, "exit status 1", true, true},
		{"post_enrichment_not_tracked", notTracked, "exit status 1", false, false},
		{"other_repo_not_tracked", strings.ReplaceAll(notTracked, "/private/repo", "/other/repo"), "exit status 1", true, false},
		{"transport_error_not_startup", notTracked, "signal: killed", true, false},
		{"generic_exit_1", "Error: command failed\n", "exit status 1", true, false},
		{"post_enrichment_typed_incomplete", incomplete, "exit status 1", false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			at := time.Unix(100, 0)
			first := at.Add(2 * time.Second)
			if !tc.before {
				first = at.Add(4 * time.Second)
			}
			owner := &reconstructedDemandOwner{records: []reconstructedDemand{
				{Start: at, End: first, Response: tc.raw, Error: tc.commandError, DurationMS: 12},
				{Start: at.Add(5 * time.Second), End: at.Add(6 * time.Second), Response: good0},
			}}
			d := &reconstructedDriver{f: &issue767Fixture{primary: "/private/repo"}, run: reconstructedRun{Workload: "R-Restart", Parameters: map[string]any{}}}
			d.recordDemand(owner, at, at.Add(7*time.Second), nil, at.Add(3*time.Second))
			firstRecord := owner.records[0]
			if (d.run.Error == "") != tc.pass || firstRecord.Response != tc.raw || firstRecord.DurationMS != 12 || firstRecord.Available {
				t.Fatalf("first=%+v run=%+v", firstRecord, d.run)
			}
			if tc.pass && (firstRecord.UnavailableReason == "" || firstRecord.Error != "" || !owner.records[1].Available || !owner.records[1].Route0) {
				t.Fatalf("records=%+v", owner.records)
			}
		})
	}
	for _, hint := range []string{"", "; retry after 0s", "; retry after 20s"} {
		t.Run("typed_incomplete_hint_"+hint, func(t *testing.T) {
			raw := strings.ReplaceAll(incomplete, "; retry after 5s", hint)
			record := reconstructedObserveDemand(reconstructedDemand{Response: raw, Error: "exit status 1"}, reconstructedContractState{PreviousSave: -1}, "/private/repo")
			if record.Error != "" || record.Available || record.UnavailableReason == "" || record.RetryAfter != strings.TrimPrefix(hint, "; retry after ") {
				t.Fatalf("record=%+v", record)
			}
		})
	}
	t.Run("restart_requires_settled_attachment", func(t *testing.T) {
		at := time.Unix(100, 0)
		owner := &reconstructedDemandOwner{records: []reconstructedDemand{{Start: at, End: at.Add(time.Second), Response: notTracked, Error: "exit status 1"}}}
		d := &reconstructedDriver{f: &issue767Fixture{primary: "/private/repo"}, run: reconstructedRun{Workload: "R-Restart", Parameters: map[string]any{}}}
		d.recordDemand(owner, at, at.Add(5*time.Second), nil, at.Add(2*time.Second))
		if d.run.Error != "contract demand: no validated attachment after enrichment" {
			t.Fatalf("run=%+v", d.run)
		}
	})
	t.Run("typed_incomplete_DE_product_miss_retains_samples", func(t *testing.T) {
		incomplete := strings.ReplaceAll(incomplete, "5s", "0s")
		at := time.Unix(100, 0)
		jobs := reconstructedPairJobs([]reconstructedJobEvent{
			{Message: "contract analysis job started", Job: "one", Repo: "issue767", Mode: "cold"},
			{Message: "contract analysis job", Mode: "cold", Outcome: "superseded_key_moved", Elapsed: new(0.5)},
		})
		d := &reconstructedDriver{f: &issue767Fixture{primary: "/private/repo"}, run: reconstructedRun{Label: "reconstructed", Workload: "R-DE", Quiet: true, Parameters: map[string]any{}, Jobs: &jobs, JobStatus: jobs.Status,
			Samples: []reconstructedSample{{Passed: true, DurationMS: 10}, {Passed: true, DurationMS: 20}}}}
		owner := &reconstructedDemandOwner{records: []reconstructedDemand{{Start: at, End: at.Add(5 * time.Second), Response: incomplete, Error: "exit status 1", DurationMS: 5000}}}
		d.recordDemand(owner, at, at.Add(6*time.Second), nil, time.Time{})
		// Score the serialized row, as the reporting path does.
		raw, err := json.Marshal(d.run)
		if err != nil {
			t.Fatal(err)
		}
		var run reconstructedRun
		if err := json.Unmarshal(raw, &run); err != nil {
			t.Fatal(err)
		}
		score, err := scoreReconstructed(run)
		record := owner.records[0]
		if err != nil || score.Status != "fail" || score.ContractRoute != "fail" || !strings.Contains(score.Reason, "product contract route unavailable") || score.Samples != 2 || score.ClientP50MS != 10 || score.ClientP95MS != 20 || run.Error != "" {
			t.Fatalf("score=%+v error=%v run=%+v", score, err, run)
		}
		if record.Available || record.Error != "" || record.UnavailableReason != "required_capability_incomplete: context deadline exceeded" || record.RetryAfter != "0s" || record.Response != incomplete || record.DurationMS != 5000 || run.ContractUnavailable != 1 {
			t.Fatalf("record=%+v", record)
		}
		if run.Jobs.Positive || len(run.Jobs.Events) != 2 || run.Jobs.Starts != 1 || score.JobP95ByMode["cold"] != 500 || !reflect.DeepEqual(d.run.Parameters["contract_route_lag_saves_distribution"], map[int]int{}) || d.run.Parameters["contract_route_lag_reason"] != "no available route answers" || d.run.Parameters["contract_route_lag_unavailable_answers"] != 1 {
			t.Fatalf("run=%+v parameters=%v score=%+v", run, d.run.Parameters, score)
		}
	})
	for _, tc := range []struct{ name, raw string }{
		{"generic_command", "Error: command failed\n"},
		{"embedded_refusal", "prefix " + incomplete},
		{"invalid_retry_hint", strings.ReplaceAll(incomplete, "5s", "nonsense")},
		{"negative_retry_hint", strings.ReplaceAll(incomplete, "5s", "-1s")},
	} {
		t.Run("non_contract_exit_1_"+tc.name, func(t *testing.T) {
			record := reconstructedObserveDemand(reconstructedDemand{Response: tc.raw, Error: "exit status 1"}, reconstructedContractState{PreviousSave: -1}, "/private/repo")
			if record.Error == "" || record.UnavailableReason != "" || record.Available {
				t.Fatalf("record=%+v", record)
			}
		})
	}
	t.Run("record_lag_and_keep_invalid_demand", func(t *testing.T) {
		at := time.Unix(100, 0)
		owner := &reconstructedDemandOwner{records: []reconstructedDemand{
			{Start: at, End: at.Add(time.Second), Response: strings.ReplaceAll(good, "save-007", "save-000")},
			{Start: at.Add(2 * time.Second), End: at.Add(3 * time.Second), Response: strings.ReplaceAll(good, "save-007", "save-000")},
			{Start: at.Add(4 * time.Second), End: at.Add(5 * time.Second), Error: "command failed"},
		}}
		d := &reconstructedDriver{f: &issue767Fixture{primary: "/private/repo"}, run: reconstructedRun{Workload: "R-DE", Parameters: map[string]any{}}}
		d.recordDemand(owner, at, at.Add(6*time.Second), []time.Time{at.Add(2 * time.Second)}, time.Time{})
		if owner.records[1].Error != "" || owner.records[1].LagSaves != 1 || d.run.Error != "contract demand: command failed" || len(d.run.Parameters["contract_demand"].([]reconstructedDemand)) != 3 {
			t.Fatalf("records=%+v run=%+v", owner.records, d.run)
		}
		if !reflect.DeepEqual(d.run.Parameters["contract_route_lag_saves_distribution"], map[int]int{0: 1, 1: 1}) {
			t.Fatal(d.run.Parameters)
		}
	})
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
	t.Run("poll_counts_omit_only_zero", func(t *testing.T) {
		for _, sample := range []reconstructedSample{{}, {ErrorsCount: 1, OKPolls: 2}} {
			raw, err := json.Marshal(sample)
			if err != nil {
				t.Fatal(err)
			}
			var fields map[string]json.RawMessage
			if err := json.Unmarshal(raw, &fields); err != nil {
				t.Fatal(err)
			}
			if (len(fields["errors_count"]) > 0) != (sample.ErrorsCount > 0) || (len(fields["ok_polls"]) > 0) != (sample.OKPolls > 0) {
				t.Fatalf("sample=%+v raw=%s", sample, raw)
			}
		}
	})
	t.Run("C2_last_error_separate_from_later_success", func(t *testing.T) {
		sample := reconstructedSample{Kind: "probe", TimedOut: true, TimeoutMS: 60000, DurationMS: 60000}
		sample.recordPoll(issue767Answer{Error: "busy"}, errors.New("busy"))
		answer := issue767Answer{Exact: true}
		sample.recordPoll(answer, nil)
		sample.recordPoll(answer, nil)
		if sample.Polls != 3 || sample.ErrorsCount != 1 || sample.OKPolls != 2 || sample.LastError != "busy" || sample.LastErrorPoll != 1 || sample.LastAnswer == nil || *sample.LastAnswer != answer || sample.Answer != answer {
			t.Fatalf("sample=%+v", sample)
		}
		sample.recordPoll(issue767Answer{Error: "later busy"}, errors.New("later busy"))
		if sample.ErrorsCount != 2 || sample.LastErrorPoll != 4 || sample.LastError != "later busy" || sample.OKPolls != 2 || *sample.LastAnswer != answer {
			t.Fatalf("sample=%+v", sample)
		}
		run := reconstructedRun{Label: "reconstructed", Workload: "R-C2", Quiet: true, Samples: []reconstructedSample{sample}}
		score, err := scoreReconstructed(run)
		if err != nil || score.Status != "fail" || score.Timeouts != 1 || score.ClientP95MS != 60000 {
			t.Fatalf("score=%+v error=%v", score, err)
		}
	})
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
		run := reconstructedRun{Label: "reconstructed", Workload: "R-DE", Quiet: true, ContractUnavailable: 1, JobStatus: status, JobReason: "no complete telemetry", Samples: []reconstructedSample{{Passed: true, DurationMS: 1}}}
		score, err := scoreReconstructed(run)
		if err != nil || score.Status != "fail" || score.ContractRoute != "fail" || !strings.Contains(score.Reason, "product contract route unavailable") || score.Samples != 1 || score.ClientP95MS != 1 {
			t.Fatalf("product miss downgraded: score=%+v error=%v", score, err)
		}
		run.ContractUnavailable = 0
		score, err = scoreReconstructed(run)
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

func TestReconstructedMeasurementRestartStarts(t *testing.T) {
	t.Run("preparation_wait_cancellation", func(t *testing.T) {
		ctx, cancel := context.WithCancel(t.Context())
		cancel()
		if err := reconstructedWait(ctx, time.Hour); !errors.Is(err, context.Canceled) {
			t.Fatalf("wait error=%v", err)
		}
		if err := reconstructedWait(t.Context(), 0); err != nil {
			t.Fatalf("zero wait error=%v", err)
		}
	})
	incomplete := reconstructedDemand{Response: "Error: required_capability_incomplete: graph.contracts did not become complete within this request: context deadline exceeded; retry after 5s\n", Error: "exit status 1", DurationMS: 5000}
	complete := reconstructedDemand{Response: `{"by_repo":{"repo":{"contracts":{"http":[{"id":"http::GET::/reconstructed-save-000","type":"http","role":"provider","file_path":"repo/reconstructed_http/route.go"}]}}},"contract_analysis":{"state":"complete"}}`}
	t.Run("preparation_result_keeps_last_reason_and_all_attempts", func(t *testing.T) {
		attempts := []reconstructedDemand{
			{UnavailableReason: "required_capability_incomplete: context deadline exceeded"},
			{UnavailableReason: "required_capability_incomplete: contract analysis of these inputs failed"},
		}
		run := reconstructedRun{Parameters: map[string]any{}}
		proceed := reconstructedRecordPreparation(&run, attempts, errors.New("product preparation failure"))
		want := "preparation contract attachment: product preparation failure; last unavailable: " + attempts[1].UnavailableReason
		if proceed || run.Error != "" || run.PreparationInsufficient != want || !reflect.DeepEqual(run.Parameters["preparation_contract_demand"], attempts) {
			t.Fatalf("proceed=%v run=%+v", proceed, run)
		}
	})
	for _, tc := range []struct {
		name, hint       string
		remaining, delay time.Duration
		calls            int
	}{
		{"hint", "; retry after 5s", 10 * time.Second, 5 * time.Second, 2},
		{"no_hint_floor", "", 10 * time.Second, time.Second, 2},
		{"zero_hint_floor", "; retry after 0s", 10 * time.Second, time.Second, 2},
		{"positive_subsecond_hint", "; retry after 100ms", 10 * time.Second, 100 * time.Millisecond, 2},
		{"capped_by_remaining", "; retry after 5s", 2 * time.Second, 2 * time.Second, 1},
	} {
		t.Run("preparation_pacing_"+tc.name, func(t *testing.T) {
			now := time.Now()
			ctx, cancel := context.WithDeadline(t.Context(), now.Add(tc.remaining))
			defer cancel()
			calls := 0
			var waits []time.Duration
			refusal := incomplete
			refusal.Response = strings.ReplaceAll(refusal.Response, "; retry after 5s", tc.hint)
			attempts, err := reconstructedPrepareAttachment(ctx, "/private/repo", func(timeout time.Duration) reconstructedDemand {
				calls++
				wantTimeout := time.Minute
				if calls > 1 {
					wantTimeout = tc.remaining - tc.delay
				}
				if timeout != wantTimeout {
					t.Fatalf("request %d timeout=%v want %v", calls, timeout, wantTimeout)
				}
				if calls == 1 {
					return refusal
				}
				return complete
			}, func() time.Time { return now }, func(_ context.Context, delay time.Duration) error {
				waits = append(waits, delay)
				now = now.Add(delay)
				return nil
			})
			if calls != tc.calls || len(attempts) != tc.calls || !reflect.DeepEqual(waits, []time.Duration{tc.delay}) || attempts[0].Available || attempts[0].RetryAfter != strings.TrimPrefix(tc.hint, "; retry after ") || attempts[0].Response != refusal.Response {
				t.Fatalf("calls=%d attempts=%+v waits=%v error=%v", calls, attempts, waits, err)
			}
			run := reconstructedRun{Label: "reconstructed", Workload: "R-Restart", Quiet: true, Parameters: map[string]any{}}
			proceed := reconstructedRecordPreparation(&run, attempts, err)
			if tc.calls == 2 {
				if err != nil || !proceed || !attempts[1].Available || !attempts[1].Route0 || run.Error != "" || run.PreparationInsufficient != "" {
					t.Fatalf("proceed=%v attempts=%+v error=%v run=%+v", proceed, attempts, err, run)
				}
				return
			}
			score, scoreErr := scoreReconstructed(run)
			if !errors.Is(err, context.DeadlineExceeded) || proceed || run.Error != "" || !strings.Contains(run.PreparationInsufficient, "deadline passed before retry") || !strings.Contains(run.PreparationInsufficient, attempts[0].UnavailableReason) || scoreErr != nil || score.Status != "insufficient" || score.Reason != run.PreparationInsufficient || !reflect.DeepEqual(run.Parameters["preparation_contract_demand"], attempts) {
				t.Fatalf("proceed=%v error=%v run=%+v score=%+v scorer=%v", proceed, err, run, score, scoreErr)
			}
		})
	}
	t.Run("preparation_retry_killed_at_deadline", func(t *testing.T) {
		now := time.Now()
		ctx, cancel := context.WithDeadline(t.Context(), now.Add(7*time.Second))
		defer cancel()
		calls := 0
		attempts, err := reconstructedPrepareAttachment(ctx, "/private/repo", func(time.Duration) reconstructedDemand {
			calls++
			if calls == 1 {
				return incomplete
			}
			now = now.Add(2 * time.Second) // The CLI is killed when the remaining deadline ends.
			return reconstructedDemand{Response: "", Error: "signal: killed"}
		}, func() time.Time { return now }, func(_ context.Context, delay time.Duration) error {
			now = now.Add(delay)
			return nil
		})
		run := reconstructedRun{Label: "reconstructed", Workload: "R-Restart", Quiet: true, Parameters: map[string]any{}}
		proceed := reconstructedRecordPreparation(&run, attempts, err)
		if calls != 2 || len(attempts) != 2 || attempts[1].Error != "signal: killed" || !errors.Is(err, context.DeadlineExceeded) || proceed || run.Error != "" || !strings.Contains(run.PreparationInsufficient, "deadline passed during retry") {
			t.Fatalf("calls=%d attempts=%+v error=%v run=%+v", calls, attempts, err, run)
		}
	})
	for _, first := range []struct {
		name   string
		answer reconstructedDemand
		pass   bool
	}{
		{"complete", complete, true},
		{"incomplete_deadline_before_retry", incomplete, false},
	} {
		t.Run("preparation_first_request_after_deadline_"+first.name, func(t *testing.T) {
			now := time.Now()
			ctx, cancel := context.WithDeadline(t.Context(), now.Add(-time.Second))
			defer cancel()
			calls := 0
			attempts, err := reconstructedPrepareAttachment(ctx, "/private/repo", func(timeout time.Duration) reconstructedDemand {
				calls++
				if timeout != time.Minute {
					t.Fatalf("first timeout=%v", timeout)
				}
				return first.answer
			}, func() time.Time { return now }, func(context.Context, time.Duration) error {
				t.Fatal("wait after deadline")
				return nil
			})
			if calls != 1 || len(attempts) != 1 || (err == nil) != first.pass || attempts[0].Response != first.answer.Response {
				t.Fatalf("calls=%d attempts=%+v error=%v", calls, attempts, err)
			}
			run := reconstructedRun{Parameters: map[string]any{}}
			if reconstructedRecordPreparation(&run, attempts, err) != first.pass || (!first.pass && (!errors.Is(err, context.DeadlineExceeded) || !strings.Contains(run.PreparationInsufficient, "deadline passed before retry") || !strings.Contains(run.PreparationInsufficient, attempts[0].UnavailableReason))) {
				t.Fatalf("error=%v run=%+v", err, run)
			}
		})
	}
	for _, reason := range []string{"contract analysis of these inputs failed", "contract analysis of these inputs stalled", "contract analysis of these inputs panic", "other product error"} {
		t.Run("preparation_product_reason_stops_"+reason, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(t.Context(), time.Minute)
			defer cancel()
			calls := 0
			refusal := incomplete
			refusal.Response = strings.ReplaceAll(refusal.Response, "context deadline exceeded", reason)
			attempts, err := reconstructedPrepareAttachment(ctx, "/private/repo", func(time.Duration) reconstructedDemand {
				calls++
				if calls > 1 {
					t.Fatal("retried product failure")
				}
				return refusal
			}, time.Now, func(context.Context, time.Duration) error {
				t.Fatal("waited after product failure")
				return nil
			})
			run := reconstructedRun{Label: "reconstructed", Workload: "R-Restart", Quiet: true, Parameters: map[string]any{}}
			proceed := reconstructedRecordPreparation(&run, attempts, err)
			score, scoreErr := scoreReconstructed(run)
			if err == nil || calls != 1 || len(attempts) != 1 || attempts[0].Response != refusal.Response || attempts[0].Error != "" || proceed || run.Error != "" || !strings.Contains(run.PreparationInsufficient, "product preparation failure") || !strings.Contains(run.PreparationInsufficient, "required_capability_incomplete: "+reason) || scoreErr != nil || score.Status != "insufficient" || score.Reason != run.PreparationInsufficient || !reflect.DeepEqual(run.Parameters["preparation_contract_demand"], attempts) {
				t.Fatalf("calls=%d attempts=%+v error=%v run=%+v score=%+v scorer=%v", calls, attempts, err, run, score, scoreErr)
			}
		})
	}
	t.Run("preparation_parent_cancellation_remains_error", func(t *testing.T) {
		ctx, cancel := context.WithTimeout(t.Context(), time.Minute)
		defer cancel()
		calls := 0
		attempts, err := reconstructedPrepareAttachment(ctx, "/private/repo", func(time.Duration) reconstructedDemand {
			calls++
			return incomplete
		}, time.Now, func(context.Context, time.Duration) error {
			cancel()
			return ctx.Err()
		})
		run := reconstructedRun{Parameters: map[string]any{}}
		if !errors.Is(err, context.Canceled) || calls != 1 || len(attempts) != 1 || reconstructedRecordPreparation(&run, attempts, err) || run.Error != "preparation contract attachment: context canceled" || run.PreparationInsufficient != "" {
			t.Fatalf("calls=%d attempts=%+v error=%v run=%+v", calls, attempts, err, run)
		}
	})
	t.Run("preparation_generic_exit_not_retried", func(t *testing.T) {
		ctx, cancel := context.WithTimeout(t.Context(), time.Second)
		defer cancel()
		calls := 0
		attempts, err := reconstructedPrepareAttachment(ctx, "/private/repo", func(time.Duration) reconstructedDemand {
			calls++
			return reconstructedDemand{Response: "Error: generic failure\n", Error: "exit status 1"}
		}, time.Now, func(context.Context, time.Duration) error {
			t.Fatal("waited after command failure")
			return nil
		})
		run := reconstructedRun{Parameters: map[string]any{}}
		if err == nil || calls != 1 || len(attempts) != 1 || attempts[0].Error != "exit status 1" || reconstructedRecordPreparation(&run, attempts, err) || run.Error != "preparation contract attachment: exit status 1" || run.PreparationInsufficient != "" {
			t.Fatalf("calls=%d attempts=%+v error=%v run=%+v", calls, attempts, err, run)
		}
	})
	for _, tc := range []struct {
		name, jobStatus, want string
		count                 *int
	}{
		{"unfinished_start", "insufficient", "fail", new(1)},
		{"gap_with_start", "insufficient", "fail", new(2)},
		{"weak_zero", "insufficient", "insufficient", new(0)},
		{"no_positive_preparation", "unsupported", "unsupported", nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			run := reconstructedRun{Label: "reconstructed", Workload: "R-Restart", Quiet: true,
				JobCount: tc.count, JobStatus: tc.jobStatus, JobReason: "weak telemetry or demand gap",
				Samples: []reconstructedSample{{Passed: true, DurationMS: 1}}}
			got, err := scoreReconstructed(run)
			if err != nil || got.Status != tc.want {
				t.Fatalf("score=%+v error=%v want=%s", got, err, tc.want)
			}
		})
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
