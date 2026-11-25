// runner.go — per-query pipeline runners for the token-efficiency
// benchmark. Three pipelines, all measured against the same indexed
// repo:
//
//   - ripgrep+full-read: rg --files-with-matches → cat each hit
//   - ripgrep+context:   rg -n -B50 -A50 → use the printed context
//   - gortex:            search_symbols → get_symbol_source on the
//     top result(s); represents the agent path
//     the savings dashboard rewards
package main

import (
	"bytes"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"

	"go.uber.org/zap"

	"github.com/zzet/gortex/internal/config"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/indexer"
	"github.com/zzet/gortex/internal/parser"
	"github.com/zzet/gortex/internal/parser/languages"
	"github.com/zzet/gortex/internal/query"
	"github.com/zzet/gortex/internal/storage/backend"
	"github.com/zzet/gortex/internal/tokens"
)

// pipelineResult is the per-query outcome of one pipeline.
type pipelineResult struct {
	// Returned is the ordered list of file paths the pipeline
	// considered relevant. Used for recall computation against
	// ground truth (which is keyed on file paths to keep the
	// comparison cross-pipeline fair — ripgrep doesn't see symbols).
	Returned []string `json:"returned"`
	// Tokens is the cumulative tiktoken count of the response
	// content the pipeline produced (the bytes a real agent would
	// have to ingest).
	Tokens int `json:"tokens"`
	// PerFileTokens lets the recall@k-by-budget calculator walk the
	// returned list in order, accumulating tokens until the budget
	// is exhausted. Aligned with Returned by index.
	PerFileTokens []int `json:"per_file_tokens"`
	// Error captures pipeline failures (e.g. ripgrep missing,
	// indexing failed). Empty string on success.
	Error string `json:"error,omitempty"`
}

// runRipgrepFullRead executes `rg --files-with-matches` against the
// repo, then reads each hit file fully. Mirrors the naive agent
// strategy "grep for foo, then read every file that matches".
func runRipgrepFullRead(repoRoot, query string) pipelineResult {
	files, err := ripgrepFilesWithMatches(repoRoot, query)
	if err != nil {
		return pipelineResult{Error: err.Error()}
	}
	out := pipelineResult{Returned: files, PerFileTokens: make([]int, 0, len(files))}
	for _, f := range files {
		body, err := os.ReadFile(filepath.Join(repoRoot, f))
		if err != nil {
			out.PerFileTokens = append(out.PerFileTokens, 0)
			continue
		}
		n := tokens.Count(string(body))
		out.Tokens += n
		out.PerFileTokens = append(out.PerFileTokens, n)
	}
	return out
}

// runRipgrepContext executes `rg -n -B 50 -A 50 <pattern>` and
// counts the bytes the surrounding context produces. More realistic
// than full-read because real grep-driven agents don't usually read
// every byte of every file.
func runRipgrepContext(repoRoot, query string) pipelineResult {
	cmd := exec.Command("rg", "-n", "-B", "50", "-A", "50", "--no-heading", query, repoRoot)
	var buf bytes.Buffer
	cmd.Stdout = &buf
	cmd.Stderr = io.Discard
	err := cmd.Run()
	// rg exit code 1 = no matches (not an error for our purposes).
	if err != nil {
		if ee, ok := err.(*exec.ExitError); ok && ee.ExitCode() == 1 {
			return pipelineResult{}
		}
		return pipelineResult{Error: err.Error()}
	}
	// Parse the output: rg prints one line per match with the file
	// prefix "<path>:<line>:<text>". Group lines by file so the
	// recall computation sees one returned-entry per file.
	files := map[string]int{}
	order := []string{}
	for line := range strings.SplitSeq(buf.String(), "\n") {
		idx := strings.Index(line, ":")
		if idx <= 0 {
			continue
		}
		path := line[:idx]
		// rg with an absolute repoRoot prints absolute paths —
		// strip the prefix so recall comparison sees the same
		// shape as ripgrep --files-with-matches.
		path = strings.TrimPrefix(path, repoRoot+"/")
		if _, seen := files[path]; !seen {
			order = append(order, path)
		}
		files[path] += tokens.Count(line + "\n")
	}
	out := pipelineResult{Returned: order, PerFileTokens: make([]int, 0, len(order))}
	for _, p := range order {
		out.Tokens += files[p]
		out.PerFileTokens = append(out.PerFileTokens, files[p])
	}
	return out
}

