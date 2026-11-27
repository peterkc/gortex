package store_pebble

import (
	"context"
	"errors"
	"iter"
)

var ErrRebuildSwiftUI = errors.New("pebble rebuild SwiftUI fallback failed")

var rebuildSwiftUIPrefix = []byte{'g', 'q', 'W'}

// RebuildSwiftUI reads the four live edge kinds in native order. The
// convention reducer proves the winner and applies frozen receipt windows.
func (b *BuildStore) RebuildSwiftUI(ctx context.Context, receipts iter.Seq2[ParseFacts, error], rules RebuildConventionRules) (uint64, error) {
	cols := rebuildCallMoves{b: b, prefix: rebuildSwiftUIPrefix, rowBytes: 16 << 20, label: "swiftui", inputRow: "edge row"}
	return b.rebuildLiveAppleConvention(ctx, receipts, cols, rules, ErrRebuildSwiftUI)
}
