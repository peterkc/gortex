package indexer

import (
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/graph/store_sqlite"
)

func TestCheckoutLifecycleNormalizesAbsentStorage(t *testing.T) {
	var nilStore *store_sqlite.Store
	var nilCatalog *store_sqlite.Catalog
	for graphName, store := range map[string]graph.Store{"nil": nil, "typed_nil": nilStore, "memory": graph.New()} {
		for catalogName, catalog := range map[string]CheckoutCatalog{"nil": nil, "typed_nil": nilCatalog} {
			t.Run(graphName+"/"+catalogName, func(t *testing.T) {
				lifecycle, err := NewCheckoutLifecycle(CheckoutLifecycleConfig{MultiIndexer: &MultiIndexer{}, Graph: store, Catalog: catalog})
				require.NoError(t, err)
				t.Cleanup(lifecycle.cancelTransitions)
				require.Nil(t, lifecycle.store)
				require.Nil(t, lifecycle.catalog)
				require.Nil(t, lifecycle.rec)
			})
		}
	}
}
