package resolver

import (
	"context"
	"iter"

	"github.com/zzet/gortex/internal/graph/store_pebble"
)

// FreezeRebuildMacroExpansionInputs captures the native pre-loop census.
func FreezeRebuildMacroExpansionInputs(ctx context.Context, b *store_pebble.BuildStore, selection FrameworkSynthesizerSelection) error {
	if !selection.allows(SynthMacroExpansion) {
		return nil
	}
	return b.FreezeMacroExpansionInputs(ctx, functionLikeMacro)
}

// RunRebuildMacroExpansion executes the private turn after Function pointer.
func RunRebuildMacroExpansion(ctx context.Context, b *store_pebble.BuildStore, receipts iter.Seq2[store_pebble.ParseFacts, error],
	selection FrameworkSynthesizerSelection,
) (store_pebble.RebuildMacroExpansionStats, error) {
	if !selection.allows(SynthMacroExpansion) {
		return store_pebble.RebuildMacroExpansionStats{}, nil
	}
	return b.RebuildMacroExpansion(ctx, receipts, store_pebble.RebuildMacroExpansionRules{
		Macro:  functionLikeMacro,
		Body:   macroBodyCallee,
		MayAdd: macroExpansionMayAdd,
		Add:    macroExpansionEdge,
	})
}
