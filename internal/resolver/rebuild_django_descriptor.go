package resolver

import (
	"context"
	"iter"

	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/graph/store_pebble"
)

// RunRebuildDjangoDescriptor runs the native claiming resolver after the
// family correction. Selection is the requested set, not the registry census.
func RunRebuildDjangoDescriptor(ctx context.Context, b *store_pebble.BuildStore, receipts iter.Seq2[store_pebble.ParseFacts, error], selection FrameworkSynthesizerSelection) (uint64, error) {
	if !selection.allows(SynthDjangoDescriptor) {
		return 0, nil
	}
	return b.RebuildDjangoDescriptor(ctx, receipts, store_pebble.RebuildDjangoRules{
		ClaimsTo:     func(to string) bool { return djangoDescriptorVocab[djangoRefName(to)] },
		Rebind:       func(e *graph.Edge, target string) { rebindDjangoDescriptor(e, target) },
		DefaultClass: djangoDefaultIterableClass,
	})
}
