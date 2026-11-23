package indexer

import (
	"context"
	"fmt"
	"iter"
	"slices"

	"github.com/zzet/gortex/internal/clones"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/graph/store_pebble"
)

// visitRebuildCloneNodeStamps prepares replacement metadata for the checked
// raw-body cohort only. It does not settle signature-only inputs or write to
// the working graph or shingle sidecar. Discard tentative rows on failure.
func visitRebuildCloneNodeStamps(ctx context.Context, build *store_pebble.BuildStore,
	receipts iter.Seq2[store_pebble.ParseFacts, error], yield func(*graph.Node) error,
) (rebuildCloneSignatureCounts, error) {
	view := build.RebuildView()
	return visitRebuildCloneSignatures(ctx, build, receipts, rebuildClonePredecessors{}, func(row rebuildCloneInput, sig clones.Signature, kept bool) error {
		return yieldRebuildCloneNodeStamp(ctx, view, row, sig, kept, yield)
	})
}

func yieldRebuildCloneNodeStamp(ctx context.Context, view store_pebble.RebuildView,
	row rebuildCloneInput, sig clones.Signature, kept bool, yield func(*graph.Node) error,
) error {
	if row.Signed != nil {
		return nil // native leaves signature-only metadata untouched
	}
	n, err := view.Node(ctx, row.ID)
	if err != nil {
		return err
	}
	if n == nil {
		return fmt.Errorf("%w: missing checked clone body %q", store_pebble.ErrRebuildView, row.ID)
	}
	// The receipt and sidecar have already matched. Check the live raw
	// metadata as well: native node finalization reads it, not an alias.
	shingles, ok := n.Meta[cloneShinglesMetaKey].([]uint64)
	tokens, tokenOK := n.Meta[cloneTokensMetaKey].(int)
	if !ok || !tokenOK || tokens != row.Tokens || !slices.Equal(shingles, row.Shingles) {
		return fmt.Errorf("%w: clone body metadata differs from receipt for %q", store_pebble.ErrRebuildView, row.ID)
	}
	delete(n.Meta, cloneShinglesMetaKey)
	if kept {
		n.Meta[cloneSigMetaKey] = clones.EncodeSignature(sig)
	} else {
		delete(n.Meta, cloneSigMetaKey)
	}
	return yield(n)
}
