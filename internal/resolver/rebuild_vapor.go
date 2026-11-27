package resolver

import (
	"context"
	"iter"

	"github.com/zzet/gortex/internal/graph/store_pebble"
)

// RunRebuildVapor applies the live Vapor convention turn after SwiftUI and UIKit.
func RunRebuildVapor(ctx context.Context, build *store_pebble.BuildStore, receipts iter.Seq2[store_pebble.ParseFacts, error],
	selection FrameworkSynthesizerSelection,
) (uint64, error) {
	if !selection.allows(SynthVaporResolve) {
		return 0, nil
	}
	return build.RebuildVapor(ctx, receipts, store_pebble.RebuildConventionRules{
		Shape:       rebuildVaporShape,
		Source:      vaporRefSource,
		Eligible:    isConventionResolvable,
		Rank:        rebuildConventionRank,
		Rebind:      vaporRefRebind,
		AllowWinner: rebuildVaporWinner,
	})
}

func rebuildVaporShape(target, _ string) ([]string, []string) {
	name, dirs, _, ok := vaporRefShape(target)
	if !ok {
		return nil, nil
	}
	return []string{name}, dirs
}

func rebuildVaporWinner(name, file string) bool {
	_, modelOnly, _ := vaporDirsFor(name)
	return !modelOnly || vaporModelPath(file)
}
