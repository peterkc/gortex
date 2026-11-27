package resolver

import (
	"context"
	"iter"

	"github.com/zzet/gortex/internal/graph/store_pebble"
)

// RunRebuildCSharpDIIfaces runs on the settled hierarchy before interface dispatch.
func RunRebuildCSharpDIIfaces(ctx context.Context, build *store_pebble.BuildStore, receipts iter.Seq2[store_pebble.ParseFacts, error],
	selection FrameworkSynthesizerSelection,
) (uint64, error) {
	if !selection.allows(SynthCSharpDIIfaces) {
		return 0, nil
	}
	return build.RebuildCSharpDIIfaces(ctx, receipts, store_pebble.RebuildCSharpDIRules{
		Name:     providesTargetName,
		Key:      csharpDIBindingKey,
		Impl:     csharpDIImplementation,
		Iface:    csharpDIInterface,
		Declared: csharpDIDeclaredImplements,
		Emit:     csharpDIProvideEdge,
	})
}
