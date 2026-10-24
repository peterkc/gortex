package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"time"

	"hybridfollowup/internal/quantindex"
)

func summary(values []float64) map[string]any {
	copy := slices.Clone(values)
	slices.Sort(copy)
	p := func(q float64) float64 { return copy[int(q*float64(len(copy)-1))] }
	return map[string]any{"count": len(copy), "p50": p(.5), "p95": p(.95), "minimum": copy[0]}
}
func loadLine() string {
	b, err := exec.Command("uptime").Output()
	if err != nil {
		return err.Error()
	}
	return string(b)
}
func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
func run() error {
	home, err := os.UserHomeDir()
	if err != nil {
		return err
	}
	dataRoot := filepath.Join(home, ".prime", "research", "gortex", "storage-architecture", "tmp", "vector", "hybrid-followup")
	phase := flag.String("phase", "plan", "plan, build, query, dry")
	n := flag.Int("n", 10000, "vector count")
	dimensions := flag.Int("dimensions", 384, "384 or1024")
	workers := flag.Int("workers", 1, "1,4,8 scan goroutines")
	queries := flag.Int("queries", 256, "query count")
	repeat := flag.Int("repeat", 1, "timing repetition")
	directory := flag.String("data-dir", filepath.Join(dataRoot, "timing-data"), "owned data/output directory")
	flag.Parse()
	if *phase == "plan" {
		return json.NewEncoder(os.Stdout).Encode(map[string]any{"heavy_started": false, "blocks": 54, "sizes": []int{10000, 100000, 1000000}, "dimensions": []int{384, 1024}, "workers": []int{1, 4, 8}, "repeats": 3, "queries_per_block": 256, "clearance": "parent required before build/query"})
	}
	if *phase != "build" && *phase != "query" && *phase != "dry" {
		return fmt.Errorf("unknown phase")
	}
	if *queries < 1 || *queries > 1000 || *repeat < 1 || *repeat > 3 {
		return fmt.Errorf("invalid query count/repetition")
	}
	if *phase == "dry" {
		temporaryRoot := filepath.Join(dataRoot, "cache", "tmp")
		if err := os.MkdirAll(temporaryRoot, 0755); err != nil {
			return err
		}
		*n = 32
		*queries = 3
		temporary, err := os.MkdirTemp(temporaryRoot, "hybrid-followup-dry-")
		if err != nil {
			return err
		}
		defer os.RemoveAll(temporary)
		*directory = temporary
	}
	if err := os.MkdirAll(*directory, 0755); err != nil {
		return err
	}
	path := filepath.Join(*directory, fmt.Sprintf("int8-n%d-d%d.bin", *n, *dimensions))
	ctx := context.Background()
	seed := int64(20261023)
	if *phase == "build" || *phase == "dry" {
		start := time.Now()
		if err := quantindex.BuildGenerated(ctx, path, *n, *dimensions, seed); err != nil {
			return err
		}
		if *phase == "build" {
			return json.NewEncoder(os.Stdout).Encode(map[string]any{"phase": "build", "n": *n, "dimensions": *dimensions, "seed": seed, "wall_ns": time.Since(start).Nanoseconds()})
		}
	}
	index, err := quantindex.Open(path, "int8", 128)
	if err != nil {
		return err
	}
	defer index.Close()
	if index.Len() != *n {
		return fmt.Errorf("index count mismatch")
	}
	if *phase == "dry" {
		for _, w := range []int{1, 4, 8} {
			if _, err = index.SearchWorkers(ctx, quantindex.SyntheticVector(seed, 1000000000, *dimensions), 10, w); err != nil {
				return err
			}
		}
		return json.NewEncoder(os.Stdout).Encode(map[string]any{"dry_check": true, "n": 32, "dimensions": *dimensions, "ann_builds": 0})
	}
	queryVectors := make([][]float32, *queries)
	for i := range queryVectors {
		queryVectors[i] = quantindex.SyntheticVector(seed, 1000000000+i, *dimensions)
	}
	for i := range min(8, len(queryVectors)) {
		if _, err = index.SearchWorkers(ctx, queryVectors[i], 10, *workers); err != nil {
			return err
		}
	}
	before := loadLine()
	latency := make([]float64, len(queryVectors))
	for i, q := range queryVectors {
		start := time.Now()
		hits, err := index.SearchWorkers(ctx, q, 10, *workers)
		latency[i] = float64(time.Since(start).Nanoseconds())
		if err != nil {
			return err
		}
		if len(hits) != 10 {
			return fmt.Errorf("incomplete result")
		}
	}
	after := loadLine()
	out := map[string]any{"phase": "query", "n": *n, "dimensions": *dimensions, "workers": *workers, "queries": *queries, "repeat": *repeat, "latency_ns": summary(latency), "load_before": before, "load_after": after, "gomaxprocs": runtime.GOMAXPROCS(0), "candidate_pool": "global quantized top128, then original F32 rerank", "request_concurrency": 1, "warmup_queries": min(8, *queries)}
	f, err := os.OpenFile(filepath.Join(*directory, fmt.Sprintf("n%d-d%d-w%d-rep%d.json", *n, *dimensions, *workers, *repeat)), os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0644)
	if err != nil {
		return err
	}
	defer f.Close()
	return json.NewEncoder(f).Encode(out)
}
