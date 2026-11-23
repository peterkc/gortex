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
	items := func(yield func(clones.Item, error) bool) {
		stopped := errors.New("clone pair consumer stopped")
		_, err := visitRebuildCloneSignatures(ctx, view, receipts, func(row rebuildCloneInput, sig clones.Signature, kept bool) error {
			if kept && !yield(clones.Item{ID: row.ID, Sig: sig, TokenCount: row.Tokens}, nil) {
				return stopped
			}
			return nil
		})
		if err != nil && err != stopped {
			yield(clones.Item{}, err)
		}
	}
	return build.VisitRebuildClonePairs(ctx, items, threshold, func(pair clones.Pair) error {
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
		if err := yield(cloneRelationEdge(graph.EdgeSimilarTo, a, b, pair.Similarity)); err != nil {
			return err
		}
		return yield(cloneRelationEdge(graph.EdgeSimilarTo, b, a, pair.Similarity))
	})
}
