package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"math"
	"math/rand"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"time"

	usearch "github.com/unum-cloud/usearch/golang"
	"hybridfollowup/internal/quality"
	"hybridfollowup/internal/quantindex"
	"hybridfollowup/internal/vector"
)

type options struct {
	Phase, Directory, Method, Index string
	M, Ef, K, Repeat, Workers       int
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
	var o options
	n := flag.Int("n", 0, "generated document count; phase generate only")
	flag.StringVar(&o.Phase, "phase", "plan", "plan,inspect,truth,build,int8-build,query,latency")
	flag.StringVar(&o.Directory, "dataset", filepath.Join(dataRoot, "real-100k-export", "beads"), "owned per-repository export")
	flag.StringVar(&o.Method, "method", "f32", "f32,i8,int8,exact")
	flag.StringVar(&o.Index, "index", "", "existing native graph for query only")
	flag.IntVar(&o.M, "m", 16, "16 or32")
	flag.IntVar(&o.Ef, "ef", 512, "512 or1024")
	flag.IntVar(&o.K, "k", 50, "I8 candidate count50,100,200")
	flag.IntVar(&o.Repeat, "repeat", 1, "insertion-order build1..5")
	flag.IntVar(&o.Workers, "workers", 1, "int8 scan workers1,4,8")
	flag.Parse()
	if o.Phase == "generate" {
		return generateInputs(o.Directory, *n)
	}
	if o.Phase == "plan" {
		return json.NewEncoder(os.Stdout).Encode(map[string]any{"heavy_started": false, "real_accuracy_queries": 1000, "native_m": []int{16, 32}, "search_ef": []int{512, 1024}, "repeats": 5, "i8_candidate_k": []int{50, 100, 200}, "i8_10k_repos": []string{"beads", "pi_agent_rust", "prime-agent"}, "ratio_definition": "paper Eq3; original cosine distances", "clearance": "parent required before truth/build/int8-build/query"})
	}
	if o.Method != "f32" && o.Method != "i8" && o.Method != "int8" && o.Method != "exact" {
		return fmt.Errorf("unknown method")
	}
	if (o.M != 16 && o.M != 32) || (o.Ef != 512 && o.Ef != 1024) || o.Repeat < 1 || o.Repeat > 5 || (o.K != 50 && o.K != 100 && o.K != 200) {
		return fmt.Errorf("unsupported native configuration")
	}
	if o.Method == "exact" && o.Phase != "latency" {
		return fmt.Errorf("exact method uses truth for accuracy, latency for timing")
	}
	d, err := loadMetadata(o.Directory)
	if err != nil {
		return err
	}
	if o.Phase == "inspect" {
		return json.NewEncoder(os.Stdout).Encode(map[string]any{"documents": len(d.Documents), "queries": len(d.Queries), "dimensions": dimensions, "keys": len(d.KeyToPosition), "ann_builds": 0, "full_scans": 0})
	}
	if err = verifyInputs(d); err != nil {
		return err
	}
	switch o.Phase {
	case "truth":
		return writeTruth(d)
	case "build":
		return buildNative(d, o)
	case "int8-build":
		return buildInt8(d)
	case "query":
		return queryIndex(d, o)
	case "latency":
		return latency(d, o)
	default:
		return fmt.Errorf("unknown phase")
	}
}
func writeNewJSON(path string, value any) error {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0644)
	if err != nil {
		return err
	}
	defer f.Close()
	if err = json.NewEncoder(f).Encode(value); err != nil {
		return err
	}
	return f.Sync()
}
func writeTruth(d metadata) error {
	path := filepath.Join(d.Directory, "truth.json")
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		return fmt.Errorf("truth output already exists or stat failed")
	}
	docs, err := openMatrix(filepath.Join(d.Directory, "document.f32"), len(d.Documents))
	if err != nil {
		return err
	}
	defer docs.Close()
	queries, err := openMatrix(filepath.Join(d.Directory, "query.f32"), len(d.Queries))
	if err != nil {
		return err
	}
	defer queries.Close()
	start := time.Now()
	truth, err := exactTruth(d, docs, queries)
	if err != nil {
		return err
	}
	if err = writeNewJSON(path, truth); err != nil {
		return err
	}
	return json.NewEncoder(os.Stdout).Encode(map[string]any{"truth_queries": len(truth.Queries), "seconds": time.Since(start).Seconds()})
}
func nativeDirectory(d metadata, o options) string {
	return filepath.Join(d.Directory, fmt.Sprintf("native-M%d-%s-rep%d", o.M, o.Method, o.Repeat))
}
func buildNative(d metadata, o options) error {
	if o.Method == "int8" {
		return fmt.Errorf("use int8-build for exhaustive scan")
	}
	directory := nativeDirectory(d, o)
	if err := os.Mkdir(directory, 0755); err != nil {
		return err
	}
	path := filepath.Join(directory, "index.bin")
	docs, err := openMatrix(filepath.Join(d.Directory, "document.f32"), len(d.Documents))
	if err != nil {
		return err
	}
	defer docs.Close()
	cfg := usearch.DefaultConfig(dimensions)
	cfg.Connectivity = uint(o.M)
	cfg.ExpansionAdd = 128
	if o.M == 32 {
		cfg.ExpansionAdd = 256
	}
	cfg.ExpansionSearch = 512
	cfg.Quantization = usearch.F32
	if o.Method == "i8" {
		cfg.Quantization = usearch.I8
	}
	index, err := usearch.NewIndex(cfg)
	if err != nil {
		return err
	}
	defer index.Destroy()
	if err = index.Reserve(uint(len(d.Documents))); err != nil {
		return err
	}
	if err = index.ChangeThreadsAdd(1); err != nil {
		return err
	}
	seed := int64(20261022 + o.Repeat)
	order := rand.New(rand.NewSource(seed)).Perm(len(d.Documents))
	before := loadLine()
	start := time.Now()
	for _, position := range order {
		v := docs.Vector(position)
		if _, err = newQuery(v); err != nil {
			return err
		}
		if err = index.Add(d.Documents[position].Key, v); err != nil {
			return err
		}
	}
	if err = index.Save(path); err != nil {
		return err
	}
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	err = f.Sync()
	f.Close()
	if err != nil {
		return err
	}
	elapsed := time.Since(start).Nanoseconds()
	if err = os.Chmod(path, 0444); err != nil {
		return err
	}
	sha, err := fileSHA(path)
	if err != nil {
		return err
	}
	st, err := os.Stat(path)
	if err != nil {
		return err
	}
	receipt := map[string]any{"native_version": usearch.Version(), "config": cfg, "repeat": o.Repeat, "order_seed": seed, "file_sha256": sha, "file_bytes": st.Size(), "build_save_fsync_ns": elapsed, "load_before": before, "load_after": loadLine(), "input_receipt_sha256": d.ReceiptSHA}
	if err = writeNewJSON(filepath.Join(directory, "build.json"), receipt); err != nil {
		return err
	}
	return json.NewEncoder(os.Stdout).Encode(receipt)
}
func buildInt8(d metadata) error {
	docs, err := openMatrix(filepath.Join(d.Directory, "document.f32"), len(d.Documents))
	if err != nil {
		return err
	}
	defer docs.Close()
	rows := make([]vector.Vector, len(d.Documents))
	for i, row := range d.Documents {
		rows[i] = vector.Vector{NodeID: row.ID, Values: docs.Vector(i)}
	}
	return quantindex.BuildDimensions(context.Background(), filepath.Join(d.Directory, "int8.bin"), "int8", rows, dimensions)
}
func loadLine() string {
	b, err := exec.Command("uptime").Output()
	if err != nil {
		return err.Error()
	}
	return string(b)
}
func stats(values []float64) map[string]any {
	sorted := slices.Clone(values)
	slices.Sort(sorted)
	var sum float64
	for _, v := range sorted {
		sum += v
	}
	return map[string]any{"count": len(sorted), "mean": sum / float64(len(sorted)), "min": sorted[0], "p50": sorted[(len(sorted)-1)/2], "p95": sorted[int(.95*float64(len(sorted)-1))]}
}
func rawRecall(got []hit, want []hit) float64 {
	found := 0
	for _, h := range want {
		if slices.ContainsFunc(got, func(g hit) bool { return g.Position == h.Position }) {
			found++
		}
	}
	return float64(found) / float64(len(want))
}
func positions(hits []hit) []int {
	out := make([]int, len(hits))
	for i, h := range hits {
		out[i] = h.Position
	}
	return out
}
func distances(hits []hit) []float64 {
	out := make([]float64, len(hits))
	for i, h := range hits {
		out[i] = max(0, 1-h.Score)
	}
	return out
}

