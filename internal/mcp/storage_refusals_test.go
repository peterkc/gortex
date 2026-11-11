package mcp

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/graph/store_sqlite"
	"github.com/zzet/gortex/internal/graphview"
)

type unprovenOutlineStore struct{ *store_sqlite.Store }

func (*unprovenOutlineStore) SharesSymbolSearchCore(any) bool { return false }

func TestOutlineCacheRefusesUnprovenAnalysisStoreIdentity(t *testing.T) {
	fixture, store := outlineFixture(t)
	scope := graph.NodeScope{RepoAllow: map[string]bool{"a": true}}
	_, ok := fixture.outlineCacheKey(store, scope, true)
	require.True(t, ok)

	s := &Server{graph: &unprovenOutlineStore{store}}
	key, ok := s.outlineCacheKey(store, scope, true)
	require.False(t, ok)
	require.Equal(t, outlineResponseKey{}, key)
}

type nilWithdrawer struct{}

func (s *nilWithdrawer) IsNilGenerationHandle() bool { return s == nil }
func (*nilWithdrawer) WithdrawProducer(context.Context, int64, string, string) error {
	panic("withdrawal reached a typed-nil store")
}

func TestRefViewWithdrawalSkipsTypedNilStore(t *testing.T) {
	var absent *nilWithdrawer
	files := &refViewFiles{store: absent, generationID: 1}
	files.withdraw()
}

type layerWithoutRouteReader struct {
	graphview.GenerationSourceHandle
}

type storeWithoutLayerRouteReader struct{ *store_sqlite.Store }

func (s storeWithoutLayerRouteReader) AtGenerationStore(id int64) graph.Store {
	return layerWithoutRouteReader{s.AtGeneration(id)}
}

func TestViewRouteCatalogReturnsNilForLayerWithoutRouteReads(t *testing.T) {
	stack := newViewStack(t)
	m := graphview.Materializer{Store: storeWithoutLayerRouteReader{stack.store}, Catalog: stack.store.Catalog(), Leases: stack.leases}
	view, err := m.MaterializeCheckout(t.Context(), viewTestWorktree)
	require.NoError(t, err)
	t.Cleanup(view.Close)
	require.NotEmpty(t, view.GenerationSources())
	require.IsType(t, layerWithoutRouteReader{}, view.GenerationSources()[0].Handle)
	require.Nil(t, viewRouteCatalog(&requestView{materialized: view}))
}
