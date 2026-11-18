// Command daemon-latency measures per-tool MCP dispatch latency.
// Builds an in-process MCP server against a target corpus, fires N
// `CallTool` invocations per tool, reports p50 / p95 / p99 per tool
// and a top-line summary.
//
// What it measures: tool-handler latency end-to-end through the
// real MCP dispatch path (`Handler.CallTool` invoked via the same
// `server.MCPServer` the production stdio / HTTP / daemon
// front-ends use). What it does NOT measure: stdio framing,
// daemon socket dispatch, JSON-RPC envelope overhead. Those add a
// small constant per call (typically <1 ms on a warm pipe); the
// handler latency dominates the user-perceived response time.
//
// The bench therefore reflects "daemon-mode handler cost", which
// is the load-bearing number for the daemon-latency publication.
package main

import (
	"context"
	"encoding/csv"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"time"

	"go.uber.org/zap"

	"github.com/zzet/gortex/internal/config"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/indexer"
	gortexmcp "github.com/zzet/gortex/internal/mcp"
	"github.com/zzet/gortex/internal/parser"
	"github.com/zzet/gortex/internal/parser/languages"
	"github.com/zzet/gortex/internal/query"
	internalserver "github.com/zzet/gortex/internal/server"
	"github.com/zzet/gortex/internal/storage/backend"
)

// toolCall is one synthetic MCP request the bench fires per
// iteration. ArgsFn lets the bench vary the args across iterations
// (e.g. different query strings) so the dispatch path isn't
// trivially memoised by an upstream cache.
type toolCall struct {
	Tool    string
	ArgsFn  func(iter int) map[string]any
	WarmupN int
	IterN   int
	// SkipIfMissing lets a tool opt out when its substrate isn't
	// in the indexed graph (e.g. nothing to call get_callers on).
	SkipIfMissing func(g graph.Store) bool
}

// result captures the per-tool aggregate the bench publishes.
type result struct {
	Tool      string    `json:"tool"`
	Iters     int       `json:"iters"`
	P50Ms     float64   `json:"p50_ms"`
	P95Ms     float64   `json:"p95_ms"`
	P99Ms     float64   `json:"p99_ms"`
	MeanMs    float64   `json:"mean_ms"`
	MaxMs     float64   `json:"max_ms"`
	ErrorRate float64   `json:"error_rate"`
	Skipped   string    `json:"skipped,omitempty"`
	Started   time.Time `json:"-"`
}

func main() {
	repo := flag.String("repo", ".", "corpus to index for the bench")
	iter := flag.Int("iter", 200, "iterations per tool (warm-up of iter/10 is added on top)")
	out := flag.String("out", "", "primary output path (default stdout)")
	jsonOut := flag.String("json", "", "companion JSON metrics output")
	csvOut := flag.String("csv", "", "companion CSV output")
	format := flag.String("format", "markdown", "markdown | json | csv")
	tools := flag.String("tools", "", "comma-separated subset (default: all known tools)")
	flag.Parse()

	absRepo, err := filepath.Abs(*repo)
	if err != nil {
		die("repo path: %v", err)
	}

	fmt.Fprintf(os.Stderr, "[daemon-latency] indexing %s...\n", absRepo)
	g, srv, closeStore := buildInProcessServer(absRepo)
	defer closeStore()
	fmt.Fprintf(os.Stderr, "[daemon-latency] indexed %d nodes\n", g.NodeCount())

	handler := internalserver.NewHandler(srv.MCPServer(), g, "bench", zap.NewNop())

	// Build the call set against the freshly indexed graph so each
	// synthetic request has at least some structural validity (a
	// real symbol id, an extant file path).
	calls := defaultCalls(g, *iter)
	if *tools != "" {
		calls = filterCalls(calls, strings.Split(*tools, ","))
	}

	rows := make([]result, 0, len(calls))
	for _, c := range calls {
		if c.SkipIfMissing != nil && c.SkipIfMissing(g) {
			rows = append(rows, result{Tool: c.Tool, Iters: 0, Skipped: "no eligible substrate in indexed graph"})
			fmt.Fprintf(os.Stderr, "[daemon-latency] %-22s skipped (no substrate)\n", c.Tool)
			continue
		}
		row := runOne(handler, c)
		rows = append(rows, row)
		fmt.Fprintf(os.Stderr, "[daemon-latency] %-22s p50=%6.2fms p95=%6.2fms p99=%6.2fms iters=%d errs=%.0f%%\n",
			c.Tool, row.P50Ms, row.P95Ms, row.P99Ms, row.Iters, row.ErrorRate*100)
	}

	var primary []byte
	switch strings.ToLower(*format) {
	case "markdown", "md":
		primary = []byte(renderMarkdown(rows, absRepo, g))
	case "csv":
		primary = []byte(renderCSV(rows))
	case "json":
		primary = mustMarshalJSON(rows)
	default:
		die("unknown --format %q", *format)
	}
	if err := writeOutput(*out, primary); err != nil {
		die("write output: %v", err)
	}
	if *csvOut != "" {
		if err := writeOutput(*csvOut, []byte(renderCSV(rows))); err != nil {
			die("write csv: %v", err)
		}
	}
	if *jsonOut != "" {
		if err := writeOutput(*jsonOut, mustMarshalJSON(rows)); err != nil {
			die("write json: %v", err)
		}
	}
}