func queryIndex(d metadata, o options) error {
	var truth truthFile
	if err := readJSON(filepath.Join(d.Directory, "truth.json"), &truth); err != nil {
		return err
	}
	if truth.ReceiptSHA != d.ReceiptSHA || len(truth.Queries) != len(d.Queries) {
		return fmt.Errorf("exact truth input drift")
	}
	k := 10
	taskK := 200
	if o.Method == "i8" {
		k = o.K
		taskK = o.K
	}
	if o.Method == "int8" {
		k = 128
		taskK = 128
	}
	resultPath := filepath.Join(d.Directory, fmt.Sprintf("result-%s-M%d-ef%d-k%d-rep%d.json", o.Method, o.M, o.Ef, k, o.Repeat))
	if _, err := os.Stat(resultPath); !os.IsNotExist(err) {
		return fmt.Errorf("result already exists or stat failed")
	}
	queries, err := openMatrix(filepath.Join(d.Directory, "query.f32"), len(d.Queries))
	if err != nil {
		return err
	}
	defer queries.Close()
	rssBefore, err := rssBytes()
	if err != nil {
		return err
	}
	searcher, err := openEngine(d, o)
	if err != nil {
		return err
	}
	defer searcher.Close()
	rssOpen, err := rssBytes()
	if err != nil {
		return err
	}
	var recalls, ratios, latencies, candidateRecalls []float64
	taskHits, exactHits, lost, rescued := 0, 0, 0, 0
	var perQuery []map[string]any
	before := loadLine()
	for qi, reference := range truth.Queries {
		if reference.QueryPosition != qi {
			return fmt.Errorf("query order drift")
		}
		q, err := newQuery(queries.Vector(qi))
		if err != nil {
			return err
		}
		start := time.Now()
		all, err := searcher.Search(q)
		latencies = append(latencies, float64(time.Since(start).Nanoseconds()))
		if err != nil {
			return err
		}
		raw, err := searcher.OriginalScores(q, all[:min(10, len(all))])
		if err != nil {
			return err
		}
		recall := rawRecall(raw, reference.Raw)
		ratio, err := quality.InverseRatio(distances(reference.Raw), distances(raw))
		if err != nil {
			return err
		}
		taskPositions, err := searcher.TaskPositions(q, all)
		if err != nil {
			return err
		}
		parents, err := quality.ParentTop10(taskPositions, d.Parents)
		if err != nil {
			return err
		}
		target := d.Queries[qi].ID
		taskHit := slices.Contains(parents, target)
		exactHit := slices.Contains(reference.Parents, target)
		if taskHit {
			taskHits++
		}
		if exactHit {
			exactHits++
		}
		if exactHit && !taskHit {
			lost++
		}
		if !exactHit && taskHit {
			rescued++
		}
		recalls = append(recalls, recall)
		ratios = append(ratios, ratio)
		candidateRecall := recall
		if o.Method != "f32" {
			candidateRecall = rawRecall(all, reference.Raw)
			candidateRecalls = append(candidateRecalls, candidateRecall)
			if math.Abs(candidateRecall-recall) > 1e-12 {
				return fmt.Errorf("rerank did not reach candidate recall upper bound")
			}
		}
		rawParents, err := quality.ParentTop10(positions(raw), d.Parents)
		if err != nil {
			return err
		}
		perQuery = append(perQuery, map[string]any{"query_id": target, "recall_at_10": recall, "inverse_ratio_at_10": ratio, "candidate_recall_10_at_k": candidateRecall, "task_hit_at_10": taskHit, "exact_task_hit_at_10": exactHit, "raw_top10_parent_hit": slices.Contains(rawParents, target), "returned_task_symbols": len(parents)})
	}
	fileComponents, err := searcher.FileComponents()
	if err != nil {
		return err
	}
	sr := stats(recalls)
	si := stats(ratios)
	n := float64(len(d.Queries))
	rssAfter, err := rssBytes()
	if err != nil {
		return err
	}
	out := map[string]any{"method": o.Method, "M": o.M, "ef_search": o.Ef, "candidate_k": k, "task_candidate_k": taskK, "repeat": o.Repeat, "documents": len(d.Documents), "queries": len(d.Queries), "input_receipt_sha256": d.ReceiptSHA, "recall_at_10": sr, "inverse_ratio_at_10": si, "recall_mean_ge_0.99": sr["mean"].(float64) >= .99, "recall_mean_ge_0.999": sr["mean"].(float64) >= .999, "task_hit_at_10": float64(taskHits) / n, "exact_task_hit_at_10": float64(exactHits) / n, "exact_task_hits_lost": lost, "task_hits_rescued": rescued, "latency_ns_noisy": stats(latencies), "timing_qualifies": false, "load_before": before, "load_after": loadLine(), "rss_before_bytes": rssBefore, "rss_open_bytes": rssOpen, "rss_after_bytes": rssAfter, "rss_includes_benchmark_metadata_and_truth": true, "per_query": perQuery, "F32_sidecar_required": o.Method == "i8", "required_file_components_bytes": fileComponents, "quality_only_original_f32_oracle_map": o.Method == "f32", "metric_timing": "accuracy harness interleaves untimed metrics; use latency phase on VM for clean timing"}
	if o.Method == "int8" {
		delete(out, "M")
		delete(out, "ef_search")
	}
	if len(candidateRecalls) > 0 {
		out["candidate_recall_10_at_k"] = stats(candidateRecalls)
	}
	return writeNewJSON(resultPath, out)
}

