//go:build acceptance

package main

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"sort"
	"time"
)

// Milliseconds keep the raw JSON and scored values in the same units.
type reconstructedSample struct {
	Kind       string         `json:"kind"`
	Symbol     string         `json:"symbol,omitempty"`
	File       string         `json:"file,omitempty"`
	Start      time.Time      `json:"start"`
	End        time.Time      `json:"end"`
	DurationMS float64        `json:"duration_ms"`
	TimeoutMS  float64        `json:"timeout_ms,omitempty"`
	TimedOut   bool           `json:"timed_out"`
	Passed     bool           `json:"passed"`
	Error      string         `json:"error,omitempty"`
	Polls      int            `json:"polls,omitempty"`
	Answer     issue767Answer `json:"answer"`

	LastError     string          `json:"last_error,omitempty"`
	LastErrorPoll int             `json:"last_error_poll,omitempty"`
	LastAnswer    *issue767Answer `json:"last_answer,omitempty"`
	ErrorsCount   int             `json:"errors_count,omitempty"`
	OKPolls       int             `json:"ok_polls,omitempty"`
}

func (sample *reconstructedSample) recordPoll(answer issue767Answer, err error) {
	sample.Polls++
	sample.Answer = answer
	if err != nil {
		sample.LastError, sample.LastErrorPoll = err.Error(), sample.Polls
		sample.ErrorsCount++
		return
	}
	sample.LastAnswer = &answer
	sample.OKPolls++
}

type reconstructedRebuild struct {
	Start      time.Time `json:"start"`
	End        time.Time `json:"end"`
	DurationMS float64   `json:"duration_ms"`
	Error      string    `json:"error,omitempty"`
	Response   string    `json:"response,omitempty"`
}

// One JSON line is one workload, binary and repeat. The workflow records the
// binary/repeat identity in its artifact path; parameters state the workload.
type reconstructedRun struct {
	Mode         string                     `json:"mode,omitempty"`
	Profiles     *reconstructedProfiles     `json:"profiles,omitempty"`
	Label        string                     `json:"label"`
	Workload     string                     `json:"workload"`
	Parameters   map[string]any             `json:"parameters"`
	Quiet        bool                       `json:"quiet"`
	Error        string                     `json:"error,omitempty"`
	Samples      []reconstructedSample      `json:"samples"`
	Rebuilds     []reconstructedRebuild     `json:"rebuilds,omitempty"`
	Jobs         *reconstructedJobEvidence  `json:"contract_jobs,omitempty"`
	JobStatus    string                     `json:"job_status,omitempty"`
	JobReason    string                     `json:"job_reason,omitempty"`
	JobMS        []float64                  `json:"job_ms,omitempty"`
	JobCount     *int                       `json:"job_count,omitempty"`
	ProcessBytes *uint64                    `json:"process_bytes_written,omitempty"`
	StoreGrowth  *int64                     `json:"store_plus_wal_growth_bytes,omitempty"`
	Resources    []issue767ProcessResources `json:"resources"`
	Score        *reconstructedScore        `json:"score,omitempty"`

	PreparationInsufficient string `json:"preparation_insufficient,omitempty"`
	ContractUnavailable     int    `json:"contract_unavailable_answers"`
}

type reconstructedScore struct {
	Label        string                `json:"label"`
	Workload     string                `json:"workload"`
	Status       string                `json:"status"`
	Samples      int                   `json:"samples"`
	Loaded       int                   `json:"loaded_searches,omitempty"`
	Excluded     int                   `json:"excluded_searches,omitempty"`
	ClientP50MS  float64               `json:"client_p50_ms"`
	ClientP95MS  float64               `json:"client_p95_ms"`
	IdleP95MS    *float64              `json:"idle_p95_ms,omitempty"`
	LoadedP95MS  *float64              `json:"loaded_p95_ms,omitempty"`
	Ratio        *float64              `json:"load_to_idle_p95_ratio,omitempty"`
	Job          string                `json:"job"`
	JobP95ByMode map[string]float64    `json:"job_p95_ms_by_mode,omitempty"`
	JobP95MS     *float64              `json:"job_p95_ms,omitempty"`
	JobCount     *int                  `json:"job_count,omitempty"`
	Timeouts     int                   `json:"timeouts"`
	Failures     []reconstructedSample `json:"failures,omitempty"`
	Reason       string                `json:"reason,omitempty"`

	ContractRoute string `json:"contract_route,omitempty"`
}

func reconstructedPercentile(values []float64, percentile float64) (float64, error) {
	if len(values) == 0 {
		return 0, errors.New("empty sample set")
	}
	ordered := append([]float64(nil), values...)
	for _, value := range ordered {
		if math.IsNaN(value) || math.IsInf(value, 0) || value < 0 {
			return 0, fmt.Errorf("invalid sample duration %v", value)
		}
	}
	sort.Float64s(ordered)
	return ordered[int(math.Ceil(percentile*float64(len(ordered))))-1], nil
}