// --- in-process server ---------------------------------------------

// buildInProcessServer wires the same Server the production stdio /
// daemon front-ends use, against a throwaway SQLite store holding a
// fresh index of repoRoot. The store backend is load-bearing for this
// bench: handler latency includes the store's own reads and its native
// full-text search, so measuring against anything the daemon does not
// serve would publish numbers nobody can reproduce.
//
// The returned cleanup closes the store and removes its temp directory.
func buildInProcessServer(repoRoot string) (graph.Store, *gortexmcp.Server, func()) {
	tmpDir, err := os.MkdirTemp("", "gortex-bench-daemon-latency-*")
	if err != nil {
		die("temp store dir: %v", err)
	}
	g, closeBackend, err := backend.OpenRaw("", filepath.Join(tmpDir, "bench.sqlite"))
	if err != nil {
		_ = os.RemoveAll(tmpDir)
		die("open store: %v", err)
	}
	cleanup := func() {
		_ = closeBackend()
		_ = os.RemoveAll(tmpDir)
	}
	reg := parser.NewRegistry()
	languages.RegisterAll(reg)
	cfg := config.Config{}
	idx := indexer.New(g, reg, cfg.Index, zap.NewNop())
	if _, err := idx.Index(repoRoot); err != nil {
		cleanup()
		die("index %s: %v", repoRoot, err)
	}
	eng := query.NewEngine(g)
	eng.SetSearch(idx.Search())
	srv := gortexmcp.NewServer(eng, g, idx, nil, zap.NewNop(), cfg.Guards.Rules)
	srv.RunAnalysis()
	return g, srv, cleanup
}

// --- call set -------------------------------------------------------

