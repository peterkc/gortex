package indexer

import (
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/graph/store_sqlite"
)

func testCatalog(value any) CheckoutCatalog {
	provider, ok := value.(interface{ Catalog() *store_sqlite.Catalog })
	if !ok {
		return nil
	}
	catalog := provider.Catalog()
	if catalog == nil {
		return nil
	}
	return catalog
}

func openTestEvidenceStore(path string) (graph.Store, error) {
	store, err := store_sqlite.Open(path)
	if store == nil {
		return nil, err
	}
	return store, err
}

func payloadHandles(handles []*store_sqlite.Store) []generationPayloadStore {
	result := make([]generationPayloadStore, len(handles))
	for i, handle := range handles {
		result[i] = handle
	}
	return result
}
