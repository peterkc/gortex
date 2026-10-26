// onnxprobe measures the existing yalue runtime with exported HF token IDs.
package main

/*
#ifdef __APPLE__
#include <mach/mach.h>
#include <sys/resource.h>
#include <sys/sysctl.h>
#include <stdlib.h>
static unsigned long long current_rss(void) {
    struct mach_task_basic_info info;
    mach_msg_type_number_t count = MACH_TASK_BASIC_INFO_COUNT;
    if (task_info(mach_task_self(), MACH_TASK_BASIC_INFO,
                  (task_info_t)&info, &count) != KERN_SUCCESS) return 0;
    return info.resident_size;
}
static int memory_level(void) {
    int value = 0;
    size_t size = sizeof(value);
    if (sysctlbyname("kern.memorystatus_level", &value, &size, NULL, 0)) return -1;
    return value;
}
static unsigned long long peak_rss(void) {
    struct rusage usage;
    if (getrusage(RUSAGE_SELF, &usage)) return 0;
    return usage.ru_maxrss;
}
#else
static unsigned long long current_rss(void) { return 0; }
static unsigned long long peak_rss(void) { return 0; }
static int memory_level(void) { return -1; }
#endif
*/
import "C"

import (
	"crypto/sha256"
	"encoding/json"
	"flag"
	"fmt"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"time"

	ort "github.com/yalue/onnxruntime_go"
)

type Batch struct {
	Indices []int              `json:"indices"`
	Shape   []int64            `json:"shape"`
	Inputs  map[string][]int64 `json:"inputs"`
}

type Report struct {
	TokenSHA256       string      `json:"token_file_sha256"`
	Batches           int         `json:"batch_count"`
	RSSUnit           string      `json:"rss_unit"`
	Corpus            string      `json:"corpus"`
	Cap               int         `json:"token_cap"`
	Threads           int         `json:"intra_op_threads"`
	InterOpThreads    int         `json:"inter_op_threads"`
	Repeat            int         `json:"repeat"`
	PlannedRepeats    int         `json:"planned_repeats"`
	CompletedRepeats  int         `json:"completed_repeats"`
	GuardSeconds      float64     `json:"guard_seconds"`
	PlannedTexts      int         `json:"planned_texts"`
	PlannedTokens     int         `json:"planned_unpadded_tokens"`
	CompletedTexts    int         `json:"completed_texts"`
	CompletedTokens   int         `json:"completed_unpadded_tokens"`
	TextsPerSecond    float64     `json:"texts_per_second"`
	TokensPerSecond   float64     `json:"unpadded_tokens_per_second"`
	MinutesPer10k     float64     `json:"minutes_per_10k_symbols"`
	PeakRSS           uint64      `json:"peak_rss_bytes"`
	RSSScope          string      `json:"peak_rss_scope"`
	Status            string      `json:"status"`
	Reason            string      `json:"reason"`
	LatencySamples    int         `json:"query_latency_samples"`
	PercentileMethod  string      `json:"query_percentile_method"`
	QueryP50MS        float64     `json:"query_p50_ms"`
	QueryP95MS        float64     `json:"query_p95_ms"`
	Model             string      `json:"model"`
	ModelPath         string      `json:"model_path"`
	Mapped            bool        `json:"mapped"`
	DisablePrepacking bool        `json:"disable_prepacking"`
	Runtime           string      `json:"runtime"`
	LoadSeconds       float64     `json:"load_seconds"`
	RSSBefore         uint64      `json:"rss_before_load"`
	RSSLoaded         uint64      `json:"rss_after_load"`
	RSS200            uint64      `json:"rss_after_200"`
	RSSAll            uint64      `json:"rss_after_all"`
	Seconds200        float64     `json:"seconds_200"`
	TextsPerSecond200 float64     `json:"texts_per_second_200"`
	Seconds           float64     `json:"seconds"`
	UptimeBefore      string      `json:"uptime_before"`
	UptimeAfter       string      `json:"uptime_after"`
	Vectors           [][]float32 `json:"vectors"`
}