// defaultCalls returns the canonical bench surface. We focus on
// tools agents actually call in production (the headline savings
// drivers) — covering both cheap (graph_stats) and expensive
// (smart_context) shapes so the published table shows the spread.
func defaultCalls(g graph.Store, iter int) []toolCall {
	if iter <= 0 {
		iter = 200
	}
	warmup := max(iter/10, 5)

	// Pick representative symbol IDs / file paths from the indexed
	// graph so the synthetic requests have real targets. Iterating
	// per kind rather than over every node keeps a disk-backed store
	// from materialising the whole corpus just to find two samples.
	var sampleFnID, sampleFilePath string
	for _, kind := range []graph.NodeKind{graph.KindFunction, graph.KindMethod} {
		for n := range g.NodesByKind(kind) {
			if n != nil && n.ID != "" {
				sampleFnID = n.ID
				break
			}
		}
		if sampleFnID != "" {
			break
		}
	}
	for n := range g.NodesByKind(graph.KindFile) {
		if n != nil && n.FilePath != "" {
			sampleFilePath = n.FilePath
			break
		}
	}

	queries := []string{
		"validateToken", "Indexer", "search", "newServer",
		"handler", "config", "graph", "rerank", "query", "savings",
	}

	return []toolCall{
		{
			Tool:    "graph_stats",
			ArgsFn:  func(_ int) map[string]any { return map[string]any{} },
			WarmupN: warmup, IterN: iter,
		},
		{
			Tool: "search_symbols",
			ArgsFn: func(i int) map[string]any {
				return map[string]any{
					"query": queries[i%len(queries)],
					"limit": float64(20),
				}
			},
			WarmupN: warmup, IterN: iter,
		},
		{
			Tool: "get_symbol_source",
			ArgsFn: func(_ int) map[string]any {
				return map[string]any{"id": sampleFnID}
			},
			WarmupN: warmup, IterN: iter,
			SkipIfMissing: func(g graph.Store) bool { return sampleFnID == "" },
		},
		{
			Tool: "get_callers",
			ArgsFn: func(_ int) map[string]any {
				return map[string]any{"id": sampleFnID, "limit": float64(50)}
			},
			WarmupN: warmup, IterN: iter,
			SkipIfMissing: func(g graph.Store) bool { return sampleFnID == "" },
		},
		{
			Tool: "find_usages",
			ArgsFn: func(_ int) map[string]any {
				return map[string]any{"id": sampleFnID}
			},
			WarmupN: warmup, IterN: iter,
			SkipIfMissing: func(g graph.Store) bool { return sampleFnID == "" },
		},
		{
			Tool: "get_file_summary",
			ArgsFn: func(_ int) map[string]any {
				return map[string]any{"path": sampleFilePath}
			},
			WarmupN: warmup, IterN: iter,
			SkipIfMissing: func(g graph.Store) bool { return sampleFilePath == "" },
		},
		{
			Tool: "smart_context",
			ArgsFn: func(i int) map[string]any {
				return map[string]any{"task": "find " + queries[i%len(queries)]}
			},
			// smart_context is heavy — fewer iterations so the
			// whole bench stays reasonable. Still produces a
			// credible p50/p95 with 30-50 samples.
			WarmupN: 3, IterN: iter / 5,
		},
		{
			Tool: "get_repo_outline",
			ArgsFn: func(_ int) map[string]any {
				return map[string]any{}
			},
			WarmupN: warmup, IterN: iter,
		},
	}
}

func filterCalls(calls []toolCall, names []string) []toolCall {
	want := map[string]bool{}
	for _, n := range names {
		want[strings.TrimSpace(n)] = true
	}
	out := make([]toolCall, 0, len(calls))
	for _, c := range calls {
		if want[c.Tool] {
			out = append(out, c)
		}
	}
	return out
}

// --- run loop -------------------------------------------------------

func runOne(handler *internalserver.Handler, c toolCall) result {
	ctx := context.Background()
	// Warm-up: prime any lazy initialisation in the handler /
	// graph so the measured iterations are steady-state.
	for i := range c.WarmupN {
		_, _ = handler.CallToolStrict(ctx, c.Tool, c.ArgsFn(i))
	}

	latencies := make([]time.Duration, 0, c.IterN)
	errors := 0
	for i := range c.IterN {
		t := time.Now()
		_, err := handler.CallToolStrict(ctx, c.Tool, c.ArgsFn(i))
		latencies = append(latencies, time.Since(t))
		if err != nil {
			errors++
		}
	}
	r := result{
		Tool:  c.Tool,
		Iters: c.IterN,
	}
	if len(latencies) > 0 {
		r.P50Ms = pctMs(latencies, 50)
		r.P95Ms = pctMs(latencies, 95)
		r.P99Ms = pctMs(latencies, 99)
		r.MaxMs = pctMs(latencies, 100)
		r.MeanMs = meanMs(latencies)
		r.ErrorRate = float64(errors) / float64(len(latencies))
	}
	return r
}

func pctMs(xs []time.Duration, pct int) float64 {
	if len(xs) == 0 {
		return 0
	}
	sorted := make([]time.Duration, len(xs))
	copy(sorted, xs)
	slices.Sort(sorted)
	idx := (pct * len(sorted)) / 100
	if idx >= len(sorted) {
		idx = len(sorted) - 1
	}
	return float64(sorted[idx].Microseconds()) / 1000.0
}

func meanMs(xs []time.Duration) float64 {
	if len(xs) == 0 {
		return 0
	}
	var sum time.Duration
	for _, x := range xs {
		sum += x
	}
	avg := sum / time.Duration(len(xs))
	return float64(avg.Microseconds()) / 1000.0
}

