package indexer

import "context"

// The lanes-held route receives only completed cold indexers already covered
// by the caller's mutation lanes; the public route coordinates each indexer.
func (mi *MultiIndexer) beginDeferredPasses(ctx context.Context, applyGate <-chan struct{}, indexers []*Indexer, lanesHeld bool) *DeferredPassesRun {
	return mi.beginDeferredPassesOptions(ctx, applyGate, indexers, lanesHeld, deferredEnrichOptions{})
}

func (mi *MultiIndexer) runDeferredEnrichPool(indexers []*Indexer) {
	mi.runDeferredEnrichPoolWork(indexers, func(idx *Indexer) { idx.runDeferredEnrich() })
}
