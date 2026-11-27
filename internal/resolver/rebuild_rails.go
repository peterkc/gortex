package resolver

import (
	"context"
	"iter"

	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/graph/store_pebble"
)

// FreezeRebuildRailsInputs captures the native pre-loop recv_const identities.
func FreezeRebuildRailsInputs(ctx context.Context, b *store_pebble.BuildStore, selection FrameworkSynthesizerSelection) error {
	if !selection.allows(SynthRailsResolve) {
		return nil
	}
	return b.FreezeRailsInputs(ctx, func(e *graph.Edge) bool {
		recv, _ := e.Meta["recv_const"].(string)
		return railsCandidateEdge(graph.FrameworkCensusEdge{EdgeIdentity: graph.EdgeIdentityFor(e), RecvConst: recv})
	})
}

// RunRebuildRails runs the native Rails turn after FastAPI.
func RunRebuildRails(ctx context.Context, b *store_pebble.BuildStore, receipts iter.Seq2[store_pebble.ParseFacts, error],
	selection FrameworkSynthesizerSelection,
) (store_pebble.RebuildRailsStats, error) {
	if !selection.allows(SynthRailsResolve) {
		return store_pebble.RebuildRailsStats{}, nil
	}
	return b.RebuildRails(ctx, receipts, store_pebble.RebuildRailsRules{
		Call:      railsCallKey,
		Source:    railsRefSource,
		Owner:     railsMemberOwner,
		Candidate: isConventionResolvable,
		Preferred: func(file, receiver string) bool {
			_, dirs := railsRefDirs(receiver)
			return dirMatchesAny(file, dirs)
		},
		Directory: dirOf,
		Model: func(receiver string) bool {
			model, _ := railsRefDirs(receiver)
			return model
		},
		Rebind: railsRefRebind,
	})
}
