package main

import (
	"encoding/json"
	"flag"
	"math"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"testing"
)

func TestCLSPoolingPreservesInputs(t *testing.T) {
	hidden := make([]float32, 2*768)
	hidden[0], hidden[1], hidden[768] = 3, 4, 900
	original := slices.Clone(hidden)
	vector := pool(hidden, []int64{1, 1}, []int64{1, 2}, "coderank")[0]
	if math.Abs(float64(vector[0])-0.6) > 1e-7 || math.Abs(float64(vector[1])-0.8) > 1e-7 {
		t.Fatalf("wrong CLS vector: %v", vector[:2])
	}
	if !slices.Equal(hidden, original) {
		t.Fatal("pooling changed hidden state")
	}
}

func TestMaskedMeanPooling(t *testing.T) {
	hidden := make([]float32, 2*768)
	hidden[0], hidden[1], hidden[768] = 3, 4, 900
	vector := pool(hidden, []int64{1, 0}, []int64{1, 2}, "nomic")[0]
	mean, norm := 0.0, 0.0
	for _, value := range vector {
		mean += float64(value)
		norm += float64(value) * float64(value)
	}
	if math.Abs(mean) > 1e-6 || math.Abs(norm-1) > 1e-6 || vector[1] <= vector[0] {
		t.Fatalf("wrong masked normalized vector: mean=%g norm=%g first=%v", mean, norm, vector[:2])
	}
}

func TestCheckedNativeRSSReadFailures(t *testing.T) {
	for _, sample := range [][2]uint64{{0, 0}, {0, 1}, {1, 0}} {
		current, peak, err := checkedRSS(sample[0], sample[1])
		if err == nil || current != 0 || peak != 0 {
			t.Fatalf("accepted failed native read %v: %d %d %v",
				sample, current, peak, err)
		}
	}
	for _, sample := range [][2]uint64{{1, 1}, {128, 256}} {
		current, peak, err := checkedRSS(sample[0], sample[1])
		if err != nil || current != sample[0] || peak != sample[1] {
			t.Fatalf("changed native bytes %v: %d %d %v",
				sample, current, peak, err)
		}
	}
}

func TestProcRSSBytesAndRejections(t *testing.T) {
	current, peak, err := procRSS("Name: probe\nVmRSS: 12 kB\nVmHWM: 20 kB\n")
	if err != nil || current != 12288 || peak != 20480 {
		t.Fatalf("current=%d peak=%d error=%v", current, peak, err)
	}
	for _, status := range []string{
		"", "VmRSS: 12 kB", "VmRSS: 12 MB\nVmHWM: 20 kB",
		"VmRSS: -1 kB\nVmHWM: 20 kB", "VmRSS: 12 kB\nVmHWM: 11 kB",
		"VmRSS: 12 kB\nVmRSS: 12 kB\nVmHWM: 20 kB",
		"VmRSS: 18446744073709551615 kB\nVmHWM: 20 kB",
	} {
		if _, _, err := procRSS(status); err == nil {
			t.Fatalf("accepted malformed RSS %q", status)
		}
	}
}

func TestArbitraryRowsAndTokenShapeRejections(t *testing.T) {
	batch := Batch{Shape: []int64{501, 2}, Inputs: map[string][]int64{}}
	for i := range 501 {
		batch.Indices = append(batch.Indices, i)
	}
	for _, name := range []string{"input_ids", "attention_mask", "token_type_ids"} {
		batch.Inputs[name] = make([]int64, 1002)
	}
	for i := range 501 {
		batch.Inputs["attention_mask"][2*i] = 1
	}
	rows, tokens, err := tokenCounts([]Batch{batch}, 8192)
	if err != nil || rows != 501 || tokens != 501 {
		t.Fatalf("rows=%d tokens=%d error=%v", rows, tokens, err)
	}
	for _, mode := range []string{"shape", "missing", "short", "duplicate", "negative", "outside", "mask", "cap"} {
		t.Run(mode, func(t *testing.T) {
			encoded, _ := json.Marshal(batch)
			var changed Batch
			if err := json.Unmarshal(encoded, &changed); err != nil {
				t.Fatal(err)
			}
			cap := 8192
			switch mode {
			case "shape":
				changed.Shape[0]--
			case "missing":
				delete(changed.Inputs, "token_type_ids")
			case "short":
				changed.Inputs["input_ids"] = []int64{1}
			case "duplicate":
				changed.Indices[500] = 0
			case "negative":
				changed.Indices[0] = -1
			case "outside":
				changed.Indices[500] = 501
			case "mask":
				changed.Inputs["attention_mask"][0] = 2
			case "cap":
				cap = 1
			}
			if _, _, err := tokenCounts([]Batch{changed}, cap); err == nil {
				t.Fatal("accepted malformed token batch")
			}
		})
	}
}

