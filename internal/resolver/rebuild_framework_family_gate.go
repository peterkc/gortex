package resolver

import (
	"context"
	"fmt"

	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/graph/store_pebble"
)

// FreezeRebuildFrameworkFamilyGate captures the native cold census before
// the registry runs. The gate itself is independent of selection.
func FreezeRebuildFrameworkFamilyGate(ctx context.Context, view store_pebble.RebuildView, selection FrameworkSynthesizerSelection) (bool, error) {
	if selection.none() {
		return false, nil
	}
	var first string
	var distinct bool
	err := view.VisitFrameworkGateNodes(ctx, func(n *graph.Node) error {
		if family := languageFamily(n.Language); family != "" {
			if first == "" {
				first = family
			} else {
				distinct = distinct || family != first
			}
		}
		return nil
	})
	if err != nil {
		return false, fmt.Errorf("%w: %w", store_pebble.ErrRebuildFrameworkFamilyGate, err)
	}
	return distinct, nil
}

// RunRebuildFrameworkFamilyGate removes accidental cross-family References
// and Imports after all registry passes, before the claiming resolvers.
func RunRebuildFrameworkFamilyGate(ctx context.Context, b *store_pebble.BuildStore, needed bool) (uint64, error) {
	if !needed {
		return 0, nil
	}
	return b.RebuildFrameworkFamilyGate(ctx, func(e *graph.Edge) bool {
		synth, _ := e.Meta[MetaSynthesizedBy].(string)
		return synth != "" && !frameworkBridgeSynths[synth]
	}, func(from, to string) bool {
		return gateFrameworkResult("", from, to)
	})
}
