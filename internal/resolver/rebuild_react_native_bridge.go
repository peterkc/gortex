package resolver

import (
	"context"
	"iter"
	"slices"

	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/graph/store_pebble"
)

// RunRebuildReactNativeBridge executes framework position five after the
// private Swift/ObjC bridge and before native-implementation pairing.
// It consumes live rn.native markers, including extractor targets retained
// after main's checked full-name miss.
func RunRebuildReactNativeBridge(ctx context.Context, b *store_pebble.BuildStore, receipts iter.Seq2[store_pebble.ParseFacts, error],
	selection FrameworkSynthesizerSelection, spillAt int,
) (store_pebble.RebuildReactNativeBridgeStats, error) {
	if !selection.allows(SynthReactNative) {
		return store_pebble.RebuildReactNativeBridgeStats{}, nil
	}
	return b.RebuildReactNativeBridge(ctx, receipts, spillAt, func(language string) bool {
		return slices.Contains(frameworkSynthLanguageFamilies[SynthReactNative], frameworkLanguageFamily(language))
	}, func(e *graph.Edge) bool {
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
				"via":             rnBridgeVia,
				"rn_module":       e.Meta["rn_module"],
				"rn_method":       e.Meta["rn_method"],
				"native_language": native.Language,
				MetaSynthesizedBy: SynthReactNative,
				MetaProvenance:    ProvenanceHeuristic,
			},
		}
	})
}
