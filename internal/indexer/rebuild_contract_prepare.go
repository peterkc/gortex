package indexer

import (
	"context"
	"errors"
	"fmt"
	"iter"
	"maps"
	"slices"

	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/graph/store_pebble"
)

// prepareRebuildContracts never offers a stage. The caller must discard the
// private build on a sealing or comparison failure. complete comes from late
// logical emission, selected comes from the independent D83 election, and
// batches carries the final unique rows produced by that election.
func prepareRebuildContracts(ctx context.Context, b *store_pebble.BuildStore, dir string, base store_pebble.RelationsBase,
	early rebuildEarlyContractIDs, complete, selected iter.Seq2[string, error],
	batches iter.Seq2[store_pebble.RebuildContractBatch, error], coverage store_pebble.ContractCoverage,
	extractorOf func(string) string,
) (store_pebble.StageTables, error) {
	if err := compareRebuildContractIDs(ctx, orderedEarlyContractIDs(early), complete); err != nil {
		return store_pebble.StageTables{}, fmt.Errorf("compare complete private contract IDs: %w", err)
	}
	if selected == nil || batches == nil {
		return store_pebble.StageTables{}, fmt.Errorf("%w: missing D83 selection", errRebuildContractIDs)
	}
	checked := func(yield func(store_pebble.RebuildContractBatch, error) bool) {
		for batch, err := range batches {
			if err == nil {
				for _, e := range batch.Edges {
					if e != nil && e.Kind == graph.EdgeCalls {
						if err = b.ValidateRebuildCallDelta(ctx, e, base.PlatformCatalogRevision(), extractorOf); err != nil {
							break
						}
					}
				}
			}
			if !yield(batch, err) || err != nil {
				return
			}
		}
	}
	tables, err := b.PrepareContracts(ctx, dir, base, checked, coverage)
	if err != nil {
		return store_pebble.StageTables{}, err
	}
	if err := compareRebuildContractIDs(ctx, selected, preparedRebuildContractIDs(ctx, b, tables)); err != nil {
		return store_pebble.StageTables{}, fmt.Errorf("compare sealed contract IDs against D83: %w", err)
	}
	return tables, nil
}

func orderedEarlyContractIDs(ids rebuildEarlyContractIDs) iter.Seq2[string, error] {
	if ids == nil {
		return nil
	}
	return func(yield func(string, error) bool) {
		for _, id := range slices.Sorted(maps.Keys(ids)) {
			if !yield(id, nil) {
				return
			}
		}
	}
}

var errStopPreparedContractIDs = errors.New("stop reading prepared contract IDs")

func preparedRebuildContractIDs(ctx context.Context, b *store_pebble.BuildStore, tables store_pebble.StageTables) iter.Seq2[string, error] {
	return func(yield func(string, error) bool) {
		err := b.VisitPreparedContractIDs(ctx, tables, func(id string) error {
			if !yield(id, nil) {
				return errStopPreparedContractIDs
			}
			return nil
		})
		if err != nil && !errors.Is(err, errStopPreparedContractIDs) {
			yield("", err)
		}
	}
}
