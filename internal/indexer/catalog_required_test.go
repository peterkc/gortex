package indexer

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/zzet/gortex/internal/graph/store_sqlite"
	"github.com/zzet/gortex/internal/graphview"
	"go.uber.org/zap"
)

func TestGenerationOwnersRefuseMissingCatalog(t *testing.T) {
	store, err := store_sqlite.Open(":memory:")
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, store.Close()) })
	leases := graphview.NewLeaseManager()
	var nativeNil *store_sqlite.Catalog
	for name, catalog := range map[string]CheckoutCatalog{"nil": nil, "typed_nil": nativeNil} {
		t.Run(name, func(t *testing.T) {
			builder := &SparseGenerationBuilder{Store: store, Catalog: catalog, Registry: builderRegistry(), Logger: zap.NewNop()}
			require.EqualError(t, builder.validate(t.Context(), &BuildRequest{}), "indexer: sparse generation builder needs a catalog")

			runtime, err := NewDedicatedBaseRuntime(store, catalog, leases)
			require.ErrorIs(t, err, errDedicatedBaseRuntimeInput)
			require.ErrorContains(t, err, "requires the stack's catalog")
			require.Nil(t, runtime)

			checkout, err := NewCheckoutCoordinator(CheckoutCoordinatorConfig{Store: store, Catalog: catalog, Builder: builder, CheckoutID: "checkout", CheckoutRoot: t.TempDir(), FamilyID: "family"})
			require.EqualError(t, err, "indexer: checkout coordinator needs a catalog")
			require.Nil(t, checkout)

			ref, err := NewRefViewManager(RefViewManagerConfig{Store: store, Catalog: catalog, Builder: builder, Leases: leases})
			require.EqualError(t, err, "indexer: ref view manager needs a catalog")
			require.Nil(t, ref)

			analysis, err := NewContractAnalysisCoordinator(ContractAnalysisCoordinatorOptions{
				Store: store, Catalog: catalog, Leases: leases, Registry: builder.Registry, OpenEvidence: openTestEvidenceStore,
				Yield: func(ctx context.Context) error { return ctx.Err() },
				Capture: func(context.Context, *ContractCaptureSource, *graphview.SelectedContractInputs, string, string) (ContractFollowupSnapshot, []ContractFollowupTarget, error) {
					panic("constructor must not capture")
				},
			})
			if analysis != nil {
				t.Cleanup(func() { require.NoError(t, analysis.CloseContext(context.WithoutCancel(t.Context()))) })
			}
			require.EqualError(t, err, "contract analysis coordinator: paired checkout catalog required")
			require.Nil(t, analysis)

			_, err = captureAcceptedContractFiles(t.Context(), ContractFollowupCaptureOptions{
				Store: store, Catalog: catalog, Registry: builder.Registry, MultiIndexer: &MultiIndexer{},
				Materializer: &graphview.Materializer{Store: store, Catalog: store.Catalog(), Leases: leases},
			}, nil, nil)
			require.EqualError(t, err, "contract capture: paired checkout catalog required")
		})
	}
}
