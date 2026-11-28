package resolver

import (
	"context"
	"iter"

	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/graph/store_pebble"
)

// RunRebuildFnValueCallbacks executes the live callback turn after factory chains.
func RunRebuildFnValueCallbacks(ctx context.Context, b *store_pebble.BuildStore, receipts iter.Seq2[store_pebble.ParseFacts, error],
	selection FrameworkSynthesizerSelection,
) (uint64, error) {
	if !selection.allows(SynthFnValue) {
		return 0, nil
	}
	return b.RebuildFnValueCallbacks(ctx, receipts, store_pebble.RebuildFnValueRules{
		Site:         rebuildFnValueSite,
		Prototype:    isPrototypeDecl,
		LocalLinkage: isFileLocalLinkage,
		Registration: fnValueRegistration,
		Choose:       chooseFnValueTargets,
	})
}

func rebuildFnValueSite(e *graph.Edge) store_pebble.RebuildFnValueSite {
	if e == nil {
		return store_pebble.RebuildFnValueSite{}
	}
	via, _ := e.Meta["via"].(string)
	name, _ := e.Meta[metaFnValueName].(string)
	if via != fnValueCandidateVia || name == "" || isFnValueNonTarget(name) {
		return store_pebble.RebuildFnValueSite{}
	}
	recv, _ := e.Meta["fn_ref_recv_hint"].(string)
	ungated, _ := e.Meta["fn_value_ungated"].(bool)
	skip, _ := e.Meta["skip_gate"].(bool)
	return store_pebble.RebuildFnValueSite{Name: name, Receiver: recv, Ungated: ungated, SkipGate: skip}
}
