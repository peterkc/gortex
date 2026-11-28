package resolver

import (
	"context"
	"iter"

	"github.com/zzet/gortex/internal/graph/store_pebble"
)

// FreezeRebuildFactoryChainInputs captures the native census candidates before any stage writes.
func FreezeRebuildFactoryChainInputs(ctx context.Context, b *store_pebble.BuildStore, selection FrameworkSynthesizerSelection) error {
	if !selection.allows(SynthFactoryChain) {
		return nil
	}
	return b.FreezeFactoryChainInputs(ctx, factoryChainCandidateEdge)
}

// RunRebuildFactoryChain settles cross-file Calls and References after the
// earlier framework turns, using settled Implements and Extends.
func RunRebuildFactoryChain(ctx context.Context, b *store_pebble.BuildStore, receipts iter.Seq2[store_pebble.ParseFacts, error],
	selection FrameworkSynthesizerSelection,
) (uint64, error) {
	if !selection.allows(SynthFactoryChain) {
		return 0, nil
	}
	return b.RebuildFactoryChain(ctx, receipts, store_pebble.RebuildFactoryChainRules{
		Call: factoryChainCall,
		Target: func(lookup store_pebble.RebuildFactoryChainLookup, from, expr, member string) (string, bool) {
			return factoryChainTarget(factoryChainLookup{
				nodes: lookup.Nodes, incoming: lookup.Incoming, node: lookup.Node, seedNodes: lookup.SeedNodes, matchedArc: lookup.MatchedArc,
			}, from, expr, member)
		},
		Rebind: rebindFactoryChain,
	})
}
