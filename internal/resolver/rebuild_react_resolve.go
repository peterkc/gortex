package resolver

import (
	"context"
	"iter"

	"github.com/zzet/gortex/internal/graph/store_pebble"
)

// FreezeRebuildReactResolveInputs captures the native Calls and References census.
func FreezeRebuildReactResolveInputs(ctx context.Context, build *store_pebble.BuildStore, selection FrameworkSynthesizerSelection) error {
	if !selection.allows(SynthReactResolve) {
		return nil
	}
	return build.FreezeReactResolveInputs(ctx, reactCandidateEdge)
}

// RunRebuildReactResolve reads residual hooks and contexts after Express.
func RunRebuildReactResolve(ctx context.Context, build *store_pebble.BuildStore, receipts iter.Seq2[store_pebble.ParseFacts, error],
	selection FrameworkSynthesizerSelection,
) (uint64, error) {
	if !selection.allows(SynthReactResolve) {
		return 0, nil
	}
	return build.RebuildReactResolve(ctx, receipts, store_pebble.RebuildConventionRules{
		Shape:    rebuildReactResolveShape,
		Source:   isReactSourceFile,
		Eligible: isConventionResolvable,
		Rank:     rebuildConventionRank,
		Rebind:   reactResolveRebind,
	})
}

func rebuildReactResolveShape(target, via string) ([]string, []string) {
	head, suffix, dirs, ok := reactResolveEdgeShape(target, via)
	if !ok || head == "" {
		return nil, nil
	}
	return dirConventionNames(head, suffix), dirs
}