func latency(d metadata, o options) error {
	queries, err := openMatrix(filepath.Join(d.Directory, "query.f32"), len(d.Queries))
	if err != nil {
		return err
	}
	defer queries.Close()
	rssBefore, err := rssBytes()
	if err != nil {
		return err
	}
	searcher, err := openEngine(d, o)
	if err != nil {
		return err
	}
	defer searcher.Close()
	rssOpen, err := rssBytes()
	if err != nil {
		return err
	}
	count := min(256, len(d.Queries))
	prepared := make([]query, count)
	for i := range prepared {
		prepared[i], err = newQuery(queries.Vector(i))
		if err != nil {
			return err
		}
	}
	for i := range min(8, count) {
		if _, err = searcher.Search(prepared[i]); err != nil {
			return err
		}
	}
	before := loadLine()
	samples := make([]float64, count)
	for i, q := range prepared {
		start := time.Now()
		hits, err := searcher.Search(q)
		samples[i] = float64(time.Since(start).Nanoseconds())
		if err != nil {
			return err
		}
		if len(hits) < 10 {
			return fmt.Errorf("incomplete latency result")
		}
	}
	fileComponents, err := searcher.FileComponents()
	if err != nil {
		return err
	}
	after := loadLine()
	rssAfter, err := rssBytes()
	if err != nil {
		return err
	}
	return json.NewEncoder(os.Stdout).Encode(map[string]any{"phase": "latency", "method": o.Method, "M": o.M, "ef_search": o.Ef, "candidate_k": actualCandidateK(o), "queries": count, "latency_ns": stats(samples), "load_before": before, "load_after": after, "metrics_interleaved": false, "gomaxprocs": runtime.GOMAXPROCS(0), "scan_workers": o.Workers, "rss_before_bytes": rssBefore, "rss_open_bytes": rssOpen, "rss_after_bytes": rssAfter, "F32_sidecar_required": o.Method == "i8", "required_file_components_bytes": fileComponents})
}

func actualCandidateK(o options) int {
	switch o.Method {
	case "i8":
		return o.K
	case "int8":
		return 128
	default:
		return 10
	}
}
