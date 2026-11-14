package indexer

import (
	"github.com/stretchr/testify/require"
	"github.com/zzet/gortex/internal/graph/store_sqlite"
	"testing"
)

func TestCheckoutLifecycleRefusesPhysicalStoreWithoutPairedCatalog(t *testing.T) {
	store := builderOpenStoreAt(t, t.TempDir()+"/catalog.sqlite")
	t.Cleanup(func() { _ = store.Close() })
	var absent *store_sqlite.Catalog
	for name, catalog := range map[string]CheckoutCatalog{"nil": nil, "native_typed_nil": absent} {
		t.Run(name, func(t *testing.T) {
			lifecycle, err := NewCheckoutLifecycle(CheckoutLifecycleConfig{MultiIndexer: &MultiIndexer{}, Graph: store, Catalog: catalog})
			require.EqualError(t, err, "indexer: checkout lifecycle needs the store's paired catalog")
			require.Nil(t, lifecycle)
		})
	}
}
