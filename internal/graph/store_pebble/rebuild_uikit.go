package store_pebble

import (
	"context"
	"errors"
	"iter"

	"github.com/zzet/gortex/internal/graph"
)

var ErrRebuildUIKit = errors.New("pebble rebuild UIKit fallback failed")

var rebuildUIKitPrefix = []byte{'g', 'q', 'F'}

func rebuildUIKitColumns(b *BuildStore) rebuildCallMoves {
	return rebuildCallMoves{b: b, prefix: rebuildUIKitPrefix, rowBytes: 16 << 20, label: "uikit", inputRow: "edge row"}
}

// FreezeUIKitInputs captures only the native shared Calls and References.
func (b *BuildStore) FreezeUIKitInputs(ctx context.Context, match func(graph.FrameworkCensusEdge) bool) error {
	return b.freezeConventionInputs(ctx, rebuildUIKitColumns(b), ErrRebuildUIKit, match)
}

// RebuildUIKit refetches the frozen sites after SwiftUI and scans the live
// Instantiates and TypedAs sites. Moves follow native kind and receipt order.
func (b *BuildStore) RebuildUIKit(ctx context.Context, receipts iter.Seq2[ParseFacts, error], rules RebuildConventionRules) (uint64, error) {
	kinds := [...]graph.EdgeKind{graph.EdgeInstantiates, graph.EdgeReferences, graph.EdgeTypedAs, graph.EdgeCalls}
	return b.rebuildConvention(ctx, receipts, rebuildUIKitColumns(b), rules, ErrRebuildUIKit, "uikit inputs", kinds[:],
		graph.EdgeInstantiates, graph.EdgeTypedAs)
}
