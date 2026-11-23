package indexer

import (
	"context"
	"errors"
	"fmt"
	"iter"

	"github.com/zzet/gortex/internal/clones"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/graph/store_pebble"
)

// visitRebuildDirectCloneEdges composes the checked cold corpus and the
// ordered pair pass on a quiet working view. It does not project language
// coverage or write graph rows. The sink must discard tentative edges on
// error, including a later scratch-cleanup error.
func visitRebuildDirectCloneEdges(ctx context.Context, build *store_pebble.BuildStore,
	receipts iter.Seq2[store_pebble.ParseFacts, error], threshold float64, yield func(*graph.Edge) error,
) (store_pebble.RebuildClonePairStats, error) {
	view := build.RebuildView()
	_, stats, err := visitRebuildClonePairs(ctx, build, receipts, threshold, nil, func(pair clones.Pair) error {
		return yieldRebuildClonePairEdges(ctx, view, graph.EdgeSimilarTo, pair, yield)
	})
	return stats, err
}

type rebuildCloneRelationStats struct {
	Signatures      rebuildCloneSignatureCounts
	Direct          store_pebble.RebuildClonePairStats
	Diffused        int
	DiffusionCapped bool
}

// visitRebuildCloneRelations runs the checked cohort once through the pair
// visitor. The diffusion visitor collects those same direct pairs, then ranks
// them only after pair production and cleanup complete. Node and edge sinks
// must not modify the working view and must discard tentative rows on failure.
func visitRebuildCloneRelations(ctx context.Context, build *store_pebble.BuildStore,
	receipts iter.Seq2[store_pebble.ParseFacts, error], threshold float64,
	yieldNode func(*graph.Node) error, yieldEdge func(*graph.Edge) error,
) (rebuildCloneRelationStats, error) {
	view := build.RebuildView()
	var stats rebuildCloneRelationStats
	pairs, capped, err := visitRebuildDiffusedCloneEdges(ctx, build, func(stage func(clones.Pair) error) error {
		var err error
		stats.Signatures, stats.Direct, err = visitRebuildClonePairs(ctx, build, receipts, threshold, yieldNode, func(pair clones.Pair) error {
			if err := yieldRebuildClonePairEdges(ctx, view, graph.EdgeSimilarTo, pair, yieldEdge); err != nil {
				return err
			}
			return stage(pair)
		})
		return err
	}, yieldEdge)
	if err != nil {
		return rebuildCloneRelationStats{}, err
	}
	stats.Diffused, stats.DiffusionCapped = pairs, capped
	return stats, nil
}

func visitRebuildClonePairs(ctx context.Context, build *store_pebble.BuildStore,
	receipts iter.Seq2[store_pebble.ParseFacts, error], threshold float64,
	yieldNode func(*graph.Node) error, yieldPair func(clones.Pair) error,
) (rebuildCloneSignatureCounts, store_pebble.RebuildClonePairStats, error) {
	view := build.RebuildView()
	var signatures rebuildCloneSignatureCounts
	items := func(yield func(clones.Item, error) bool) {
		stopped := errors.New("clone pair consumer stopped")
		var err error
		signatures, err = visitRebuildCloneSignatures(ctx, view, receipts, func(row rebuildCloneInput, sig clones.Signature, kept bool) error {
			if yieldNode != nil {
				if err := yieldRebuildCloneNodeStamp(ctx, view, row, sig, kept, yieldNode); err != nil {
					return err
				}
			}
			if kept && !yield(clones.Item{ID: row.ID, Sig: sig, TokenCount: row.Tokens}, nil) {
				return stopped
			}
			return nil
		})
		if err != nil && err != stopped {
			yield(clones.Item{}, err)
		}
	}
	stats, err := build.VisitRebuildClonePairs(ctx, items, threshold, yieldPair)
	if err != nil {
		return rebuildCloneSignatureCounts{}, store_pebble.RebuildClonePairStats{}, err
	}
	return signatures, stats, nil
}

func yieldRebuildClonePairEdges(ctx context.Context, view store_pebble.RebuildView, kind graph.EdgeKind,
	pair clones.Pair, yield func(*graph.Edge) error,
) error {
	a, err := view.Node(ctx, pair.A)
	if err != nil {
		return err
	}
	b, err := view.Node(ctx, pair.B)
	if err != nil {
		return err
	}
	// Both endpoints were checked against a receipt before banking.
	// Losing one here violates the quiet-view contract.
	if a == nil || b == nil {
		return fmt.Errorf("%w: missing checked clone endpoint in pair %q, %q", store_pebble.ErrRebuildView, pair.A, pair.B)
	}
	if err := yield(cloneRelationEdge(kind, a, b, pair.Similarity)); err != nil {
		return err
	}
	return yield(cloneRelationEdge(kind, b, a, pair.Similarity))
}
