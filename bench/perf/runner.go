// runner.go — per-repo benchmark execution. Cold-index, search p95,
// impact p95/p99, incremental reindex, on-disk DB size. All
// measurements come from real indexer / query / analysis calls so
// the headline numbers are reproducible from the same code that ships
// in production.
package main

import (
	"crypto/rand"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"sort"
	"strings"
	"time"

	"go.uber.org/zap"

	"github.com/zzet/gortex/internal/analysis"
	"github.com/zzet/gortex/internal/config"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/indexer"
	"github.com/zzet/gortex/internal/parser"
	"github.com/zzet/gortex/internal/parser/languages"
	"github.com/zzet/gortex/internal/query"
	"github.com/zzet/gortex/internal/storage/backend"
)

// repoSpec names one benchmark target. Path is either an absolute /
// repo-relative directory (used as-is, no clone) or a clone target
// triple (slug, URL, optional branch).
type repoSpec struct {
	Slug   string
	URL    string
	Branch string
	// Path is set after cloning (or directly when Local is true).
	Path  string
	Local bool
}

// repoRow is the markdown / CSV / JSON row a single repo produces.
// All durations in milliseconds for table-friendliness; DBBytes in
// bytes (rendered as MB in markdown).
type repoRow struct {
	Slug             string  `json:"slug"`
	Path             string  `json:"path"`
	LoC              int     `json:"loc"`
	Files            int     `json:"files"`
	Nodes            int     `json:"nodes"`
	Edges            int     `json:"edges"`
	ColdIndexMs      float64 `json:"cold_index_ms"`
	SearchP50Ms      float64 `json:"search_p50_ms"`
	SearchP95Ms      float64 `json:"search_p95_ms"`
	ImpactP50Ms      float64 `json:"impact_p50_ms"`
	ImpactP95Ms      float64 `json:"impact_p95_ms"`
	ImpactP99Ms      float64 `json:"impact_p99_ms"`
	IncrementalMs    float64 `json:"incremental_ms"`
	DBBytes          int64   `json:"db_bytes"`
	RSSBytes         int64   `json:"rss_bytes"`
	BudgetViolations int     `json:"budget_violations"`
	Skipped          string  `json:"skipped,omitempty"`
	Error            string  `json:"error,omitempty"`
}

// runRepo executes the full bench against one repo. Returns a row
// with whatever measurements completed; partial failures populate
// Error / Skipped instead of aborting the whole run so a flaky repo
// doesn't kill the others.
func runRepo(spec repoSpec, queries []string, budget budgets) repoRow {
	row := repoRow{Slug: spec.Slug}

	// Resolve path: local repos are used as-is; remote URLs clone
	// (depth=1) into the configured cache.
	if !spec.Local {
		path, err := ensureCloned(spec)
		if err != nil {
			row.Error = fmt.Sprintf("clone: %v", err)
			return row
		}
		spec.Path = path
	}
	row.Path = spec.Path

	// --- 1. Cold-index ------------------------------------------
	// The bench indexes into a throwaway SQLite store because that is
	// what a daemon holds: cold-index time, search latency and the
	// on-disk size column all depend on the store, and an in-memory
	// graph would report a configuration nobody runs.
	storeDir, err := os.MkdirTemp("", "gortex-bench-perf-"+spec.Slug+"-*")
	if err != nil {
		row.Error = fmt.Sprintf("store dir: %v", err)
		return row
	}
	defer func() { _ = os.RemoveAll(storeDir) }()
	g, closeBackend, err := backend.OpenRaw("", filepath.Join(storeDir, "bench.sqlite"))
	if err != nil {
		row.Error = fmt.Sprintf("open store: %v", err)
		return row
	}
	defer func() { _ = closeBackend() }()

	reg := parser.NewRegistry()
	languages.RegisterAll(reg)
	cfg := config.Config{}
	idx := indexer.New(g, reg, cfg.Index, zap.NewNop())

	t0 := time.Now()
	indexRes, err := idx.Index(spec.Path)
	row.ColdIndexMs = msSince(t0)
	if err != nil {
		row.Error = fmt.Sprintf("index: %v", err)
		return row
	}
	row.Files = indexRes.FileCount
	row.Nodes = indexRes.NodeCount
	row.LoC = sumLOC(g)
	row.Edges = countEdges(g)

	// --- 2. Search p50/p95 --------------------------------------
	eng := query.NewEngine(g)
	eng.SetSearch(idx.Search())
	searchLatencies := make([]time.Duration, 0, len(queries))
	for _, q := range queries {
		t := time.Now()
		_ = eng.SearchSymbols(q, 20)
		searchLatencies = append(searchLatencies, time.Since(t))
	}
	row.SearchP50Ms = pctMs(searchLatencies, 50)
	row.SearchP95Ms = pctMs(searchLatencies, 95)

	// --- 3. Impact p50/p95/p99 ----------------------------------
	seeds := pickImpactSeeds(g, 10)
	impactLatencies := make([]time.Duration, 0, len(seeds))
	for _, id := range seeds {
		t := time.Now()
		_ = analysis.AnalyzeImpact(g, []string{id}, nil, nil)
		impactLatencies = append(impactLatencies, time.Since(t))
	}
	if len(impactLatencies) > 0 {
		row.ImpactP50Ms = pctMs(impactLatencies, 50)
		row.ImpactP95Ms = pctMs(impactLatencies, 95)
		row.ImpactP99Ms = pctMs(impactLatencies, 99)
	}

	// --- 4. Incremental reindex (touch 5 files) -----------------
	touched := pickIncrementalFiles(g, 5)
	for _, fp := range touched {
		_ = touchFile(fp)
	}
	t1 := time.Now()
	_, err = idx.Index(spec.Path)
	row.IncrementalMs = msSince(t1)
	if err != nil {
		// Incremental failure isn't fatal — record it but keep the
		// row's other measurements.
		row.Error = fmt.Sprintf("incremental: %v", err)
	}

	// --- 5. DB size ---------------------------------------------
	row.DBBytes = dbSizeBytes(storeDir)

	// --- 6. Resident memory -------------------------------------
	// Measured with the graph, indexer (search index) and query
	// engine all live — the same objects a `gortex daemon` holds for
	// this repo.
	row.RSSBytes = residentBytes()

	// --- 7. Budget gates ----------------------------------------
	if budget.ImpactP95Ms > 0 && row.ImpactP95Ms > budget.ImpactP95Ms {
		row.BudgetViolations++
	}
	if budget.SearchP95Ms > 0 && row.SearchP95Ms > budget.SearchP95Ms {
		row.BudgetViolations++
	}
	if budget.ColdIndexMs > 0 && row.ColdIndexMs > budget.ColdIndexMs {
		row.BudgetViolations++
	}

	return row
}