func reconstructedSampleMS(sample reconstructedSample) (float64, error) {
	if !sample.TimedOut {
		return sample.DurationMS, nil
	}
	// Censor at the owning operation's deadline, not a partial elapsed value.
	timeout := 60000.0
	switch sample.Kind {
	case "fresh":
		timeout = 900000
	case "idle", "load":
		timeout = float64(issue767SearchTimeout.Milliseconds())
	}
	if sample.TimeoutMS != timeout {
		return 0, fmt.Errorf("%s timeout_ms = %v, want %v", sample.Kind, sample.TimeoutMS, timeout)
	}
	return timeout, nil
}

func reconstructedLoaded(sample reconstructedSample, rebuilds []reconstructedRebuild) bool {
	for _, rebuild := range rebuilds {
		if rebuild.Error == "" && !sample.Start.Before(rebuild.Start) && !sample.End.After(rebuild.End) {
			return true
		}
	}
	return false
}

func scoreReconstructed(run reconstructedRun) (reconstructedScore, error) {
	score := reconstructedScore{Label: "reconstructed", Workload: run.Workload, Status: "pass", Job: "unsupported"}
	if run.Label != "reconstructed" {
		return score, errors.New("result label must be reconstructed")
	}
	switch run.Workload {
	case "R-C2", "R-DE", "R-Fill", "R-Fresh", "R-Restart":
	default:
		return score, fmt.Errorf("unknown reconstructed workload %q", run.Workload)
	}
	if run.Error != "" {
		score.Status, score.Reason = "fail", run.Error
	}
	if run.Mode == "profile" {
		if run.Profiles == nil {
			return score, errors.New("profile run missing capture manifest")
		}
		if run.Error != "" {
			return score, nil
		}
		if run.Profiles.Error != "" {
			return score, errors.New(run.Profiles.Error)
		}
		if err := run.Profiles.validate(); err != nil {
			return score, err
		}
		score.Status, score.Reason = "diagnostic", "separate profile repeat; excluded from timing evidence"
		return score, nil
	}
	if run.Profiles != nil {
		return score, errors.New("timing run contains diagnostic profiles")
	}
	if run.PreparationInsufficient != "" && run.Error == "" {
		score.Status, score.Reason = "insufficient", run.PreparationInsufficient
		return score, nil
	}
	if run.Workload != "R-Fresh" && !run.Quiet && run.Error == "" {
		score.Status, score.Reason = "insufficient", "quiet did not arrive within 300 s"
		return score, nil
	}
	if len(run.Samples) == 0 {
		// A driver failure before the first sample is a failed run with its
		// cause, not a malformed result.
		if run.Error != "" {
			return score, nil
		}
		return score, errors.New("empty sample set")
	}
	for _, rebuild := range run.Rebuilds {
		if rebuild.Start.IsZero() || !rebuild.End.After(rebuild.Start) {
			return score, errors.New("invalid rebuild window")
		}
		if rebuild.Error != "" {
			score.Status, score.Reason = "fail", rebuild.Error
		}
	}
	var client, idle, loaded []float64
	for _, sample := range run.Samples {
		if sample.Start.After(sample.End) {
			return score, errors.New("sample ends before it starts")
		}
		excluded := run.Workload == "R-Fill" && sample.Kind == "load" && !reconstructedLoaded(sample, run.Rebuilds)
		if excluded && !sample.TimedOut {
			score.Excluded++
			continue
		}
		value, err := reconstructedSampleMS(sample)
		if err != nil {
			return score, err
		}
		if sample.TimedOut {
			score.Timeouts++
		}
		// C2 is an every-edit target, not a percentile target. Preserve the
		// oracle result while listing late successful answers as target failures.
		late := (run.Workload == "R-C2" && value > 5000) || (run.Workload == "R-Fresh" && value > 180000)
		if !sample.Passed || sample.TimedOut || sample.Error != "" || late {
			sample.DurationMS = value
			score.Failures = append(score.Failures, sample)
			score.Status = "fail"
			if run.Workload == "R-Fill" && score.Reason == "" {
				cause := sample.Error
				if cause == "" {
					cause = "did not pass"
				}
				score.Reason = fmt.Sprintf("%s search %q failed: %s", sample.Kind, sample.Symbol, cause)
			}
		}
		// A timed-out search outside a window still fails the row, but stays
		// out of the loaded percentiles.
		if excluded {
			score.Excluded++
			continue
		}
		client = append(client, value)
		if run.Workload == "R-Fill" {
			switch sample.Kind {
			case "idle":
				idle = append(idle, value)
			case "load":
				loaded = append(loaded, value)
			default:
				return score, fmt.Errorf("unknown search phase %q", sample.Kind)
			}
		}
	}
	if len(client) == 0 && score.Status == "fail" {
		return score, nil
	}
	var err error
	score.ClientP50MS, err = reconstructedPercentile(client, .50)
	if err != nil {
		return score, err
	}
	score.ClientP95MS, err = reconstructedPercentile(client, .95)
	if err != nil {
		return score, err
	}
	score.Samples = len(client)
	switch run.Workload {
	case "R-DE":
		if run.ContractUnavailable > 0 {
			score.ContractRoute, score.Status = "fail", "fail"
			if run.Error == "" {
				score.Reason = "product contract route unavailable: required_capability_incomplete"
			}
		}
		if run.JobStatus != "" {
			score.Job = run.JobStatus
			if run.JobStatus != "supported" {
				if score.Status != "fail" {
					score.Status, score.Reason = run.JobStatus, run.JobReason
				}
				break
			}
			score.JobP95ByMode = map[string]float64{}
			if run.Jobs == nil || len(run.Jobs.BodyMS) == 0 {
				return score, errors.New("supported jobs have no paired bodies")
			}
			for mode, values := range run.Jobs.BodyMS {
				p95, err := reconstructedPercentile(values, .95)
				if err != nil {
					return score, err
				}
				score.JobP95ByMode[mode] = p95
				if p95 > 700 {
					score.Status = "fail"
				}
			}
			break
		}
		// Legacy phase-0 scorer fixtures carry only pooled durations.
		if len(run.JobMS) == 0 {
			if score.Status == "pass" {
				score.Status = "unsupported"
			}
			break
		}
		jobP95, err := reconstructedPercentile(run.JobMS, .95)
		if err != nil {
			return score, err
		}
		score.Job, score.JobP95MS = "supported", &jobP95
		if jobP95 > 700 {
			score.Status = "fail"
		}
	case "R-Fill":
		score.Loaded = len(loaded)
		if len(idle) == 0 {
			return score, errors.New("empty idle sample set")
		}
		idleP95, err := reconstructedPercentile(idle, .95)
		if err != nil {
			return score, err
		}
		if idleP95 == 0 {
			return score, errors.New("idle p95 is zero; ratio is undefined")
		}
		score.IdleP95MS = &idleP95
		if len(loaded) < 30 {
			// A rebuild or driver failure already explains the shortage; keep it.
			if score.Status != "fail" {
				score.Status, score.Reason = "insufficient", "fewer than 30 loaded searches"
			}
			break
		}
		loadedP95, err := reconstructedPercentile(loaded, .95)
		if err != nil {
			return score, err
		}
		ratio := loadedP95 / idleP95
		score.LoadedP95MS, score.Ratio = &loadedP95, &ratio
		if ratio > 2 {
			score.Status = "fail"
		}
	case "R-Restart":
		if run.JobCount != nil && *run.JobCount > 0 {
			score.Job, score.JobCount = "supported", run.JobCount
			score.Status, score.Reason = "fail", "contract job started during restart"
			break
		}
		if (run.ProcessBytes == nil || run.StoreGrowth == nil) && score.Status != "fail" {
			score.Status, score.Reason = "insufficient", "restart I/O measurement unavailable"
		}
		if run.JobStatus != "" && run.JobStatus != "supported" {
			score.Job = run.JobStatus
			if score.Status != "fail" {
				score.Status, score.Reason = run.JobStatus, run.JobReason
			}
			break
		}
		if run.JobCount != nil {
			score.Job, score.JobCount = "supported", run.JobCount
			if *run.JobCount != 0 {
				score.Status = "fail"
			}
		} else if score.Status == "pass" {
			score.Status = "unsupported"
		}
	}
	return score, nil
}

func readReconstructed(input io.Reader) ([]reconstructedRun, error) {
	scanner := bufio.NewScanner(input)
	scanner.Buffer(make([]byte, 4096), 16<<20)
	var runs []reconstructedRun
	for scanner.Scan() {
		var run reconstructedRun
		if err := json.Unmarshal(scanner.Bytes(), &run); err != nil {
			return nil, fmt.Errorf("result line %d: %w", len(runs)+1, err)
		}
		score, err := scoreReconstructed(run)
		if err != nil {
			return nil, fmt.Errorf("result line %d: %w", len(runs)+1, err)
		}
		run.Score = &score
		runs = append(runs, run)
	}
	if err := scanner.Err(); err != nil {
		return nil, err
	}
	if len(runs) == 0 {
		return nil, errors.New("empty result set")
	}
	return runs, nil
}