// pool returns new vectors; hidden and mask remain owned by the caller.
func pool(hidden []float32, mask []int64, shape []int64, model string) [][]float32 {
	batch, sequence := int(shape[0]), int(shape[1])
	vectors := make([][]float32, batch)
	for b := range batch {
		vector := make([]float32, 768)
		if model == "coderank" {
			copy(vector, hidden[b*sequence*768:(b*sequence+1)*768])
		} else {
			count := 0.0
			sums := make([]float64, 768)
			for t := range sequence {
				if mask[b*sequence+t] == 0 {
					continue
				}
				count++
				for d := range 768 {
					sums[d] += float64(hidden[(b*sequence+t)*768+d])
				}
			}
			mean := 0.0
			for d := range 768 {
				sums[d] /= count
				mean += sums[d] / 768
			}
			variance := 0.0
			for d := range 768 {
				sums[d] -= mean
				variance += sums[d] * sums[d] / 768
			}
			scale := math.Sqrt(variance + 1e-5)
			for d := range 768 {
				vector[d] = float32(sums[d] / scale)
			}
		}
		norm := 0.0
		for _, value := range vector {
			norm += float64(value) * float64(value)
		}
		norm = math.Sqrt(norm)
		for d := range vector {
			vector[d] = float32(float64(vector[d]) / norm)
		}
		vectors[b] = vector
	}
	return vectors
}

func runBatch(session *ort.DynamicAdvancedSession, batch Batch, model string) ([][]float32, error) {
	names := []string{"input_ids", "attention_mask", "token_type_ids"}
	values := make([]ort.Value, 0, len(names))
	for _, name := range names {
		value, err := ort.NewTensor(ort.Shape(batch.Shape), batch.Inputs[name])
		if err != nil {
			return nil, err
		}
		defer value.Destroy()
		values = append(values, value)
	}
	outputs := []ort.Value{nil}
	if err := session.Run(values, outputs); err != nil {
		return nil, err
	}
	defer outputs[0].Destroy()
	hidden, ok := outputs[0].(*ort.Tensor[float32])
	if !ok {
		return nil, fmt.Errorf("output is %T, want float32 tensor", outputs[0])
	}
	return pool(hidden.GetData(), batch.Inputs["attention_mask"], batch.Shape, model), nil
}

func dumpMap(path string) error {
	if runtime.GOOS != "darwin" {
		return nil
	}
	output, err := exec.Command("vmmap", fmt.Sprint(os.Getpid())).CombinedOutput()
	if err != nil {
		return fmt.Errorf("vmmap: %w: %s", err, output)
	}
	return os.WriteFile(path, output, 0644)
}

// procRSS parses Linux status units, not page counts or macOS ru_maxrss units.
func procRSS(status string) (uint64, uint64, error) {
	values := make(map[string]uint64)
	for _, line := range strings.Split(status, "\n") {
		fields := strings.Fields(line)
		if len(fields) == 0 || (fields[0] != "VmRSS:" && fields[0] != "VmHWM:") {
			continue
		}
		if len(fields) != 3 || fields[2] != "kB" || values[fields[0]] != 0 {
			return 0, 0, fmt.Errorf("invalid /proc RSS field")
		}
		value, err := strconv.ParseUint(fields[1], 10, 54)
		if err != nil || value == 0 {
			return 0, 0, fmt.Errorf("invalid /proc RSS value")
		}
		values[fields[0]] = value * 1024
	}
	current, peak := values["VmRSS:"], values["VmHWM:"]
	if current == 0 || peak < current {
		return 0, 0, fmt.Errorf("missing or inconsistent /proc RSS")
	}
	return current, peak, nil
}

func rss() (uint64, uint64, error) {
	if runtime.GOOS == "linux" {
		status, err := os.ReadFile("/proc/self/status")
		if err != nil {
			return 0, 0, err
		}
		return procRSS(string(status))
	}
	if runtime.GOOS != "darwin" {
		return 0, 0, fmt.Errorf("unsupported RSS platform %s", runtime.GOOS)
	}
	return uint64(C.current_rss()), uint64(C.peak_rss()), nil
}

