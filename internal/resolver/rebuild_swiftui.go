package resolver

import (
	"context"
	"iter"

	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/graph/store_pebble"
)

// RunRebuildSwiftUI applies the live SwiftUI convention turn.
func RunRebuildSwiftUI(ctx context.Context, build *store_pebble.BuildStore, receipts iter.Seq2[store_pebble.ParseFacts, error],
	selection FrameworkSynthesizerSelection,
) (uint64, error) {
	if !selection.allows(SynthSwiftUIResolve) {
		return 0, nil
	}
	return build.RebuildSwiftUI(ctx, receipts, store_pebble.RebuildConventionRules{
		Shape:       rebuildSwiftUIShape,
		Source:      swiftUIRefSource,
		Eligible:    isConventionResolvable,
		Rank:        rebuildConventionRank,
		Rebind:      swiftUIRefRebind,
		AllowWinner: rebuildSwiftUIWinner,
	})
}

func rebuildSwiftUIShape(target, _ string) ([]string, []string) {
	name, dirs, _, ok := swiftUIRefShape(&graph.Edge{To: target})
	if !ok {
		return nil, nil
	}
	return []string{name}, dirs
}

func rebuildSwiftUIWinner(name, file string) bool {
	_, modelOnly, _ := swiftUIDirsFor(name)
	return !modelOnly || swiftUIModelPath(file)
}
