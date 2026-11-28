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
	return lateRouterJoinError(missing, err)
}

func lateRouterJoinError(missing string, err error) error {
	if missing == "" {
		return err
	}
	// The proof check set missing and returned an error. A second cause
	// in either scratch cleanup means this is not just a missing proof.
	for cause := err; cause != nil; {
		switch e := cause.(type) {
		case interface{ Unwrap() []error }:
			children := e.Unwrap()
			if len(children) != 1 {
				return err
			}
			cause = children[0]
		case interface{ Unwrap() error }:
			cause = e.Unwrap()
		default:
			return fmt.Errorf("%w: %w", errRebuildRouterFactsMissing, err)
		}
	}
	return err
}
