package indexer

import (
	"context"
	"fmt"
	"slices"
	"strings"

	"github.com/zzet/gortex/internal/contracts"
	"github.com/zzet/gortex/internal/graph/store_pebble"
)

func (r rebuildContractReceiptReader) joinLateRouterPrefixes(ctx context.Context, records *store_pebble.RebuildContractRecords,
	overlap func(string) error,
) error {
	missing, err := records.JoinCheckedRouterPrefixes(r.idx.repoPrefix, r.idx.workspaceID, func(file string) (*contracts.RouterFacts, error) {
		origin := r.census.originByGraphPath[file]
		if r.census.byPath[origin] == "" {
			// A size-skipped or other unadmitted path has no checked answer.
			return nil, nil
		}
		i, ok := slices.BinarySearchFunc(r.job.inputs, origin, func(in rebuildInput, key string) int {
			return strings.Compare(in.Path, key)
		})
		if !ok {
			return nil, fmt.Errorf("%w: missing admitted router input for %q", errParseEntryCorrupt, file)
		}
		_, facts, err := r.load(ctx, r.job.inputs[i])
		if err != nil {
			return nil, err
		}
		return facts.Router, nil
	}, overlap)
	if missing != "" {
		return fmt.Errorf("%w: %w", errRebuildRouterFactsMissing, err)
	}
	return err
}