// --- rendering ------------------------------------------------------

func renderMarkdown(rows []result, repoRoot string, g graph.Store) string {
	var b strings.Builder
	fmt.Fprintln(&b, "# Daemon-mode MCP-tool latency")
	fmt.Fprintln(&b)
	fmt.Fprintf(&b, "_Corpus: `%s` (%d nodes). In-process handler dispatch — measures `Handler.CallToolStrict` end-to-end. Daemon socket overhead adds typically <1 ms on a warm pipe; the handler latency below dominates user-perceived response time._\n",
		repoRoot, g.NodeCount())
	fmt.Fprintln(&b)
	fmt.Fprintln(&b, "| tool | iters | p50 | p95 | p99 | mean | max | errors |")
	fmt.Fprintln(&b, "|------|------:|----:|----:|----:|-----:|----:|-------:|")
	for _, r := range rows {
		if r.Skipped != "" {
			fmt.Fprintf(&b, "| %s | — | — | — | — | — | — | skipped: %s |\n", r.Tool, r.Skipped)
			continue
		}
		fmt.Fprintf(&b, "| %s | %d | %s | %s | %s | %s | %s | %.0f%% |\n",
			r.Tool, r.Iters,
			fmtMs(r.P50Ms), fmtMs(r.P95Ms), fmtMs(r.P99Ms),
			fmtMs(r.MeanMs), fmtMs(r.MaxMs),
			r.ErrorRate*100,
		)
	}
	fmt.Fprintln(&b)
	fmt.Fprintln(&b, summary(rows))
	return b.String()
}

func renderCSV(rows []result) string {
	var b strings.Builder
	w := csv.NewWriter(&b)
	_ = w.Write([]string{"tool", "iters", "p50_ms", "p95_ms", "p99_ms", "mean_ms", "max_ms", "error_rate", "skipped"})
	for _, r := range rows {
		_ = w.Write([]string{
			r.Tool,
			fmt.Sprintf("%d", r.Iters),
			fmt.Sprintf("%.3f", r.P50Ms),
			fmt.Sprintf("%.3f", r.P95Ms),
			fmt.Sprintf("%.3f", r.P99Ms),
			fmt.Sprintf("%.3f", r.MeanMs),
			fmt.Sprintf("%.3f", r.MaxMs),
			fmt.Sprintf("%.4f", r.ErrorRate),
			r.Skipped,
		})
	}
	w.Flush()
	return b.String()
}

func mustMarshalJSON(rows []result) []byte {
	b, err := json.MarshalIndent(rows, "", "  ")
	if err != nil {
		die("marshal json: %v", err)
	}
	return append(b, '\n')
}

func summary(rows []result) string {
	ran := 0
	var p95s, p99s []float64
	for _, r := range rows {
		if r.Skipped != "" {
			continue
		}
		ran++
		p95s = append(p95s, r.P95Ms)
		p99s = append(p99s, r.P99Ms)
	}
	if ran == 0 {
		return "_no tools ran (all skipped)_"
	}
	sort.Float64s(p95s)
	sort.Float64s(p99s)
	medianP95 := p95s[len(p95s)/2]
	medianP99 := p99s[len(p99s)/2]
	return fmt.Sprintf("**Summary:** %d/%d tools ran. Median p95 across tools: %s. Median p99: %s.",
		ran, len(rows), fmtMs(medianP95), fmtMs(medianP99))
}

func fmtMs(v float64) string {
	switch {
	case v == 0:
		return "—"
	case v < 1.0:
		return fmt.Sprintf("%.2fms", v)
	case v < 1000:
		return fmt.Sprintf("%.1fms", v)
	default:
		return fmt.Sprintf("%.2fs", v/1000.0)
	}
}

// --- helpers --------------------------------------------------------

func die(format string, args ...any) {
	fmt.Fprintf(os.Stderr, "daemon-latency: "+format+"\n", args...)
	os.Exit(1)
}

func writeOutput(path string, body []byte) error {
	if path == "" {
		_, err := os.Stdout.Write(body)
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	return os.WriteFile(path, body, 0o644)
}