// --- helpers --------------------------------------------------------

// ensureCloned does `git clone --depth 1` of the repo into the cache
// dir if absent. Returns the local path. Idempotent — a present
// directory is reused (so re-runs don't re-clone).
func ensureCloned(spec repoSpec) (string, error) {
	dest := filepath.Join(benchCacheDir, spec.Slug)
	if st, err := os.Stat(dest); err == nil && st.IsDir() {
		// Heuristic: a non-empty dir is assumed to be a previous
		// clone. The user can `rm -rf` the slug to force a refresh.
		if entries, _ := os.ReadDir(dest); len(entries) > 0 {
			return dest, nil
		}
	}
	if spec.URL == "" {
		return "", fmt.Errorf("no URL configured for %q (and no local clone)", spec.Slug)
	}
	if err := os.MkdirAll(filepath.Dir(dest), 0o755); err != nil {
		return "", err
	}
	args := []string{"clone", "--depth", "1", "--quiet"}
	if spec.Branch != "" {
		args = append(args, "--branch", spec.Branch)
	}
	args = append(args, spec.URL, dest)
	cmd := exec.Command("git", args...)
	cmd.Stdout = io.Discard
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("git clone %s: %w", spec.URL, err)
	}
	return dest, nil
}

// sumLOC is a cheap LoC proxy: total Lines field across file-kind
// nodes. Not byte-exact (matches what the graph counted at index
// time), but stable across runs and that's what the bench needs.
func sumLOC(g graph.Store) int {
	total := 0
	for n := range g.NodesByKind(graph.KindFile) {
		if n == nil {
			continue
		}
		// Graph stores per-file line counts under Meta["lines"] when
		// available; fall back to 0 (matches what the indexer
		// produced — no extrapolation).
		if v, ok := n.Meta["lines"]; ok {
			switch x := v.(type) {
			case int:
				total += x
			case int64:
				total += int(x)
			case float64:
				total += int(x)
			}
		}
	}
	return total
}

// countEdges returns the store's total edge count. Asking the store
// for its own tally beats walking every node's out-edges: on a disk
// backend that would be one round-trip per node.
func countEdges(g graph.Store) int {
	return g.Stats().TotalEdges
}

// pickImpactSeeds returns up to n random function/method IDs from
// the graph — the impact bench targets symbols a real refactor
// would name. Deterministic by node-ID sort + crypto/rand pick so
// the same fixture produces consistent magnitudes (not byte-equal
// numbers, but the same shape).
func pickImpactSeeds(g graph.Store, n int) []string {
	var pool []string
	for _, kind := range []graph.NodeKind{graph.KindFunction, graph.KindMethod} {
		for node := range g.NodesByKind(kind) {
			if node == nil {
				continue
			}
			pool = append(pool, node.ID)
		}
	}
	sort.Strings(pool)
	if len(pool) <= n {
		return pool
	}
	out := make([]string, 0, n)
	used := make(map[int]struct{}, n)
	for len(out) < n {
		idx, err := randInt(len(pool))
		if err != nil {
			// Random failed — fall back to deterministic stride.
			idx = (len(out) * 7919) % len(pool)
		}
		if _, ok := used[idx]; ok {
			continue
		}
		used[idx] = struct{}{}
		out = append(out, pool[idx])
	}
	return out
}

