package indexer

import (
	"context"
	"fmt"
	"iter"

	"github.com/zzet/gortex/internal/clones"
	"github.com/zzet/gortex/internal/graph/store_pebble"
)

type rebuildCloneSignatureCounts struct {
	Corpus, Kept int
}

// visitRebuildCloneSignatures finalizes each body from the same immutable
// receipts and quiet working view in both passes. All eligible bodies feed
// the sketch, including those that later drop out. A callback with kept=false
// is a finalized drop, not a missing receipt. All callbacks are tentative
// until the visitor returns nil; the callback must not mutate the view.
func visitRebuildCloneSignatures(ctx context.Context, view store_pebble.RebuildView,
	receipts iter.Seq2[store_pebble.ParseFacts, error], yield func(row rebuildCloneInput, sig clones.Signature, kept bool) error,
) (rebuildCloneSignatureCounts, error) {
	cms := clones.NewCMS(65536, 4)
	corpus := 0
	if err := visitRebuildCloneInputs(ctx, view, receipts, func(row rebuildCloneInput) error {
		corpus++
		for _, shingle := range row.Shingles {
			cms.Add(shingle)
		}
		return ctx.Err()
	}); err != nil {
		return rebuildCloneSignatureCounts{}, err
	}
	if corpus == 0 {
		return rebuildCloneSignatureCounts{}, nil
	}
	useFilter := corpus >= cmsMinCorpus
	threshold := max(uint32(1), uint32(float64(corpus)*cmsBoilerplateRatio))
	var counts rebuildCloneSignatureCounts
	if err := visitRebuildCloneInputs(ctx, view, receipts, func(row rebuildCloneInput) error {
		sig, kept := computeCloneSigFromShingles(cms, threshold, useFilter, row.Shingles)
		counts.Corpus++
		if kept {
			counts.Kept++
		}
		return yield(row, sig, kept)
	}); err != nil {
		return rebuildCloneSignatureCounts{}, err
	}
	if counts.Corpus != corpus {
		return rebuildCloneSignatureCounts{}, fmt.Errorf("%w: clone cohort changed between passes: %d then %d", errParseEntryCorrupt, corpus, counts.Corpus)
	}
	return counts, nil
}
