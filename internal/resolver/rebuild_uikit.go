package resolver

import (
	"context"
	"iter"

	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/graph/store_pebble"
)

// FreezeRebuildUIKitInputs records the native shared candidate identities.
func FreezeRebuildUIKitInputs(ctx context.Context, b *store_pebble.BuildStore, selection FrameworkSynthesizerSelection) error {
	if !selection.allows(SynthUIKitResolve) {
		return nil
	}
	return b.FreezeUIKitInputs(ctx, uikitCandidateEdge)
}

// RunRebuildUIKit runs the UIKit convention turn after SwiftUI.
func RunRebuildUIKit(ctx context.Context, b *store_pebble.BuildStore, receipts iter.Seq2[store_pebble.ParseFacts, error],
	selection FrameworkSynthesizerSelection,
) (uint64, error) {
	if !selection.allows(SynthUIKitResolve) {
		return 0, nil
	}
	return b.RebuildUIKit(ctx, receipts, store_pebble.RebuildConventionRules{
		Shape:    rebuildUIKitShape,
		Source:   isAppleSourceFile,
		Eligible: isConventionResolvable,
		Rank:     rebuildConventionRank,
		Rebind:   uikitRefRebind,
	})
}

func rebuildUIKitShape(target, _ string) ([]string, []string) {
	if !isUIKitCandidateTarget(target) {
		return nil, nil
	}
	name := graph.UnresolvedName(target)
	dirs, _ := uikitDirsFor(name)
	return []string{name}, dirs
}