func tokenCounts(batches []Batch, cap int) (int, int, error) {
	rows, tokens := 0, 0
	for _, batch := range batches {
		rows += len(batch.Indices)
	}
	seen := make([]bool, rows)
	for _, batch := range batches {
		if len(batch.Shape) != 2 || batch.Shape[0] != int64(len(batch.Indices)) || batch.Shape[0] == 0 || batch.Shape[1] <= 0 || batch.Shape[1] > int64(cap) {
			return 0, 0, fmt.Errorf("token batch shape/cap mismatch")
		}
		for _, name := range []string{"input_ids", "attention_mask", "token_type_ids"} {
			if len(batch.Inputs[name]) != len(batch.Indices)*int(batch.Shape[1]) {
				return 0, 0, fmt.Errorf("token shape mismatch: %s", name)
			}
		}
		for _, index := range batch.Indices {
			if index < 0 || index >= rows || seen[index] {
				return 0, 0, fmt.Errorf("duplicate or out-of-range row index")
			}
			seen[index] = true
		}
		for _, mask := range batch.Inputs["attention_mask"] {
			if mask != 0 && mask != 1 {
				return 0, 0, fmt.Errorf("invalid attention mask")
			}
			tokens += int(mask)
		}
	}
	if rows == 0 || tokens == 0 {
		return 0, 0, fmt.Errorf("empty token inputs")
	}
	return rows, tokens, nil
}

func finish(report Report, latencies []float64) Report {
	if report.CompletedTexts > 0 && report.Seconds > 0 {
		report.TextsPerSecond = float64(report.CompletedTexts) / report.Seconds
		report.TokensPerSecond = float64(report.CompletedTokens) / report.Seconds
		report.MinutesPer10k = report.Seconds * 10000 / float64(report.CompletedTexts) / 60
	}
	if report.CompletedTexts == report.PlannedTexts && report.Reason == "" {
		report.Status = "completed"
		if report.Repeat > 0 {
			report.CompletedRepeats = 1
		}
	} else {
		report.Status = "partial"
	}
	report.LatencySamples = len(latencies)
	if len(latencies) > 0 {
		report.PercentileMethod = "nearest-rank"
		ordered := slices.Sorted(slices.Values(latencies))
		report.QueryP50MS = ordered[int(math.Ceil(float64(len(ordered))*0.5))-1]
		report.QueryP95MS = ordered[int(math.Ceil(float64(len(ordered))*0.95))-1]
	}
	return report
}

