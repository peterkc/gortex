package mcp

import (
	"context"
	"testing"

	mcpgo "github.com/mark3labs/mcp-go/mcp"
	"github.com/stretchr/testify/require"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/graph/store_sqlite"
	"github.com/zzet/gortex/internal/graphview"
)

func TestCheckoutCatalogAbsenceKeepsRefusalAndLookupOutcomes(t *testing.T) {
	var absent *store_sqlite.Catalog
	for _, typedNil := range []bool{false, true} {
		t.Run(map[bool]string{false: "nil", true: "typed_nil"}[typedNil], func(t *testing.T) {
			materializer := &graphview.Materializer{}
			if typedNil {
				materializer.Catalog = absent
			}
			server := &Server{materializer: materializer}
			ctx := t.Context()
			req := &mcpgo.CallToolRequest{}
			selector := graphview.Selector{Kind: graphview.SelectorWorktree, CheckoutID: "missing"}
			selected, selectErr := server.selectRequestView(ctx, selector, requestViewPolicy{})
			require.Nil(t, selected)
			require.Equal(t, graphview.CodeCapabilityUnavailable, graphview.CodeOf(selectErr))
			control, err := server.resolveCheckoutControlScope(ctx, selector, req)
			require.Nil(t, control)
			require.Equal(t, graphview.CodeCapabilityUnavailable, graphview.CodeOf(err))
			control, err = server.resolveCheckoutControlScope(ctx, graphview.Selector{Kind: graphview.SelectorAuto}, req)
			require.NoError(t, err)
			require.Nil(t, control)
			rider := graphview.NewViewRider(selector)
			rider.CheckoutID = "missing"
			view := &requestView{rider: rider}
			_, err = server.registeredWorktreeSelector(ctx, selector)
			require.Equal(t, graphview.CodeCapabilityUnavailable, graphview.CodeOf(err))
			_, reason, waitable := server.freshnessWaitTarget(ctx, selector, view, nil)
			require.False(t, waitable)
			require.Equal(t, freshReasonWaitTargetUnavailable, reason)
			_, found := server.publishedCheckoutRoute(ctx, "missing")
			require.False(t, found)
			require.NoError(t, server.validateSourceCheckoutIdentity(ctx, view))
		})
	}
}

func TestPendingSourceRetirementKeepsGraphFallbackWithoutMaterializerCatalog(t *testing.T) {
	store, err := store_sqlite.Open(":memory:")
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, store.Close()) })
	server := &Server{graph: store, materializer: &graphview.Materializer{}}
	record := checkoutSourceVersion(server, "missing", "old", "/repo/file.go")
	require.Contains(t, server.pendingSourceFiles.versions, record.id)
	server.retireDeadPendingSourceIncarnations()
	require.NotContains(t, server.pendingSourceFiles.versions, record.id)
}

type noSelectedMasks struct {
	graphview.GenerationSourceHandle
}

func (noSelectedMasks) PayloadInputRevisionContext(context.Context) (store_sqlite.PayloadInputRevision, error) {
	return store_sqlite.PayloadInputRevision{}, nil
}

func TestSelectedDeletionRefusesHandleWithoutMaskReads(t *testing.T) {
	layer := graph.NewOverlayLayer()
	layer.MarkFile("repo/file.go", true)
	projection := &selectedProjectionStore{
		ctx: t.Context(), entry: &projectionEntry{repoPrefix: "repo", graphPath: "repo/file.go"},
		sources: []graphview.GenerationSource{{Handle: noSelectedMasks{}, Layer: layer}},
	}
	deleted, err := projection.selectedDeletion()
	require.False(t, deleted)
	require.ErrorIs(t, err, graph.ErrConstantProjectionUnsupported)
}

// Method promotion must not grant native admission to a decorated value.
type decoratedNativeConsumer struct{ *store_sqlite.Store }

func TestNativeConsumerAdmissionKeepsDecoratedFallback(t *testing.T) {
	store, err := store_sqlite.Open(":memory:")
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, store.Close()) })
	decorated := &decoratedNativeConsumer{Store: store}
	require.Same(t, decorated, bindAnalysisWriteContext(decorated, t.Context()))
	server := &Server{graph: decorated}
	require.Same(t, decorated, server.analysisGenerationStore())
	_, ok := server.outlineCacheKey(decorated, graph.NodeScope{}, true)
	require.False(t, ok)
	_, _, _, ok = contractCoreExclusionSource(decorated)
	require.False(t, ok)
	_, ok = adaptStoreFTSRankSource(decorated)
	require.False(t, ok)
	var absent *store_sqlite.Store
	require.Nil(t, bindAnalysisWriteContext(absent, t.Context()))
}

type catalogWithoutCheckoutRead struct{ *store_sqlite.Catalog }

func (catalogWithoutCheckoutRead) GetCheckout() {}

func TestSourceIdentityRefusesCatalogWithoutCheckoutReads(t *testing.T) {
	store, err := store_sqlite.Open(":memory:")
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, store.Close()) })
	server := &Server{materializer: &graphview.Materializer{Catalog: catalogWithoutCheckoutRead{Catalog: store.Catalog()}}}
	view := &requestView{rider: graphview.NewViewRider(graphview.Selector{Kind: graphview.SelectorWorktree})}
	err = server.validateSourceCheckoutIdentity(t.Context(), view)
	require.Equal(t, graphview.CodeCapabilityUnavailable, graphview.CodeOf(err))
}
