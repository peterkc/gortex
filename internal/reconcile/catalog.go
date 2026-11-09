package reconcile

import (
	"context"

	"github.com/zzet/gortex/internal/storage"
)

// Catalog is the control-plane capability the reconciler calls.
type Catalog interface {
	AdvanceCleanupAttempt(ctx context.Context, entry storage.CleanupEntry, expected string) error
	AllocateCheckout(ctx context.Context, checkout storage.Checkout) error
	AuthorizeUntrack(ctx context.Context, req storage.AuthorizeUntrackRequest) (storage.AuthorizeUntrackResult, error)
	BeginIntentTransition(ctx context.Context, transition storage.IntentTransition) error
	CommitAuthorizedDemotion(ctx context.Context, req storage.CommitAuthorizedDemotionRequest) error
	CreateCleanupAttempt(ctx context.Context, entry storage.CleanupEntry, expected string) error
	DeleteAutomaticCheckout(ctx context.Context, checkoutID, incarnation string) error
	DeleteCheckout(ctx context.Context, checkoutID string) error
	DeleteCheckoutRoute(ctx context.Context, checkoutID string) error
	DeleteCleanupAttempt(ctx context.Context, cleanupID, expected string) error
	DeleteCleanupEntry(ctx context.Context, cleanupID string) error
	DeleteCompletedCleanupForRecovery(ctx context.Context, checkoutID, incarnation, cleanupID string) error
	DeleteDedicatedGraph(ctx context.Context, graphID string) error
	DeleteRefView(ctx context.Context, refViewID string) error
	DeleteRepositoryFamily(ctx context.Context, familyID string) error
	GetCheckout(ctx context.Context, checkoutID string) (storage.Checkout, bool, error)
	GetCheckoutPathEvidence(ctx context.Context, checkoutID string) (storage.CheckoutPathEvidence, bool, error)
	GetCheckoutRoute(ctx context.Context, checkoutID string) (storage.CheckoutRoute, bool, error)
	GetCleanupAttempt(ctx context.Context, cleanupID, expected string) (storage.CleanupEntry, error)
	GetCleanupEntry(ctx context.Context, cleanupID string) (storage.CleanupEntry, bool, error)
	GetDedicatedGraph(ctx context.Context, graphID string) (storage.DedicatedGraph, bool, error)
	GetIntentTransition(ctx context.Context, checkoutID string) (storage.IntentTransition, bool, error)
	GetRepositoryFamily(ctx context.Context, familyID string) (storage.RepositoryFamily, bool, error)
	ListCheckouts(ctx context.Context, familyID string) ([]storage.Checkout, error)
	ListCleanupEntries(ctx context.Context) ([]storage.CleanupEntry, error)
	ListDedicatedGraphs(ctx context.Context, familyID string) ([]storage.DedicatedGraph, error)
	ListRefViews(ctx context.Context, graphID string) ([]storage.RefView, error)
	ListTrackingIntents(ctx context.Context, checkoutID string) ([]storage.TrackingIntent, error)
	ListViewGenerations(ctx context.Context, filter storage.ViewGenerationFilter) ([]storage.ViewGeneration, error)
	RevokeTrackingIntents(
		ctx context.Context,
		checkoutID string,
		revokedAt int64,
		revocableKinds []storage.IntentSourceKind,
	) (revoked, blocked []storage.TrackingIntent, err error)
	UpdateCheckoutObservation(ctx context.Context, req storage.UpdateCheckoutObservationRequest) error
	UpgradeCleanupAttempt(ctx context.Context, snapshot storage.CleanupEntry, attemptID string) (storage.CleanupEntry, error)
	UpsertCheckoutPathEvidence(ctx context.Context, evidence storage.CheckoutPathEvidence) error
	UpsertCleanupEntry(ctx context.Context, entry storage.CleanupEntry) error
}