// pickIncrementalFiles returns up to n file paths to touch for the
// incremental-reindex measurement.
func pickIncrementalFiles(g graph.Store, n int) []string {
	var pool []string
	for node := range g.NodesByKind(graph.KindFile) {
		if node == nil || node.FilePath == "" {
			continue
		}
		pool = append(pool, node.FilePath)
	}
	sort.Strings(pool)
	if len(pool) <= n {
		return pool
	}
	return pool[:n]
}

// touchFile bumps a file's mtime to "now" so the indexer treats it
// as changed on the next pass. Best-effort: missing files are
// silently skipped (a graph node referencing a deleted file is the
// indexer's problem, not ours).
func touchFile(path string) error {
	now := time.Now()
	err := os.Chtimes(path, now, now)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	return err
}

// dbSizeBytes returns the real on-disk footprint of the store built
// for this repo: every file in its directory, so the write-ahead log
// and any sidecar count toward the total the way they do in a user's
// state directory. Replaces an older per-node/per-edge byte estimate,
// which could only ever be a calibrated guess. Returns -1 when the
// directory can't be walked.
func dbSizeBytes(dir string) int64 {
	var total int64
	err := filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		info, ierr := d.Info()
		if ierr != nil {
			return ierr
		}
		total += info.Size()
		return nil
	})
	if err != nil {
		return -1
	}
	return total
}

// residentBytes returns the Go heap currently retained — the
// runtime.MemStats figure `gortex daemon status` reports as daemon
// memory. A forced GC first drops the previous repo's graph so the
// number reflects only this repo's retained graph + search index.
// True OS RSS adds a fixed Go-runtime overhead (stacks, mcache, code)
// on top that does not scale with repo size.
func residentBytes() int64 {
	runtime.GC()
	var m runtime.MemStats
	runtime.ReadMemStats(&m)
	return int64(m.HeapAlloc)
}

// pctMs returns the p-th percentile of a duration slice in
// milliseconds. Nearest-rank method: pct=50 on [a,b,c,d,e] → c.
// Returns 0 when the slice is empty.
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

func msSince(t time.Time) float64 {
	return float64(time.Since(t).Microseconds()) / 1000.0
}

// randInt returns a cryptographically-random int in [0, max). Used
// for impact-seed selection; deterministic-stride fallback at the
// caller covers crypto/rand failure (which should be ~impossible).
func randInt(max int) (int, error) {
	if max <= 0 {
		return 0, fmt.Errorf("randInt: max must be > 0")
	}
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		return 0, err
	}
	return int(binary.BigEndian.Uint64(b[:]) % uint64(max)), nil
}

// --- repo presets ---------------------------------------------------

// defaultRepoSet returns the four reference repos the published
// perf table covers. Linux is opt-in via --include-linux because
// it's multi-GB and a fresh clone would tank CI runs.
func defaultRepoSet(includeLinux bool) []repoSpec {
	repos := []repoSpec{
		{Slug: "gin", URL: "https://github.com/gin-gonic/gin.git"},
		{Slug: "nestjs", URL: "https://github.com/nestjs/nest.git"},
		{Slug: "react", URL: "https://github.com/facebook/react.git"},
	}
	if includeLinux {
		repos = append(repos, repoSpec{
			Slug:   "linux",
			URL:    "https://github.com/torvalds/linux.git",
			Branch: "master",
		})
	}
	return repos
}

// resolveRepoSpec parses a --repos token. Forms:
//   - "gin"                        → preset by slug
//   - "owner/repo"                 → github.com/owner/repo, slug=repo
//   - "https://example/repo.git"   → URL clone, slug=basename
//   - "local:/abs/path"            → local repo, slug=basename(path)
func resolveRepoSpec(token string) (repoSpec, error) {
	token = strings.TrimSpace(token)
	if token == "" {
		return repoSpec{}, fmt.Errorf("empty repo token")
	}
	if path, ok := strings.CutPrefix(token, "local:"); ok {
		if !filepath.IsAbs(path) {
			abs, err := filepath.Abs(path)
			if err != nil {
				return repoSpec{}, err
			}
			path = abs
		}
		return repoSpec{
			Slug:  filepath.Base(path),
			Path:  path,
			Local: true,
		}, nil
	}
	if strings.HasPrefix(token, "https://") || strings.HasPrefix(token, "git@") {
		slug := strings.TrimSuffix(filepath.Base(token), ".git")
		return repoSpec{Slug: slug, URL: token}, nil
	}
	// "owner/repo" shorthand for github.com.
	if strings.Contains(token, "/") {
		parts := strings.Split(token, "/")
		slug := parts[len(parts)-1]
		return repoSpec{
			Slug: slug,
			URL:  "https://github.com/" + token + ".git",
		}, nil
	}
	// Bare slug — look up in presets.
	for _, r := range defaultRepoSet(true) {
		if r.Slug == token {
			return r, nil
		}
	}
	return repoSpec{}, fmt.Errorf("unknown repo %q (use owner/repo, https://..., or local:/path)", token)
}
