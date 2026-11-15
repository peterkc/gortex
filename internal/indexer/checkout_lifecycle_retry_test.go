package indexer

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"testing/synctest"
	"time"

	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	"github.com/zzet/gortex/internal/config"
	"github.com/zzet/gortex/internal/graph/store_sqlite"
	"github.com/zzet/gortex/internal/reconcile"
)

func TestCheckoutLifecycleReconcileFamilyRetainsExpiredIntendedCheckout(t *testing.T) {
	f := newLifecycleFixture(t)
	defer f.close()
	ctx := context.Background()
	root := f.gitRepo("event-offline")
	tracked, err := f.lc.Register(ctx, config.RepoEntry{Path: root, Name: "event-offline"}, TrackSourceCLI)
	require.NoError(t, err)
	require.NoError(t, tracked.CatalogErr)
	evidence, _, err := f.catalog.GetCheckoutPathEvidence(ctx, tracked.CheckoutID)
	require.NoError(t, err)
	proven := evidence.CommonDirVolumeToken != ""
	require.NoError(t, os.RemoveAll(root))
	graced, err := f.lc.ReconcileFamily(ctx, tracked.FamilyID)
	require.NoError(t, err)
	entry := checkoutReportByID(t, graced, tracked.CheckoutID)
	if proven {
		require.Equal(t, reconcile.ActionRemovalGraceStarted, entry.Action)
	} else {
		require.Equal(t, reconcile.ActionInventoryUnknown, entry.Action)
	}
	f.lc.retryMu.Lock()
	retry, armed := f.lc.familyRetries[tracked.FamilyID]
	f.lc.retryMu.Unlock()
	require.True(t, armed, "unknown inventories and grace windows both need a retry")
	require.Greater(t, retry.deadline, f.clock.Now().Unix())
	f.clock.advance(lifecycleGrace.AvailabilityGrace + time.Second)
	expired, err := f.lc.ReconcileFamily(ctx, tracked.FamilyID)
	require.NoError(t, err)
	entry = checkoutReportByID(t, expired, tracked.CheckoutID)
	if proven {
		require.Equal(t, reconcile.ActionPrimaryRetirementRefused, entry.Action)
	} else {
		require.Equal(t, reconcile.ActionInventoryUnknown, entry.Action)
	}
	require.NotNil(t, f.mi.GetMetadata("event-offline"))
	require.Contains(t, f.configPaths(), root)
}

func TestCheckoutLifecycleFamilyRetryTimerFires(t *testing.T) {
	lifecycle := &CheckoutLifecycle{
		now:           time.Now,
		logger:        zap.NewNop(),
		familyRetries: map[string]familyRetry{},
		coordinators:  map[string]*CheckoutCoordinator{},
	}
	t.Cleanup(func() { require.NoError(t, lifecycle.Close()) })

	initial := time.Now().Unix()
	lifecycle.scheduleFamilyRetryAt("family", initial)
	require.Eventually(t, func() bool {
		lifecycle.retryMu.Lock()
		defer lifecycle.retryMu.Unlock()
		retry, ok := lifecycle.familyRetries["family"]
		return ok && retry.deadline > initial
	}, time.Second, 5*time.Millisecond,
		"the expired timer should run reconciliation and schedule a bounded retry after failure")
}

func TestFamilyRetryDefersClosingGenerationRetirementUntilReady(t *testing.T) {
	for _, release := range []string{"ready", "explicit_reconcile", "explicit_sweep"} {
		t.Run(release, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				// Given unleased positive generations under a reader-held closing root.
				f := newLifecycleFixture(t)
				t.Cleanup(f.close)
				f.lc.now = time.Now
				root := f.gitRepo("retry-closing")
				tracked, err := f.lc.Register(t.Context(), config.RepoEntry{Path: root, Name: builderRepoPrefix}, TrackSourceCLI)
				require.NoError(t, err)
				require.NoError(t, tracked.CatalogErr)
				template := store_sqlite.PayloadGenerationRequest{OwnerKind: checkoutLayerOwnerKind, GraphID: tracked.GraphID,
					CheckoutID: tracked.CheckoutID, GenerationKind: "commit", TreeOID: "retry-tree"}
				owed := seedRetirementPayload(t, f.store, template, "retry-owed", 1, 1)
				retiring := seedRetirementPayload(t, f.store, template, "retry-interrupted", 1, 1)
				require.NoError(t, f.catalog.SetViewGenerationState(t.Context(), retiring, store_sqlite.ViewGenerationRetiring, store_sqlite.ViewGenerationFailed))
				f.lc.oweRetirement(owed)
				pin := f.lc.ViewLeases().AcquireBaseCorpus(tracked.Prefix)
				t.Cleanup(pin.Release)
				f.lc.EnableDeferredRepositoryCleanup()
				out, err := f.lc.Untrack(t.Context(), root)
				require.NoError(t, err)
				require.True(t, out.Pending)

				// Seed uses this same report scheduler for unavailable family inventory.
				const retryFamily = "inventory-unknown-retry"
				require.NoError(t, f.catalog.UpsertRepositoryFamily(t.Context(), store_sqlite.RepositoryFamily{
					FamilyID: retryFamily, CommonDirIdentity: filepath.Join(t.TempDir(), "offline.git"), State: "active",
				}))
				fired := make(chan struct{}, 1)
				f.lc.familyRetryBarrier = func() { fired <- struct{}{} }
				f.lc.scheduleFamilyRetry(reconcile.FamilyReport{FamilyID: retryFamily})
				f.lc.retryMu.Lock()
				retry, armed := f.lc.familyRetries[retryFamily]
				f.lc.retryMu.Unlock()
				require.True(t, armed)
				require.Equal(t, time.Now().Add(30*time.Second).Unix(), retry.deadline)

				// When its real timer fires while cleanup release is still deferred.
				<-fired
				f.lc.retryWG.Wait()

				// Then neither an owed generation nor an interrupted release is swept.
				requireGenerationStateWithPayload(t, f.store, owed, store_sqlite.ViewGenerationFailed)
				requireGenerationStateWithPayload(t, f.store, retiring, store_sqlite.ViewGenerationRetiring)
				f.lc.coordMu.Lock()
				_, stillOwed := f.lc.owed[owed]
				f.lc.coordMu.Unlock()
				require.True(t, stillOwed)

				// When readiness opens, or an operator explicitly requests collection.
				switch release {
				case "ready":
					f.lc.MarkRepositoryCleanupReady()
					<-fired
					f.lc.retryWG.Wait()
				case "explicit_reconcile":
					_, err = f.lc.ReconcileFamily(t.Context(), retryFamily)
				case "explicit_sweep":
					_, err = f.lc.Sweep(t.Context())
				}

				// Then existing retirement collection still removes their payload.
				require.NoError(t, err)
				requireGenerationRetired(t, f.store, owed)
				requireGenerationRetired(t, f.store, retiring)
				f.lc.coordMu.Lock()
				_, stillOwed = f.lc.owed[owed]
				f.lc.coordMu.Unlock()
				require.False(t, stillOwed)
			})
		})
	}
}

func checkoutReportByID(t *testing.T, report reconcile.FamilyReport, checkoutID string) reconcile.CheckoutReport {
	t.Helper()
	for _, checkout := range report.Checkouts {
		if checkout.CheckoutID == checkoutID {
			return checkout
		}
	}
	t.Fatalf("checkout %s missing from report %+v", checkoutID, report)
	return reconcile.CheckoutReport{}
}
