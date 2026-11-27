package store_pebble

import (
	"context"
	"errors"
	"iter"

	"github.com/zzet/gortex/internal/graph"
)

var ErrRebuildFastAPI = errors.New("pebble rebuild FastAPI fallback failed")

var rebuildFastAPIPrefix = []byte{'g', 'q', 'A'}

// FreezeFastAPIInputs retains exact pre-loop identities, not edge payloads.
func (b *BuildStore) FreezeFastAPIInputs(ctx context.Context, match func(graph.FrameworkCensusEdge) bool) error {
	return b.freezeConventionInputs(ctx, rebuildFastAPIColumns(b), ErrRebuildFastAPI, match)
}

func rebuildFastAPIColumns(b *BuildStore) rebuildCallMoves {
	return rebuildCallMoves{b: b, prefix: rebuildFastAPIPrefix, rowBytes: 16 << 20, label: "FastAPI", inputRow: "edge row"}
}

// RebuildFastAPI resolves residual dependency and router rows. Calls and
// References share native move windows, with each kind in receipt order.
func (b *BuildStore) RebuildFastAPI(ctx context.Context, receipts iter.Seq2[ParseFacts, error], rules RebuildConventionRules) (uint64, error) {
	return b.rebuildConvention(ctx, receipts, rebuildFastAPIColumns(b), rules, ErrRebuildFastAPI, false)
}
