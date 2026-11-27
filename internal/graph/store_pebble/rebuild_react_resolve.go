package store_pebble

import (
	"context"
	"errors"
	"iter"

	"github.com/zzet/gortex/internal/graph"
)

var ErrRebuildReactResolve = errors.New("pebble rebuild React hooks and context failed")

var rebuildReactResolvePrefix = []byte{'g', 'q', 'H'}

// FreezeReactResolveInputs captures Calls and References. RendersChild stays live.
func (b *BuildStore) FreezeReactResolveInputs(ctx context.Context, match func(graph.FrameworkCensusEdge) bool) error {
	return b.freezeConventionInputs(ctx, rebuildReactResolveColumns(b), ErrRebuildReactResolve, match)
}

func rebuildReactResolveColumns(b *BuildStore) rebuildCallMoves {
	return rebuildCallMoves{b: b, prefix: rebuildReactResolvePrefix, rowBytes: 16 << 20, label: "react hooks and context", inputRow: "edge row"}
}

// RebuildReactResolve runs after Express and before FastAPI.
func (b *BuildStore) RebuildReactResolve(ctx context.Context, receipts iter.Seq2[ParseFacts, error], rules RebuildConventionRules) (uint64, error) {
	cols := rebuildReactResolveColumns(b)
	kinds := [...]graph.EdgeKind{graph.EdgeCalls, graph.EdgeReferences, graph.EdgeRendersChild}
	return b.rebuildConvention(ctx, receipts, cols, rules, ErrRebuildReactResolve, cols.label+" fallback inputs", kinds[:], graph.EdgeRendersChild)
}
