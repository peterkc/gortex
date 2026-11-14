package graphview

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/graph/store_sqlite"
)

type generationFactoryFixture struct {
	MaterializerStore
	missing bool
}

func (s *generationFactoryFixture) AtGenerationStore(generation int64) graph.Store {
	if s.missing {
		return graph.New()
	}
	return s.MaterializerStore.AtGenerationStore(generation)
}

func TestMaterializerRefusesGenerationWithoutSourceReads(t *testing.T) {
	store := openStackStore(t, "missing-source-reads")
	generation := writeStackCommitGeneration(t, store)
	m := newTestMaterializer(store)
	factory := &generationFactoryFixture{MaterializerStore: store, missing: true}
	m.Store = factory
	handle, layer, _, err := m.openGeneration(t.Context(), generation)
	require.Equal(t, CodeCheckoutInaccessible, CodeOf(err))
	require.ErrorContains(t, err, "generation handle lacks source reads")
	require.Nil(t, handle)
	require.Nil(t, layer)
}

func TestWarmRouteRefusesPreloadWithoutLayerReads(t *testing.T) {
	store := openStackStore(t, "missing-preload-reads")
	generation := writeStackCommitGeneration(t, store)
	m := newTestMaterializer(store)
	factory := &generationFactoryFixture{MaterializerStore: store}
	m.Store = factory
	m.newGenerationLayer = func(ctx context.Context, handle GenerationLayerHandle) (*GenerationLayer, error) {
		layer, err := NewGenerationLayerContext(ctx, handle)
		factory.missing = true
		return layer, err
	}
	_, err := m.WarmRoute(t.Context(), generation)
	require.Equal(t, CodeCheckoutInaccessible, CodeOf(err))
	require.ErrorContains(t, err, "generation handle lacks layer reads")
}

func TestContractInputsRefuseBaseWithoutInputReads(t *testing.T) {
	store := openTestStore(t)
	m := newTestMaterializer(store)
	m.Store = &generationFactoryFixture{MaterializerStore: store, missing: true}
	inputs, err := m.CaptureContractInputs(t.Context(), nil, "repo", "")
	require.ErrorIs(t, err, graph.ErrContractInputVector)
	require.Nil(t, inputs)
}

func TestContractCompanionInputsRefuseBaseWithoutInputReads(t *testing.T) {
	store := openTestStore(t)
	generation := contractInputGeneration(t, store, "dedicated", 0, acceptedContractInput(stackRepo, testCheckoutID, "selected"))
	m, view := materializeContractInputs(t, store, generation)
	factory := &generationFactoryFixture{MaterializerStore: store, missing: true}
	m.Store = factory
	view.contractStore = factory
	inputs, err := m.CaptureContractCompanionInputs(t.Context(), view, "companion", "")
	require.ErrorIs(t, err, graph.ErrContractInputVector)
	require.Nil(t, inputs)
}

func TestContractReadinessRefusesBaseWithoutReadinessReads(t *testing.T) {
	store := openTestStore(t)
	m := newTestMaterializer(store)
	m.Store = &generationFactoryFixture{MaterializerStore: store, missing: true}
	_, err := m.ContractReadiness(t.Context(), ContractSelection{RepoPrefixes: []string{"repo"}})
	require.ErrorIs(t, err, graph.ErrContractInputVector)
	lease := m.Leases.Acquire(BaseCorpusGeneration)
	t.Cleanup(lease.Release)
	view := &RepoView{contractStore: m.Store, lease: lease}
	_, err = view.ContractReadiness(t.Context(), ContractSelection{RepoPrefixes: []string{"repo"}})
	require.ErrorIs(t, err, graph.ErrContractInputVector)
}

func TestGenerationConstructionKeepsNativeTypedNilRefusals(t *testing.T) {
	var handle *store_sqlite.Store
	layer, err := NewGenerationLayerContext(t.Context(), handle)
	require.Error(t, err)
	require.Nil(t, layer)
	store := openTestStore(t)
	m := newTestMaterializer(store)
	m.Store = handle
	require.Equal(t, CodeInvalidViewSelector, CodeOf(m.validate()))
	var catalog *store_sqlite.Catalog
	m.Store = store
	m.Catalog = catalog
	require.Equal(t, CodeInvalidViewSelector, CodeOf(m.validate()))
}
