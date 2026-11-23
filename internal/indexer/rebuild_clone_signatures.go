package indexer

import (
	"context"
	"fmt"
	"iter"

	"github.com/zzet/gortex/internal/clones"
	"github.com/zzet/gortex/internal/graph/store_pebble"
)

type rebuildCloneSignatureCounts struct {
	Corpus, Kept, Signed int
}

// visitRebuildCloneSignatures finalizes each body from the same immutable
// receipts and quiet working view in both passes. Only raw bodies feed the
// sketch, including those that later drop out. A signature-only input is
// already finalized and an item even when there are no raw bodies. A
// callback with kept=false is a finalized drop. All callbacks are tentative
// until the visitor returns nil; the callback must not mutate the view.
func visitRebuildCloneSignatures(ctx context.Context, view store_pebble.RebuildView,
	receipts iter.Seq2[store_pebble.ParseFacts, error], yield func(row rebuildCloneInput, sig clones.Signature, kept bool) error,
) (rebuildCloneSignatureCounts, error) {
	cms := clones.NewCMS(65536, 4)
	corpus, signed := 0, 0
	if err := visitRebuildCloneInputs(ctx, view, receipts, func(row rebuildCloneInput) error {
		if row.Signed != nil {
			signed++
			return nil
		}
		corpus++
		for _, shingle := range row.Shingles {
			cms.Add(shingle)
		}
		return ctx.Err()
	}); err != nil {
		return rebuildCloneSignatureCounts{}, err
	}
	if corpus == 0 && signed == 0 {
		return rebuildCloneSignatureCounts{}, nil
	}
	useFilter := corpus >= cmsMinCorpus
	threshold := max(uint32(1), uint32(float64(corpus)*cmsBoilerplateRatio))
	var counts rebuildCloneSignatureCounts
	if err := visitRebuildCloneInputs(ctx, view, receipts, func(row rebuildCloneInput) error {
		if row.Signed != nil {
			counts.Signed++
			return yield(row, *row.Signed, true)
		}
		sig, kept := computeCloneSigFromShingles(cms, threshold, useFilter, row.Shingles)
		counts.Corpus++
		if kept {
			counts.Kept++
		}
		return yield(row, sig, kept)
	}); err != nil {
		return rebuildCloneSignatureCounts{}, err
	}
	if counts.Corpus != corpus || counts.Signed != signed {
		return rebuildCloneSignatureCounts{}, fmt.Errorf("%w: clone cohort changed between passes: raw %d then %d, signed %d then %d", errParseEntryCorrupt, corpus, counts.Corpus, signed, counts.Signed)
	}
	return counts, nil
}
