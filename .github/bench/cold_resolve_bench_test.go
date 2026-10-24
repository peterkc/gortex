// Fork-only measurement for zzet/gortex#945; do not merge into main.
package indexer

import (
	"os"
	"testing"
	"time"

	"github.com/zzet/gortex/internal/config"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/parser"
	"github.com/zzet/gortex/internal/parser/languages"
	"go.uber.org/zap"
	"go.uber.org/zap/zaptest/observer"
)

// BenchmarkColdResolve945 indexes the same bounded main-source corpus on both
// revisions. Every iteration starts with a fresh graph, indexer and parser
// registry. The normal ns/op is the whole index; resolve-ns/op is only the
// production ResolveAll call, excluding parsing and later derived passes.
func BenchmarkColdResolve945(b *testing.B) {
	root := os.Getenv("GORTEX_BENCH_CORPUS")
	if root == "" {
		b.Fatal("GORTEX_BENCH_CORPUS is required")
	}
	var resolveTime time.Duration
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		g := graph.New()
		reg := parser.NewRegistry()
		languages.RegisterAll(reg)
		core, logs := observer.New(zap.InfoLevel)
		idx := New(g, reg, config.Default().Index, zap.New(core))
		stats, err := idx.Index(root)
		if err != nil {
			b.Fatal(err)
		}
		if stats.FileCount == 0 {
			b.Fatal("empty corpus")
		}
		entries := logs.FilterMessage("indexer: resolve pass").All()
		if len(entries) != 1 {
			b.Fatalf("got %d resolve records, want 1", len(entries))
		}
		elapsed, ok := entries[0].ContextMap()["elapsed"].(time.Duration)
		if !ok || elapsed <= 0 {
			b.Fatal("resolve duration is missing or nonpositive")
		}
		resolveTime += elapsed
		b.ReportMetric(float64(stats.FileCount), "files")
		b.ReportMetric(float64(g.NodeCount()), "nodes")
		b.ReportMetric(float64(g.EdgeCount()), "edges")
	}
	b.ReportMetric(float64(resolveTime.Nanoseconds())/float64(b.N), "resolve-ns/op")
}