func TestProbeThreadAndGuardFlagsBeforeRuntime(t *testing.T) {
	previousArgs, previousFlags := os.Args, flag.CommandLine
	t.Cleanup(func() { os.Args, flag.CommandLine = previousArgs, previousFlags })
	for _, threads := range []int{1, 2, 4, 8, 16, 0, -1} {
		flag.CommandLine = flag.NewFlagSet("probe", flag.ContinueOnError)
		os.Args = []string{"probe", "-model", "coderank", "-threads", strconv.Itoa(threads), "-tokens", t.TempDir() + "/missing"}
		err := run()
		if threads > 0 {
			if !os.IsNotExist(err) || flag.Lookup("threads").Value.String() != strconv.Itoa(threads) {
				t.Fatalf("thread selection %d: %v", threads, err)
			}
		} else if err == nil || !strings.Contains(err.Error(), "threads") {
			t.Fatalf("invalid threads %d: %v", threads, err)
		}
	}
	for _, guard := range []string{"0s", "-1s"} {
		flag.CommandLine = flag.NewFlagSet("probe", flag.ContinueOnError)
		os.Args = []string{"probe", "-model", "coderank", "-guard", guard}
		if err := run(); err == nil || !strings.Contains(err.Error(), "guard") {
			t.Fatalf("invalid guard %s: %v", guard, err)
		}
	}
}

func TestPartialDenominatorsAndQueryUnits(t *testing.T) {
	latencies := []float64{10, 1, 5, 3}
	partial := finish(Report{PlannedTexts: 5000, PlannedTokens: 10000,
		CompletedTexts: 4, CompletedTokens: 20, Seconds: 2,
		Reason: "runner guard exceeded", GuardSeconds: 2}, latencies)
	if partial.Status != "partial" || partial.TextsPerSecond != 2 || partial.TokensPerSecond != 10 || partial.MinutesPer10k != 5000.0/60 {
		t.Fatalf("wrong partial rates: %+v", partial)
	}
	if partial.LatencySamples != 4 || partial.PercentileMethod != "nearest-rank" || partial.QueryP50MS != 3 || partial.QueryP95MS != 10 || !slices.Equal(latencies, []float64{10, 1, 5, 3}) {
		t.Fatalf("latency units, percentiles or caller ownership: %+v %v", partial, latencies)
	}
	complete := finish(Report{PlannedTexts: 4, CompletedTexts: 4, Seconds: 2, GuardSeconds: 1}, nil)
	if complete.Status != "completed" || complete.TextsPerSecond200 != 0 {
		t.Fatalf("short complete block: %+v", complete)
	}
	for _, report := range []Report{partial, complete, finish(Report{PlannedTexts: 1}, nil)} {
		encoded, err := json.Marshal(report)
		if err != nil || strings.Contains(string(encoded), "NaN") || strings.Contains(string(encoded), "Inf") {
			t.Fatalf("non-JSON denominator: %s %v", encoded, err)
		}
	}
}

func TestPoolingPreservesMaskAndShape(t *testing.T) {
	for _, model := range []string{"coderank", "nomic"} {
		hidden := make([]float32, 2*768)
		hidden[0], hidden[1], hidden[768] = 3, 4, 900
		mask, shape := []int64{1, 0}, []int64{1, 2}
		original := slices.Clone(hidden)
		pool(hidden, mask, shape, model)
		if !slices.Equal(hidden, original) || !slices.Equal(mask, []int64{1, 0}) || !slices.Equal(shape, []int64{1, 2}) {
			t.Fatal("pooling changed caller inputs")
		}
	}
}

func TestQueryBatchesRejectBeforeRuntime(t *testing.T) {
	previousArgs, previousFlags := os.Args, flag.CommandLine
	t.Cleanup(func() { os.Args, flag.CommandLine = previousArgs, previousFlags })
	batch := Batch{Indices: []int{0, 1}, Shape: []int64{2, 1}, Inputs: map[string][]int64{
		"input_ids": {1, 1}, "attention_mask": {1, 1}, "token_type_ids": {0, 0},
	}}
	encoded, err := json.Marshal([]Batch{batch})
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "tokens.json")
	if err := os.WriteFile(path, encoded, 0600); err != nil {
		t.Fatal(err)
	}
	flag.CommandLine = flag.NewFlagSet("probe", flag.ContinueOnError)
	os.Args = []string{"probe", "-model", "coderank", "-corpus", "query", "-tokens", path}
	if err := run(); err == nil || !strings.Contains(err.Error(), "one row per batch") {
		t.Fatalf("accepted batched query latency or reached runtime: %v", err)
	}
}

func TestLinuxRSSUsesProc(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("Linux /proc route; macOS runtime is checked separately")
	}
	current, peak, err := rss()
	if err != nil || current == 0 || peak < current {
		t.Fatalf("Linux current=%d peak=%d error=%v", current, peak, err)
	}
}
