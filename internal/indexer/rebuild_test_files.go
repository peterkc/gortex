package indexer

import (
	"context"
	"iter"

	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/graph/store_pebble"
	"github.com/zzet/gortex/internal/pathkey"
)

func runRebuildTestFiles(ctx context.Context, build *store_pebble.BuildStore,
	receipts iter.Seq2[store_pebble.ParseFacts, error], classifications map[string]rebuildClassificationFact,
) error {
	return build.RebuildTestFiles(ctx, receipts, rebuildCheckedSkips(classifications), IsTestFile, func(n *graph.Node) string {
		return detectTestRunnerForFileEdges(n, nil)
	}, func(n *graph.Node, e *graph.Edge) string {
		return detectRunnerFromImportEdgeSlice(n, []*graph.Edge{e})
	})
}

func rebuildCheckedSkips(classifications map[string]rebuildClassificationFact) iter.Seq[*graph.Node] {
	return func(yield func(*graph.Node) bool) {
		for key, fact := range classifications {
			if n := rebuildCheckedSkipNode(key, fact); n != nil && !yield(n) {
				return
			}
		}
	}
}

// The owner-checked classification is already validated by buildRebuildFileStages.
// Replay the same exact file row rather than trusting graph metadata as proof.
func rebuildCheckedSkipNode(key string, fact rebuildClassificationFact) *graph.Node {
	sf := skippedFile{relPath: pathkey.Normalize(key), lang: fact.Language, size: fact.Size, reason: fact.ContentSkipReason}
	switch {
	case fact.sizeSkipped():
		return sizeSkipNode(sf, fact.MaxFileSize)
	case sf.reason != "":
		return contentSkipNode(sf)
	default:
		return nil
	}
}