func run() error {
	model := flag.String("model", "", "nomic or coderank")
	modelPath := flag.String("path", "", "ONNX or ORT model path")
	library := flag.String("library", "", "existing ONNX Runtime dylib path")
	tokensPath := flag.String("tokens", "", "exported token batch JSON")
	outputPath := flag.String("output", "", "report JSON path")
	mapped := flag.Bool("mmap", false, "map ORT-format model and borrow initializers")
	noPrepack := flag.Bool("no-prepack", false, "disable CPU weight prepacking")
	threads := flag.Int("threads", 2, "intra-op threads; inter-op stays 1")
	guard := flag.Duration("guard", 550*time.Second, "runner block guard; supervisor also bounds native calls")
	corpus := flag.String("corpus", "metadata", "metadata, body or query")
	cap := flag.Int("cap", 8192, "precomputed token cap")
	repeat := flag.Int("repeat", 1, "this fresh process repeat number; 0 is warm-up")
	plannedRepeats := flag.Int("planned-repeats", 3, "planned measured repeats")
	flag.Parse()
	if *threads < 1 || *guard <= 0 || *cap < 1 || *cap > 8192 || *repeat < 0 || *plannedRepeats < 1 || *repeat > *plannedRepeats {
		return fmt.Errorf("invalid threads, guard, cap or repeat")
	}
	if *corpus != "metadata" && *corpus != "body" && *corpus != "query" {
		return fmt.Errorf("invalid corpus")
	}
	if *model != "nomic" && *model != "coderank" {
		return fmt.Errorf("unknown model %q", *model)
	}
	if *mapped && filepath.Ext(*modelPath) != ".ort" {
		return fmt.Errorf("mmap option requires ORT format")
	}
	encoded, err := os.ReadFile(*tokensPath)
	if err != nil {
		return err
	}
	var batches []Batch
	if err := json.Unmarshal(encoded, &batches); err != nil {
		return err
	}
	rows, tokens, err := tokenCounts(batches, *cap)
	if err != nil {
		return err
	}
	for _, batch := range batches {
		if *corpus == "query" && len(batch.Indices) != 1 {
			return fmt.Errorf("query latency requires one row per batch")
		}
	}
	blockStart := time.Now()
	if runtime.GOOS == "darwin" && int(C.memory_level()) < 30 {
		return fmt.Errorf("memorystatus below 30 before load")
	}
	ort.SetSharedLibraryPath(*library)
	if err := ort.InitializeEnvironment(); err != nil {
		return err
	}
	defer ort.DestroyEnvironment()
	options, err := ort.NewSessionOptions()
	if err != nil {
		return err
	}
	defer options.Destroy()
	if err := options.SetIntraOpNumThreads(*threads); err != nil {
		return err
	}
	if err := options.SetInterOpNumThreads(1); err != nil {
		return err
	}
	if *mapped {
		for _, key := range []string{"session.use_memory_mapped_ort_model", "session.use_ort_model_bytes_for_initializers"} {
			if err := options.AddSessionConfigEntry(key, "1"); err != nil {
				return err
			}
		}
	}
	if *noPrepack {
		if err := options.AddSessionConfigEntry("session.disable_prepacking", "1"); err != nil {
			return err
		}
	}
	report := Report{Model: *model, ModelPath: *modelPath, Mapped: *mapped,
		DisablePrepacking: *noPrepack, Runtime: ort.GetVersion(), Vectors: make([][]float32, rows),
		TokenSHA256: fmt.Sprintf("%x", sha256.Sum256(encoded)), Batches: len(batches), RSSUnit: "bytes",
		Corpus: *corpus, Cap: *cap, Threads: *threads, InterOpThreads: 1,
		Repeat: *repeat, PlannedRepeats: *plannedRepeats, GuardSeconds: guard.Seconds(),
		PlannedTexts: rows, PlannedTokens: tokens,
		RSSScope: "process-lifetime high-water; Linux /proc/self/status VmHWM kB*1024; macOS getrusage bytes"}
	report.RSSBefore, _, err = rss()
	if err != nil {
		return err
	}
	start := time.Now()
	session, err := ort.NewDynamicAdvancedSession(*modelPath,
		[]string{"input_ids", "attention_mask", "token_type_ids"},
		[]string{"last_hidden_state"}, options)
	if err != nil {
		return err
	}
	defer session.Destroy()
	report.LoadSeconds = time.Since(start).Seconds()
	report.RSSLoaded, report.PeakRSS, err = rss()
	if err != nil {
		return err
	}
	if err := dumpMap(*outputPath + ".loaded.vmmap"); err != nil {
		return err
	}
	uptime, err := exec.Command("uptime").Output()
	if err != nil {
		return err
	}
	report.UptimeBefore = string(uptime)
	start = time.Now()
	count := 0
	latencies := []float64{}
	for _, batch := range batches {
		if runtime.GOOS == "darwin" && int(C.memory_level()) < 25 {
			return fmt.Errorf("memorystatus below 25 during inference")
		}
		if time.Since(blockStart) >= *guard {
			report.Reason = "runner guard exceeded"
			break
		}
		batchStart := time.Now()
		vectors, err := runBatch(session, batch, *model)
		if err != nil {
			return err
		}
		latencyMS := time.Since(batchStart).Seconds() * 1000
		for i, index := range batch.Indices {
			report.Vectors[index] = vectors[i]
		}
		count += len(batch.Indices)
		report.CompletedTexts = count
		for _, mask := range batch.Inputs["attention_mask"] {
			report.CompletedTokens += int(mask)
		}
		if *corpus == "query" {
			latencies = append(latencies, latencyMS)
		}
		if count == 200 {
			report.Seconds200 = time.Since(start).Seconds()
			report.RSS200, _, err = rss()
			if err != nil {
				return err
			}
		}
	}
	report.Seconds = time.Since(start).Seconds()
	if report.Seconds200 > 0 {
		report.TextsPerSecond200 = 200 / report.Seconds200
	}
	report.RSSAll, report.PeakRSS, err = rss()
	if err != nil {
		return err
	}
	report = finish(report, latencies)
	uptime, err = exec.Command("uptime").Output()
	if err != nil {
		return err
	}
	report.UptimeAfter = string(uptime)
	if err := dumpMap(*outputPath + ".finished.vmmap"); err != nil {
		return err
	}
	encoded, err = json.Marshal(report)
	if err != nil {
		return err
	}
	if err := os.WriteFile(*outputPath, encoded, 0644); err != nil {
		return err
	}
	if report.Status != "completed" {
		return fmt.Errorf("partial block: %s", report.Reason)
	}
	return nil
}

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
