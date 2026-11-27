package resolver

import (
	"context"
	"iter"

	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/graph/store_pebble"
)

// RunRebuildExpoModuleBridge executes framework position twelve after KMP.
func RunRebuildExpoModuleBridge(ctx context.Context, b *store_pebble.BuildStore, receipts iter.Seq2[store_pebble.ParseFacts, error],
	selection FrameworkSynthesizerSelection, spillAt int,
) (store_pebble.RebuildExpoModuleBridgeStats, error) {
	if !selection.allows(SynthExpoModules) {
		return store_pebble.RebuildExpoModuleBridgeStats{}, nil
	}
	return b.RebuildExpoModuleBridge(ctx, receipts, spillAt, expoBridgeFamily, func(e *graph.Edge) bool {
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

func expoBridgeFamily(language string) uint8 {
	switch frameworkLanguageFamily(language) {
	case "web":
		return 1
	case "apple", "jvm":
		return 2
	default:
		return 0
	}
}
