package indexer

import "context"

// RunDeferredPassesAllResult is the result-bearing orchestration form used by
// lifecycle callers that must decide whether a later global safety sweep is
// still required.
func (mi *MultiIndexer) RunDeferredPassesAllResult(ctx context.Context) DeferredPassesResult {
	return mi.BeginDeferredPasses(ctx, nil).FinishTailResult()
}

// The lanes-held route receives only completed cold indexers already covered
// by the caller's mutation lanes; the public route coordinates each indexer.
func (mi *MultiIndexer) beginDeferredPasses(ctx context.Context, applyGate <-chan struct{}, indexers []*Indexer, lanesHeld bool) *DeferredPassesRun {
	return mi.beginDeferredPassesOptions(ctx, applyGate, indexers, lanesHeld, deferredEnrichOptions{})
}

func (mi *MultiIndexer) runDeferredEnrichPool(indexers []*Indexer) {
	mi.runDeferredEnrichPoolWork(indexers, func(idx *Indexer) { idx.runDeferredEnrich() })
}

// BeginDeferredPasses selects the repos with deferred work, prepares the
// mutation-receipt window, materialises go.mod dependencies, and launches the
// enrichment pool on its own goroutine. The caller must call FinishTailResult (which
// joins the pool) exactly once. applyGate, when non-nil, parks every
// provider's graph-apply phase until the caller closes it — the caller MUST
// first call BeginApplyMutationReceipt after its resolve phase, then close the
// gate, or FinishTailResult deadlocks.
//
// Without an apply gate the receipt opens here. With overlap, delaying it until
// the apply boundary excludes resolver writes while still observing every
// semantic apply and contract commit, preserving an exact file frontier.
func (mi *MultiIndexer) BeginDeferredPasses(ctx context.Context, applyGate <-chan struct{}) *DeferredPassesRun {
	return mi.beginRegisteredDeferredPasses(ctx, applyGate, deferredEnrichOptions{})
}
