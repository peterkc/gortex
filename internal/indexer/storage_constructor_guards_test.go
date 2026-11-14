package indexer

import (
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/zzet/gortex/internal/graph/store_sqlite"
	"github.com/zzet/gortex/internal/graphview"
)

func TestCheckoutLifecycleNormalizesTypedNilStoreWithCatalog(t *testing.T) {
	store, err := store_sqlite.Open(":memory:")
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, store.Close()) })
	var absent *store_sqlite.Store
	catalog := store.Catalog()
	lifecycle, err := NewCheckoutLifecycle(CheckoutLifecycleConfig{MultiIndexer: &MultiIndexer{}, Graph: absent, Catalog: catalog})
	require.NoError(t, err)
	t.Cleanup(lifecycle.cancelTransitions)
	require.True(t, lifecycle.store == nil)
	require.Equal(t, catalog, lifecycle.catalog)
}

func TestCheckoutCoordinatorRefusesAbsentStore(t *testing.T) {
	store, err := store_sqlite.Open(":memory:")
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, store.Close()) })
	var absent *store_sqlite.Store
	for name, input := range map[string]CheckoutGenerationStore{"nil": nil, "typed_nil": absent} {
		t.Run(name, func(t *testing.T) {
			coordinator, err := NewCheckoutCoordinator(CheckoutCoordinatorConfig{
				CheckoutID: "checkout", CheckoutRoot: t.TempDir(), FamilyID: "family",
				Store: input, Catalog: store.Catalog(), Builder: &SparseGenerationBuilder{Store: store, Catalog: store.Catalog()},
			})
			require.EqualError(t, err, "indexer: checkout coordinator needs a store")
			require.Nil(t, coordinator)
		})
	}
}

func TestDedicatedBaseRuntimeRefusesAbsentStore(t *testing.T) {
	store, err := store_sqlite.Open(":memory:")
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, store.Close()) })
	var absent *store_sqlite.Store
	for name, input := range map[string]GenerationBuilderStore{"nil": nil, "typed_nil": absent} {
		t.Run(name, func(t *testing.T) {
			runtime, err := NewDedicatedBaseRuntime(input, store.Catalog(), graphview.NewLeaseManager())
			require.EqualError(t, err, "invalid dedicated base runtime input: publisher runtime requires the stack's store")
			require.ErrorIs(t, err, errDedicatedBaseRuntimeInput)
			require.Nil(t, runtime)
		})
	}
}
