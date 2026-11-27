package resolver

import (
	"context"
	"iter"

	"github.com/zzet/gortex/internal/graph/store_pebble"
)

// RunRebuildCSharpIfaceDispatch runs after MediatR and before Sidekiq.
func RunRebuildCSharpIfaceDispatch(ctx context.Context, build *store_pebble.BuildStore, receipts iter.Seq2[store_pebble.ParseFacts, error],
	selection FrameworkSynthesizerSelection,
) (uint64, error) {
	if !selection.allows(SynthCSharpIfaceDispatch) {
		return 0, nil
	}
	return build.RebuildCSharpIfaceDispatch(ctx, receipts, store_pebble.RebuildCSharpIfaceRules{
		Declared:  csharpIfaceDeclaredArc,
		Candidate: csharpIfaceHierarchyCandidate,
		Anchor:    csharpIfaceDispatchAnchor,
		Source:    csharpIfaceDispatchSource,
		Emit:      csharpIfaceDispatchEdge,
		Cap:       csharpIfaceDispatchCap,
	})
}
