package indexer

import (
	"github.com/zzet/gortex/internal/graph"
)

// Select actual source handles, including mutable zero only when read. The
// immutable cache key's stack omits every positive ancestor when zero is
// composed, so that key cannot identify this input witness.
func editDeltaContractInputGenerations(base graph.Reader, store interface{ SymbolSearchCoreKey() any }) ([]int64, bool) {
	if store == nil || store.SymbolSearchCoreKey() == nil {
		return nil, false
	}
	var handles []generationPayloadStore
	switch base := base.(type) {
	case generationPayloadStore:
		if !base.SharesSymbolSearchCore(base) {
			return nil, false
		}
		handles = append(handles, base)
	case commitLayerBase:
		if base.cloneCorpusBase != nil && !base.cloneCorpusBase.IsNilGenerationHandle() {
			handles = append(handles, base.cloneCorpusBase)
		}
		for _, source := range base.cloneSources {
			if (source.Handle == nil || source.Handle.IsNilGenerationHandle()) || source.Generation != source.Handle.ViewGeneration() {
				return nil, false
			}
			handles = append(handles, source.Handle.(generationPayloadStore))
		}
		if len(handles) == 0 {
			// An older single-handle base is known only when its reader is
			// exactly that handle; an opaque composed reader is unsupported.
			if reader, ok := base.Reader.(generationPayloadStore); ok && reader.SharesSymbolSearchCore(base.Reader) && reader == base.corpus {
				handles = append(handles, reader)
			} else {
				return nil, false
			}
		}
	default:
		return nil, false
	}
	ids := make([]int64, 0, len(handles))
	seen := make(map[int64]bool)
	for _, handle := range handles {
		if (handle == nil || handle.IsNilGenerationHandle()) || handle.SymbolSearchCoreKey() != store.SymbolSearchCoreKey() {
			return nil, false
		}
		generation := handle.ViewGeneration()
		if !seen[generation] {
			ids = append(ids, generation)
			seen[generation] = true
		}
	}
	return ids, len(ids) != 0
}
