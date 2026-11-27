package store_pebble

import (
	"context"
	"errors"
	"iter"
)

var ErrRebuildVapor = errors.New("pebble rebuild Vapor fallback failed")

var rebuildVaporPrefix = []byte{'g', 'q', 'Z'}

// RebuildVapor reads the four live edge kinds in native order. The
// convention reducer proves the winner and applies frozen receipt windows.
func (b *BuildStore) RebuildVapor(ctx context.Context, receipts iter.Seq2[ParseFacts, error], rules RebuildConventionRules) (uint64, error) {
	cols := rebuildCallMoves{b: b, prefix: rebuildVaporPrefix, rowBytes: 16 << 20, label: "vapor", inputRow: "edge row"}
	return b.rebuildLiveAppleConvention(ctx, receipts, cols, rules, ErrRebuildVapor)
}
