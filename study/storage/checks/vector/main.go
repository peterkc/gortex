package main

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"math"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"time"

	"gxstudy/adapter"
	"gxstudy/dataset"
)

// CI sets this with -ldflags '-X main.sourceRevision=<full reviewed commit>'.
var sourceRevision string

func main() {
	if err := dispatch(context.Background(), os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(2)
	}
}
func dispatch(ctx context.Context, args []string) error {
	if len(args) == 0 {
		return fmt.Errorf("command required: generate, run; internal child commands require a bound request")
	}
	switch args[0] {
	case "generate":
		return generateCLI(ctx, args[1:])
	case "run":
		return runCLI(args[1:])
	case "__summarize":
		if len(args) != 2 {
			return fmt.Errorf("summarizer requires one request path")
		}
		var request runConfig
		if err := readJSON(args[1], &request); err != nil {
			return err
		}
		return summarize(request)
	case "__engine", "__checker":
		if len(args) != 3 {
			return fmt.Errorf("child requires request and scan mode")
		}
		var request runConfig
		if err := readJSON(args[1], &request); err != nil {
			return err
		}
		ctx, cancel := context.WithDeadline(ctx, request.Deadline)
		defer cancel()
		runtime.GOMAXPROCS(request.Workers)
		if args[0] == "__engine" {
			return measure(ctx, request, args[2])
		}
		return check(ctx, request, args[2])
	default:
		return fmt.Errorf("unknown command %q", args[0])
	}
}
func generateCLI(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("generate", flag.ContinueOnError)
	out := fs.String("out", "", "fresh output directory")
	n := fs.Int("documents", 0, "explicit document count, at most 150000")
	q := fs.Int("queries", 0, "explicit saved query count")
	seed := fs.Int64("seed", 0, "explicit synthetic seed")
	if err := fs.Parse(args); err != nil {
		return err
	}
	seenSeed := false
	fs.Visit(func(f *flag.Flag) {
		if f.Name == "seed" {
			seenSeed = true
		}
	})
	if fs.NArg() != 0 || !seenSeed || !filepath.IsAbs(*out) || !validRevision(sourceRevision) {
		return fmt.Errorf("explicit absolute out, documents, queries, seed and build-time source revision required")
	}
	exe, err := os.Executable()
	if err != nil {
		return err
	}
	pin, err := dataset.Hash(exe)
	if err != nil {
		return err
	}
	return writeFixture(ctx, *out, sourceRevision, pin, *seed, *n, *q)
}

type sample struct {
	QueryID   string        `json:"query_id"`
	Phase     string        `json:"phase"`
	LatencyNS int64         `json:"latency_ns"`
	Hits      []adapter.Hit `json:"hits"`
}
type engineResult struct {
	Mode       string   `json:"mode"`
	Workers    int      `json:"workers"`
	Candidates int      `json:"candidates"`
	K          int      `json:"k"`
	Documents  int      `json:"documents"`
	Dimensions int      `json:"dimensions"`
	Samples    []sample `json:"samples"`
}
type checkResult struct {
	Exact      bool     `json:"exact"`
	Checked    int      `json:"checked"`
	Mismatches []string `json:"mismatched_query_ids"`
}

func measure(ctx context.Context, c runConfig, mode string) error {
	in, err := loadInput(ctx, c.Data, c.FixturePin)
	if err != nil {
		return err
	}
	if mode == "int8" {
		if err := in.matrix.buildInt8(ctx); err != nil {
			return err
		}
	}
	result := engineResult{Mode: mode, Workers: c.Workers, Candidates: c.Candidates, K: c.K, Documents: in.manifest.Documents, Dimensions: dimensions}
	for _, phase := range []string{"warmup", "measure"} {
		count := len(in.vectors)
		if phase == "warmup" {
			count = min(8, count)
		}
		for i := range count {
			start := time.Now()
			hits, err := in.matrix.search(ctx, in.vectors[i], c.K, c.Workers, c.Candidates, mode)
			elapsed := time.Since(start).Nanoseconds()
			if err != nil {
				return err
			}
			result.Samples = append(result.Samples, sample{in.queries[i].ID, phase, elapsed, hits})
		}
	}
	return writeJSON(filepath.Join(c.Out, mode+"-engine.json"), result)
}

func check(ctx context.Context, c runConfig, mode string) error {
	in, err := loadInput(ctx, c.Data, c.FixturePin)
	if err != nil {
		return err
	}
	var result engineResult
	if err := readJSON(filepath.Join(c.Out, mode+"-engine.json"), &result); err != nil {
		return err
	}
	checked, err := checkAnswers(ctx, in, c, mode, result)
	if err != nil {
		return err
	}
	return writeJSON(filepath.Join(c.Out, mode+"-check.json"), checked)
}
func checkAnswers(ctx context.Context, in input, c runConfig, mode string, result engineResult) (checkResult, error) {
	checked := checkResult{Exact: true, Mismatches: []string{}}
	if result.Mode != mode || result.Workers != c.Workers || result.Candidates != c.Candidates || result.K != c.K || result.Documents != in.manifest.Documents || result.Dimensions != dimensions {
		return checked, fmt.Errorf("engine result identity mismatch")
	}
	warm := min(8, len(in.vectors))
	if len(result.Samples) != warm+len(in.vectors) {
		return checked, fmt.Errorf("missing/extra query samples")
	}
	// Recompute each saved query once; inspect warm-up answers as well, outside timing.
	scratch := make([]adapter.Hit, len(in.matrix.ids))
	for i, q := range in.vectors {
		want, err := reference(ctx, in.matrix, q, c.K, scratch)
		if err != nil {
			return checked, err
		}
		positions := []int{warm + i}
		if i < warm {
			positions = append(positions, i)
		}
		for _, position := range positions {
			s := result.Samples[position]
			phase := "measure"
			if position < warm {
				phase = "warmup"
			}
			if s.QueryID != in.queries[i].ID || s.Phase != phase || s.LatencyNS <= 0 {
				return checked, fmt.Errorf("query identity/order/latency mismatch")
			}
			checked.Checked++
			if err := exactResults(want, s.Hits); err != nil {
				checked.Exact = false
				checked.Mismatches = append(checked.Mismatches, phase+":"+s.QueryID)
			}
		}
	}
	return checked, nil
}

func p95(samples []sample) (float64, error) {
	var ns []int64
	for _, sample := range samples {
		if sample.Phase == "measure" {
			if sample.LatencyNS <= 0 {
				return 0, fmt.Errorf("invalid latency")
			}
			ns = append(ns, sample.LatencyNS)
		}
	}
	if len(ns) == 0 {
		return 0, fmt.Errorf("no measured samples")
	}
	slices.Sort(ns)
	return float64(ns[int(math.Ceil(.95*float64(len(ns))))-1]) / 1e6, nil
}
func validSHA(pin string) bool { b, err := hex.DecodeString(pin); return err == nil && len(b) == 32 }
func validRevision(pin string) bool {
	b, err := hex.DecodeString(pin)
	return err == nil && (len(b) == 20 || len(b) == 32)
}
func writeJSON(path string, value any) (err error) {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, f.Close()) }()
	if err := json.NewEncoder(f).Encode(value); err != nil {
		return err
	}
	return f.Sync()
}
func readJSON(path string, value any) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	decoder := json.NewDecoder(io.LimitReader(f, 64<<20))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(value); err != nil {
		return err
	}
	var extra any
	if err := decoder.Decode(&extra); err != io.EOF {
		return fmt.Errorf("trailing or overlong JSON: %s", path)
	}
	return nil
}
