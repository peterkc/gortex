// onnxprobe measures the existing yalue runtime with exported HF token IDs.
package main

/*
#include <mach/mach.h>
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
*/
import "C"

import (
	"encoding/json"
	"flag"
	"fmt"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"time"

	ort "github.com/yalue/onnxruntime_go"
)

type Batch struct {
	Indices []int              `json:"indices"`
	Shape   []int64            `json:"shape"`
	Inputs  map[string][]int64 `json:"inputs"`
}

type Report struct {
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
	output, err := exec.Command("vmmap", fmt.Sprint(os.Getpid())).CombinedOutput()
	if err != nil {
		return fmt.Errorf("vmmap: %w: %s", err, output)
	}
	return os.WriteFile(path, output, 0644)
}

func run() error {
	model := flag.String("model", "", "nomic or coderank")
	modelPath := flag.String("path", "", "ONNX or ORT model path")
	library := flag.String("library", "", "existing ONNX Runtime dylib path")
	tokensPath := flag.String("tokens", "", "exported token batch JSON")
	outputPath := flag.String("output", "", "report JSON path")
	mapped := flag.Bool("mmap", false, "map ORT-format model and borrow initializers")
	noPrepack := flag.Bool("no-prepack", false, "disable CPU weight prepacking")
	flag.Parse()
	if *model != "nomic" && *model != "coderank" {
		return fmt.Errorf("unknown model %q", *model)
	}
	if *mapped && filepath.Ext(*modelPath) != ".ort" {
		return fmt.Errorf("mmap option requires ORT format")
	}
	if int(C.memory_level()) < 30 {
		return fmt.Errorf("memorystatus below 30 before load")
	}
	encoded, err := os.ReadFile(*tokensPath)
	if err != nil {
		return err
	}
	var batches []Batch
	if err := json.Unmarshal(encoded, &batches); err != nil {
		return err
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
	if err := options.SetIntraOpNumThreads(2); err != nil {
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
		DisablePrepacking: *noPrepack, Runtime: ort.GetVersion(), Vectors: make([][]float32, 400), RSSBefore: uint64(C.current_rss())}
	start := time.Now()
	session, err := ort.NewDynamicAdvancedSession(*modelPath,
		[]string{"input_ids", "attention_mask", "token_type_ids"},
		[]string{"last_hidden_state"}, options)
	if err != nil {
		return err
	}
	defer session.Destroy()
	report.LoadSeconds = time.Since(start).Seconds()
	report.RSSLoaded = uint64(C.current_rss())
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
	for _, batch := range batches {
		if int(C.memory_level()) < 25 {
			return fmt.Errorf("memorystatus below 25 during inference")
		}
		if time.Since(start) > 550*time.Second {
			return fmt.Errorf("inference exceeded 550 seconds")
		}
		vectors, err := runBatch(session, batch, *model)
		if err != nil {
			return err
		}
		for i, index := range batch.Indices {
			report.Vectors[index] = vectors[i]
		}
		count += len(batch.Indices)
		if count == 200 {
			report.Seconds200 = time.Since(start).Seconds()
			report.RSS200 = uint64(C.current_rss())
		}
	}
	report.Seconds = time.Since(start).Seconds()
	report.TextsPerSecond200 = 200 / report.Seconds200
	report.RSSAll = uint64(C.current_rss())
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
	return os.WriteFile(*outputPath, encoded, 0644)
}

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
