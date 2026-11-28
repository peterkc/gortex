package indexer

import (
	"context"
	"fmt"
	"iter"

	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/graph/store_pebble"
)

func runRebuildArgOf(ctx context.Context, build *store_pebble.BuildStore, receipts iter.Seq2[store_pebble.ParseFacts, error]) error {
	return build.RebuildArgOf(ctx, receipts, argOfRewriteTarget, intFromMeta)
}

func runRebuildReturnsTo(ctx context.Context, build *store_pebble.BuildStore, receipts iter.Seq2[store_pebble.ParseFacts, error]) error {
	return build.RebuildReturnsTo(ctx, receipts, func(e *graph.Edge) (string, int, string, bool) {
		caller, line, text, ok := returnsToRewriteTarget(e)
		return caller, line, recordedCallTargetName(text), ok
	}, resolvedCallTargetName)
}

// Read only the base of the same entries already checked during admission.
// The store still checks the envelope. No checkout read or extraction occurs.
func (idx *Indexer) rebuildDataflowReceipts(ctx context.Context, job rebuildFileStages, store rebuildParseStore,
	census *rebuildExtractorCensus,
) iter.Seq2[store_pebble.ParseFacts, error] {
	return idx.rebuildSelectedDataflowReceipts(ctx, job, store, census, nil)
}

// The non-nil isolated build selects before opening the receipt, then removes
// any remaining claims on held-out winners from this supported receipt.
func (idx *Indexer) rebuildSelectedDataflowReceipts(ctx context.Context, job rebuildFileStages, store rebuildParseStore,
	census *rebuildExtractorCensus, isolated *store_pebble.BuildStore,
) iter.Seq2[store_pebble.ParseFacts, error] {
	return func(yield func(store_pebble.ParseFacts, error) bool) {
		for _, input := range job.inputs {
			if err := ctx.Err(); err != nil {
				yield(store_pebble.ParseFacts{}, err)
				return
			}
			if isolated != nil {
				held, err := census.holdsOutLinkInput(input.Path)
				if err != nil {
					yield(store_pebble.ParseFacts{}, err)
					return
				}
				if held {
					continue
				}
			}
			recipe := idx.rebuildExtractorRecipe(job.recipes[input.Language], input)
			raw, err := store.read(input.parseKey(recipe))
			if err != nil {
				yield(store_pebble.ParseFacts{}, fmt.Errorf("%w: reload %q for dataflow lift: %w", errParseEntryCorrupt, input.Path, err))
				return
			}
			facts, err := store_pebble.DecodeContractParseFactsBase(raw)
			if err != nil {
				yield(store_pebble.ParseFacts{}, fmt.Errorf("%w: decode %q for dataflow lift: %w", errParseEntryCorrupt, input.Path, err))
				return
			}
			if facts.File.FilePath != input.Path || facts.EffectiveLanguage != census.byPath[input.Path] {
				yield(store_pebble.ParseFacts{}, fmt.Errorf("%w: receipt attribution changed for %q", errParseEntryCorrupt, input.Path))
				return
			}
			if isolated != nil {
				facts, err = isolated.SelectRebuildLinkFacts(ctx, facts)
				if err != nil {
					yield(store_pebble.ParseFacts{}, err)
					return
				}
			}
			if !yield(facts, nil) {
				return
			}
		}
	}
}
