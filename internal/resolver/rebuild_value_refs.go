package resolver

import (
	"context"
	"iter"

	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/graph/store_pebble"
)

// RunRebuildValueRefs executes the cold value-ref turn after function callbacks.
func RunRebuildValueRefs(ctx context.Context, build *store_pebble.BuildStore, receipts iter.Seq2[store_pebble.ParseFacts, error],
	selection FrameworkSynthesizerSelection,
) (uint64, error) {
	if !selection.allows(SynthValueRefName) {
		return 0, nil
	}
	return build.RebuildValueRefs(ctx, receipts, store_pebble.RebuildValueRefsRules{
		Name: func(e *graph.Edge) string {
			if via, _ := e.Meta["via"].(string); via != valueRefCandidateVia {
				return ""
			}
			name, _ := e.Meta["name"].(string)
			return name
		},
		Distinctive: isDistinctiveValueName,
		Elect:       electValueRefDeclaration,
		Shadows:     valueRefShadowsReader,
		Generated:   isGeneratedReader,
		Rebind:      rebindValueRef,
	})
}
