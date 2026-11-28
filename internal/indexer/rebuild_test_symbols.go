package indexer

import (
	"context"
	"iter"

	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/graph/store_pebble"
)

func runRebuildTestSymbols(ctx context.Context, build *store_pebble.BuildStore,
	receipts iter.Seq2[store_pebble.ParseFacts, error],
) error {
	return build.RebuildTestSymbols(ctx, receipts, IsTestFile, TestRole, AnnotationTestRole, AnnotationTestRunner, func(n *graph.Node) string {
		return detectTestRunnerForFileEdges(n, nil)
	})
}
