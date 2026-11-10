package store_sqlite

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/zzet/gortex/internal/graph"
)

func TestPayloadResultAdaptersKeepNativeObjects(t *testing.T) {
	t.Run("flight", func(t *testing.T) {
		store := openCatalogStore(t)
		id := reservedGeneration(t, store, "adapter-flight")
		result, leader, ready, err := store.JoinPayloadBuildFlightHandle(t.Context(), id, false)
		require.NoError(t, err)
		require.True(t, leader)
		require.False(t, ready)
		native, ok := result.(*PayloadBuildFlight)
		require.True(t, ok, "the interface must contain the native pointer, not a wrapper")
		require.Same(t, native, result)
		require.Same(t, store.storeCore, native.core)
		require.Equal(t, id, native.generationID)
		result.Complete(nil)
		require.False(t, store.PayloadBuildFlightActive(id))
		require.NoError(t, store.Catalog().SetViewGenerationState(t.Context(), id, ViewGenerationReady, ViewGenerationBuilding))
		absent, leader, ready, err := store.JoinPayloadBuildFlightHandle(t.Context(), id, true)
		require.NoError(t, err)
		require.False(t, leader)
		require.True(t, ready)
		if absent != nil {
			t.Fatal("a nil native ready flight was boxed")
		}
	})
	t.Run("fold", func(t *testing.T) {
		store := openCatalogStore(t)
		chain := foldChain(t, store, 2)
		id := reservedGeneration(t, store, "adapter-fold")
		result, err := store.BeginChainFoldHandle(t.Context(), ChainFoldRequest{Chain: chain, To: id})
		require.NoError(t, err)
		native, ok := result.(*ChainFold)
		require.True(t, ok)
		require.Same(t, native, result)
		require.Same(t, store, native.s)
		require.Equal(t, id, native.to)
		require.NoError(t, result.Release(t.Context()))
		require.False(t, store.PayloadBuildFlightActive(id))
	})
	t.Run("publication", func(t *testing.T) {
		store := openCatalogStore(t)
		id := reservedGeneration(t, store, "adapter-publication")
		result, err := store.PreparePayloadGenerationPublicationHandle(t.Context(), id)
		require.NoError(t, err)
		native, ok := result.(*PreparedPayloadGenerationPublication)
		require.True(t, ok)
		require.Same(t, native, result)
		require.Same(t, store, native.store)
		require.Equal(t, id, native.generationID)
	})
	t.Run("correction", func(t *testing.T) {
		store, id := publishedStampGeneration(t)
		req := DerivedCorrectionRequest{GenerationID: id, Pass: "capability", FromVersion: 1, ToVersion: 2, EdgeKinds: []graph.EdgeKind{graph.EdgeAccessesField}}
		result, err := store.BeginDerivedCorrectionHandle(t.Context(), req)
		require.NoError(t, err)
		native, ok := result.(*DerivedCorrection)
		require.True(t, ok)
		require.Same(t, native, result)
		require.Same(t, store, native.s)
		require.Equal(t, req, native.req)
		_, err = result.Finish(t.Context())
		require.NoError(t, err)
	})
}

func TestPayloadResultAdaptersReturnNilInterfacesOnRefusal(t *testing.T) {
	store := openCatalogStore(t)
	flight, _, _, err := store.JoinPayloadBuildFlightHandle(t.Context(), 0, false)
	require.ErrorIs(t, err, ErrCatalogInvalidValue)
	if flight != nil {
		t.Fatal("nil native flight was boxed")
	}
	fold, err := store.BeginChainFoldHandle(t.Context(), ChainFoldRequest{})
	require.ErrorIs(t, err, ErrCatalogInvalidValue)
	if fold != nil {
		t.Fatal("nil native fold was boxed")
	}
	publication, err := store.PreparePayloadGenerationPublicationHandle(t.Context(), 0)
	require.ErrorIs(t, err, ErrCatalogInvalidValue)
	if publication != nil {
		t.Fatal("nil native publication was boxed")
	}
	correction, err := store.BeginDerivedCorrectionHandle(t.Context(), DerivedCorrectionRequest{})
	require.ErrorIs(t, err, ErrDerivedCorrectionRefused)
	if correction != nil {
		t.Fatal("nil native correction was boxed")
	}
}

type foreignPayloadFlight struct{ calls int }

func (f *foreignPayloadFlight) Wait(context.Context) error { f.calls++; panic("foreign Wait called") }
func (f *foreignPayloadFlight) Complete(error)             { f.calls++; panic("foreign Complete called") }

func TestChainFoldHandleRejectsForeignFlightWithoutCallingIt(t *testing.T) {
	store := openCatalogStore(t)
	chain := foldChain(t, store, 2)
	id := reservedGeneration(t, store, "foreign-interface")
	foreign := &foreignPayloadFlight{}
	var typedNil *foreignPayloadFlight
	for _, flight := range []PayloadBuildFlightHandle{foreign, typedNil} {
		fold, err := store.BeginChainFoldHandle(t.Context(), ChainFoldRequest{Chain: chain, To: id, BuildFlight: flight})
		require.ErrorIs(t, err, ErrCatalogInvalidValue)
		if fold != nil {
			t.Fatal("foreign flight returned a fold")
		}
		require.Zero(t, foreign.calls)
		require.False(t, store.PayloadBuildFlightActive(id))
	}
}

func TestChainFoldHandleNilFlightsKeepAbsentMeaning(t *testing.T) {
	for _, name := range []string{"literal_nil", "native_typed_nil"} {
		t.Run(name, func(t *testing.T) {
			store := openCatalogStore(t)
			chain := foldChain(t, store, 2)
			id := reservedGeneration(t, store, name)
			req := ChainFoldRequest{Chain: chain, To: id}
			if name == "native_typed_nil" {
				req.BuildFlight = (*PayloadBuildFlight)(nil)
			}
			fold, err := store.BeginChainFoldHandle(t.Context(), req)
			require.NoError(t, err)
			require.True(t, store.PayloadBuildFlightActive(id))
			require.NoError(t, fold.Release(t.Context()))
			require.False(t, store.PayloadBuildFlightActive(id))
		})
	}
}

func TestAttachmentPublicationRefusesForeignAndNilStoreValues(t *testing.T) {
	store := openCatalogStore(t)
	var absent *Store
	for name, candidate := range map[string]graph.Store{"foreign": graph.New(), "nil": nil, "native_typed_nil": absent} {
		t.Run(name, func(t *testing.T) {
			result, err := store.PublishContractAttachmentsWithInputsContext(t.Context(), []ContractAttachmentPublishTarget{{Store: candidate, Witnesses: []graph.ContractInputWitness{{}}}}, 1)
			require.ErrorIs(t, err, ErrCatalogInvalidValue)
			require.ErrorContains(t, err, "attachment stores differ")
			require.Nil(t, result)
		})
	}
}
