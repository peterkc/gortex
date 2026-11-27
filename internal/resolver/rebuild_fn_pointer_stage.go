package resolver

import (
	"context"
	"iter"

	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/graph/store_pebble"
)

// FreezeRebuildFnPointerInputs captures native candidate identities before the registry loop.
func FreezeRebuildFnPointerInputs(ctx context.Context, b *store_pebble.BuildStore, selection FrameworkSynthesizerSelection) error {
	if !selection.allows(SynthFnPointerDispatch) {
		return nil
	}
	return b.FreezeFnPointerInputs(ctx, func(e *graph.Edge) bool {
		via, _ := e.Meta["via"].(string)
		c := graph.FrameworkCensusEdge{Via: via}
		return e.Kind == graph.EdgeReferences && fnPtrRegCandidateEdge(c) ||
			e.Kind == graph.EdgeCalls && fnPtrDispatchCandidateEdge(c)
	})
}

// RunRebuildFnPointerStage executes the private registry turn after Laravel events.
func RunRebuildFnPointerStage(ctx context.Context, b *store_pebble.BuildStore, receipts iter.Seq2[store_pebble.ParseFacts, error],
	selection FrameworkSynthesizerSelection,
) (store_pebble.RebuildFnPointerStageStats, error) {
	if !selection.allows(SynthFnPointerDispatch) {
		return store_pebble.RebuildFnPointerStageStats{}, nil
	}
	return b.RebuildFnPointerStage(ctx, receipts, store_pebble.RebuildFnPointerStageRules{
		Registration: fnPtrRegistration,
		Dispatch:     func(e *graph.Edge) (string, string, bool) { return fnPtrSlot(e, fnPtrDispatchVia) },
		Callable:     fnPtrCallable,
		Getter:       isStoreFactoryGetter,
		BindRef:      rebindFnPtrRegistration,
		BindCall:     rebindFnPtrDispatch,
		Fanout: func(e *graph.Edge, id, st, field string) *graph.Edge {
			return fnPtrFanoutEdge(e, &graph.Node{ID: id}, st, field)
		},
		Sweeps: fnPointerSweepCap,
		Cap:    fnPointerFanoutCap,
	})
}