// runGortex indexes the repo, runs search_symbols, and reads the
// source of the top-K results — the path the savings dashboard
// rewards.
func runGortex(repoRoot, q string, indexedRepo *indexedRepo, topK int) pipelineResult {
	if indexedRepo == nil || indexedRepo.engine == nil {
		return pipelineResult{Error: "gortex repo not indexed"}
	}
	nodes := indexedRepo.engine.SearchSymbols(q, topK)
	out := pipelineResult{Returned: make([]string, 0, len(nodes)), PerFileTokens: make([]int, 0, len(nodes))}
	seen := map[string]bool{}
	for _, n := range nodes {
		if n == nil {
			continue
		}
		// Returned uses the file path so the recall comparison
		// works against the file-path ground truth (same axis as
		// ripgrep).
		fp := n.FilePath
		if fp == "" || seen[fp] {
			continue
		}
		seen[fp] = true
		// Read the symbol's lines from disk so the token count
		// matches what an agent would ingest via get_symbol_source.
		body, err := readSymbolSource(repoRoot, n)
		if err != nil {
			out.PerFileTokens = append(out.PerFileTokens, 0)
			out.Returned = append(out.Returned, fp)
			continue
		}
		n := tokens.Count(body)
		out.Tokens += n
		out.PerFileTokens = append(out.PerFileTokens, n)
		out.Returned = append(out.Returned, fp)
	}
	return out
}

// indexedRepo bundles a store + engine for the gortex pipeline so we
// pay the index cost once per repo regardless of how many queries
// run. close releases the store and its temp directory.
type indexedRepo struct {
	graph  graph.Store
	engine *query.Engine
	close  func()
}

// indexRepoForBench builds a fresh gortex index of the repo into a
// throwaway SQLite store — the same backend the daemon serves, so the
// gortex row measures the search stack a user's agent would hit. Same
// shape as the reference-repo perf bench otherwise: quiet logger, real
// parser registry, no extras.
func indexRepoForBench(root string) (*indexedRepo, error) {
	tmpDir, err := os.MkdirTemp("", "gortex-bench-token-efficiency-*")
	if err != nil {
		return nil, err
	}
	g, closeBackend, err := backend.OpenRaw("", filepath.Join(tmpDir, "bench.sqlite"))
	if err != nil {
		_ = os.RemoveAll(tmpDir)
		return nil, err
	}
	closeStore := func() {
		_ = closeBackend()
		_ = os.RemoveAll(tmpDir)
	}
	reg := parser.NewRegistry()
	languages.RegisterAll(reg)
	cfg := config.Config{}
	idx := indexer.New(g, reg, cfg.Index, zap.NewNop())
	if _, err := idx.Index(root); err != nil {
		closeStore()
		return nil, fmt.Errorf("index %s: %w", root, err)
	}
	eng := query.NewEngine(g)
	eng.SetSearch(idx.Search())
	return &indexedRepo{graph: g, engine: eng, close: closeStore}, nil
}

// ripgrepFilesWithMatches runs `rg --files-with-matches <pattern>` and
// returns the matched paths relative to repoRoot.
func ripgrepFilesWithMatches(repoRoot, pattern string) ([]string, error) {
	cmd := exec.Command("rg", "--files-with-matches", pattern, repoRoot)
	var buf bytes.Buffer
	cmd.Stdout = &buf
	cmd.Stderr = io.Discard
	err := cmd.Run()
	if err != nil {
		if ee, ok := err.(*exec.ExitError); ok && ee.ExitCode() == 1 {
			// No matches — clean exit, empty result.
			return nil, nil
		}
		return nil, fmt.Errorf("rg: %w", err)
	}
	var out []string
	for line := range strings.SplitSeq(buf.String(), "\n") {
		if line == "" {
			continue
		}
		rel := strings.TrimPrefix(line, repoRoot+"/")
		out = append(out, rel)
	}
	sort.Strings(out)
	return out, nil
}

// readSymbolSource extracts the symbol's line range from its file.
// Falls back to the whole file when StartLine/EndLine aren't set —
// matches the fallback shape the MCP tool would have to produce.
func readSymbolSource(repoRoot string, n *graph.Node) (string, error) {
	if n == nil || n.FilePath == "" {
		return "", fmt.Errorf("no path")
	}
	path := n.FilePath
	if !filepath.IsAbs(path) {
		path = filepath.Join(repoRoot, path)
	}
	body, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	if n.StartLine <= 0 || n.EndLine <= 0 || n.EndLine < n.StartLine {
		return string(body), nil
	}
	lines := strings.Split(string(body), "\n")
	start := n.StartLine - 1
	end := n.EndLine
	if start < 0 {
		start = 0
	}
	if end > len(lines) {
		end = len(lines)
	}
	if start >= end {
		return "", nil
	}
	return strings.Join(lines[start:end], "\n"), nil
}

// recallAtBudget walks the pipeline's returned list in order,
// accumulating tokens until budget is exhausted, then computes
// |intersection(expected, returned_within_budget)| / |expected|.
// Returns 0 when expected is empty (no ground truth = no recall to
// measure, not 100%).
func recallAtBudget(r pipelineResult, expected []string, budgetTokens int) float64 {
	if len(expected) == 0 {
		return 0
	}
	exp := map[string]bool{}
	for _, e := range expected {
		exp[e] = true
	}
	cumulative := 0
	hits := 0
	for i, ret := range r.Returned {
		if i >= len(r.PerFileTokens) {
			break
		}
		next := cumulative + r.PerFileTokens[i]
		if budgetTokens > 0 && next > budgetTokens {
			// Including this entry would blow the budget; stop.
			break
		}
		cumulative = next
		if exp[ret] {
			hits++
		}
	}
	return float64(hits) / float64(len(expected))
}
