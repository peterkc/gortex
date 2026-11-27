package resolver

import (
	"context"
	"iter"

	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/graph/store_pebble"
)

// RunRebuildExpoModuleBridge executes framework position twelve after KMP.
func RunRebuildExpoModuleBridge(ctx context.Context, b *store_pebble.BuildStore, receipts iter.Seq2[store_pebble.ParseFacts, error],
	selection FrameworkSynthesizerSelection,
) (store_pebble.RebuildExpoModuleBridgeStats, error) {
	if !selection.allows(SynthExpoModules) {
		return store_pebble.RebuildExpoModuleBridgeStats{}, nil
	}
	required, family := expoBridgeFamilyMask()
	return b.RebuildExpoModuleBridge(ctx, receipts, required, family, func(e *graph.Edge) bool {
		return e.Kind == graph.EdgeCalls && e.From != "" && e.Meta["via"] == rnNativeVia
	}, func(e *graph.Edge, native *graph.Node) *graph.Edge {
		return &graph.Edge{
			From:            e.From,
			To:              native.ID,
			Kind:            graph.EdgeCalls,
			FilePath:        e.FilePath,
			Line:            e.Line,
			Confidence:      0.6,
			ConfidenceLabel: graph.ConfidenceLabelFor(graph.EdgeCalls, 0.6),
			Origin:          graph.OriginASTInferred,
			Meta: map[string]any{
				"via":             expoBridgeVia,
				"expo_module":     e.Meta["rn_module"],
				"expo_method":     e.Meta["rn_method"],
				"native_language": native.Language,
				MetaSynthesizedBy: SynthExpoModules,
				MetaProvenance:    ProvenanceHeuristic,
			},
		}
	})
}

func expoBridgeFamilyMask() (uint8, func(string) uint8) {
	var required uint8
	families := map[string]uint8{}
	for i, group := range frameworkSynthFamilyConjunctions[SynthExpoModules] {
		bit := uint8(1) << i
		required |= bit
		for _, family := range group {
			families[family] |= bit
		}
	}
	return required, func(language string) uint8 {
		return families[frameworkLanguageFamily(language)]
	}
}
