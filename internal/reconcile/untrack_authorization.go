package reconcile

import (
	"context"
	"encoding/json"
	"fmt"

	"uuid"

	"github.com/zzet/gortex/internal/storage"
)

const explicitUntrackDemotionCause = "explicit_untrack_demote"

// DemotionAuthorization is the durable right to move one checkout onto the
// family's primary. Intent revocation and creation of Transition happened in
// the same catalog transaction; the final mode flip still rechecks the primary
// epoch after the replacement layers have been built.
type DemotionAuthorization struct {
	Revocation IntentRevocation
	Transition storage.IntentTransition

	OwnedGraphID   string
	PrimaryGraphID string
	PrimaryEpoch   int64
}

// DemotionCommitResult distinguishes a refused publication from a committed
// mode flip whose separately journalled graph cleanup still needs a retry.
type DemotionCommitResult struct {
	Committed bool
}

// ForgetCheckoutExplicit atomically revokes explicit tracking intent and starts
// the existing forget saga. Automatic disappearance callers keep using
// ForgetCheckout and therefore do not participate in intent authorization.
func (r *Reconciler) ForgetCheckoutExplicit(
	ctx context.Context,
	checkoutID, incarnation, familyID, graphID string,
) (IntentRevocation, error) {
	target := sagaTarget{
		Kind:        sagaForgetCheckout,
		Cause:       CauseUntrack,
		CheckoutID:  checkoutID,
		Incarnation: incarnation,
		FamilyID:    familyID,
		GraphID:     graphID,
	}
	revocation, authorized, err := r.authorizeExplicitCleanup(ctx, target,
		storage.UntrackAuthorizationForget)
	if err != nil {
		return revocation, err
	}
	return revocation, r.runSaga(ctx, authorized)
}

// RetirePrimaryClosureExplicit is the explicit counterpart of
// RetirePrimaryClosure. The preview's epoch guard, intent revocation and first
// cleanup-journal row are one transaction; after that commit the ordinary saga
// runner owns completion and restart recovery.
func (r *Reconciler) RetirePrimaryClosureExplicit(
	ctx context.Context,
	graphID, checkoutID, incarnation, familyID string,
	primaryEpoch int64,
) (IntentRevocation, error) {
	target := sagaTarget{
		Kind:         sagaRetirePrimaryClosure,
		Cause:        CauseUntrack,
		GraphID:      graphID,
		CheckoutID:   checkoutID,
		Incarnation:  incarnation,
		FamilyID:     familyID,
		PrimaryEpoch: primaryEpoch,
	}
	revocation, authorized, err := r.authorizeExplicitCleanup(ctx, target,
		storage.UntrackAuthorizationPrimaryClosure)
	if err != nil {
		return revocation, err
	}
	return revocation, r.runSaga(ctx, authorized)
}

func (r *Reconciler) authorizeExplicitCleanup(
	ctx context.Context,
	target sagaTarget,
	plan storage.UntrackAuthorizationPlan,
) (IntentRevocation, sagaTarget, error) {
	// This method is the explicit authorization boundary, including callers
	// that construct a target directly. Its durable cause is never inferred.
	target.Cause = CauseUntrack
	entry, err := r.pendingCleanupEntry(target)
	if err != nil {
		return IntentRevocation{}, sagaTarget{}, err
	}
	req := storage.AuthorizeUntrackRequest{
		Plan:           plan,
		CheckoutID:     target.CheckoutID,
		Incarnation:    target.Incarnation,
		FamilyID:       target.FamilyID,
		OwnedGraphID:   target.GraphID,
		RevokedAt:      r.now().Unix(),
		RevocableKinds: revocableIntentKinds,
		Cleanup:        &entry,
	}
	if plan == storage.UntrackAuthorizationPrimaryClosure {
		req.PrimaryGraphID = target.GraphID
		req.ExpectedPrimaryEpoch = target.PrimaryEpoch
	}
	result, err := r.catalog.AuthorizeUntrack(ctx, req)
	revocation := IntentRevocation{Revoked: result.Revoked, Blocked: result.Blocked}
	if err != nil {
		return revocation, sagaTarget{}, err
	}
	if revocation.IsBlocked() {
		return revocation, sagaTarget{}, fmt.Errorf(
			"%w: checkout %s is still wanted by %d intent(s)",
			ErrIntentNotRevocable, target.CheckoutID, len(revocation.Blocked))
	}
	if result.Cleanup == nil {
		return revocation, sagaTarget{}, fmt.Errorf(
			"%w: authorization for checkout %s recorded no cleanup", ErrSagaTarget, target.CheckoutID)
	}
	authorized, err := decodeSagaTarget(*result.Cleanup)
	if err != nil {
		return revocation, sagaTarget{}, err
	}
	return revocation, authorized, nil
}

// AuthorizeDemotion atomically revokes explicit tracking intents and records a
// retryable demotion transition. It does not build or route anything; those
// potentially slow operations happen only after the durable authorization is
// visible.
func (r *Reconciler) AuthorizeDemotion(
	ctx context.Context,
	checkout storage.Checkout,
	ownedGraphID, primaryGraphID string,
	primaryEpoch int64,
) (DemotionAuthorization, error) {
	now := r.now().Unix()
	transition := storage.IntentTransition{
		TransitionID:       uuid.NewV7().String(),
		CheckoutID:         checkout.CheckoutID,
		Cause:              explicitUntrackDemotionCause,
		PriorDesiredMode:   checkout.DesiredMode,
		PriorEffectiveMode: checkout.EffectiveMode,
		RequestedMode:      storage.CheckoutModeAutomatic,
		PriorCheckoutState: checkout.State,
		SourceSnapshotHash: fmt.Sprintf("%s:%s:%d", ownedGraphID, primaryGraphID, primaryEpoch),
		State:              storage.IntentTransitionRunning,
		CreatedAt:          now,
		LastProgress:       now,
	}
	result, err := r.catalog.AuthorizeUntrack(ctx, storage.AuthorizeUntrackRequest{
		Plan:                 storage.UntrackAuthorizationDemote,
		CheckoutID:           checkout.CheckoutID,
		Incarnation:          checkout.Incarnation,
		FamilyID:             checkout.FamilyID,
		OwnedGraphID:         ownedGraphID,
		PrimaryGraphID:       primaryGraphID,
		ExpectedPrimaryEpoch: primaryEpoch,
		RequiredPrimaryState: GraphStateReady,
		RevokedAt:            now,
		RevocableKinds:       revocableIntentKinds,
		Transition:           &transition,
	})
	authorization := DemotionAuthorization{
		Revocation:     IntentRevocation{Revoked: result.Revoked, Blocked: result.Blocked},
		OwnedGraphID:   ownedGraphID,
		PrimaryGraphID: primaryGraphID,
		PrimaryEpoch:   primaryEpoch,
	}
	if result.Transition != nil {
		authorization.Transition = *result.Transition
	}
	if err != nil {
		return authorization, err
	}
	if authorization.Revocation.IsBlocked() {
		return authorization, fmt.Errorf(
			"%w: checkout %s is still wanted by %d intent(s)",
			ErrIntentNotRevocable, checkout.CheckoutID, len(authorization.Revocation.Blocked))
	}
	if authorization.Transition.TransitionID == "" {
		return authorization, fmt.Errorf(
			"%w: demotion authorization for checkout %s recorded no transition",
			ErrSagaTarget, checkout.CheckoutID)
	}
	return authorization, nil
}

// CommitAuthorizedDemotion publishes the mode flip and, when necessary,
// journals retirement of the checkout's former dedicated graph in the same
// transaction. The returned Committed flag remains true when only execution of
// that already-durable cleanup failed.
func (r *Reconciler) CommitAuthorizedDemotion(
	ctx context.Context,
	checkout storage.Checkout,
	authorization DemotionAuthorization,
) (DemotionCommitResult, error) {
	var cleanup *storage.CleanupEntry
	if authorization.OwnedGraphID != "" {
		target := sagaTarget{
			Kind:       sagaRetireGraph,
			Cause:      CauseDemotion,
			GraphID:    authorization.OwnedGraphID,
			FamilyID:   checkout.FamilyID,
			CheckoutID: checkout.CheckoutID,
		}
		entry, err := r.pendingCleanupEntry(target)
		if err != nil {
			return DemotionCommitResult{}, err
		}
		cleanup = &entry
	}
	if err := r.catalog.CommitAuthorizedDemotion(ctx, storage.CommitAuthorizedDemotionRequest{
		CheckoutID:           checkout.CheckoutID,
		Incarnation:          checkout.Incarnation,
		FamilyID:             checkout.FamilyID,
		TransitionID:         authorization.Transition.TransitionID,
		OwnedGraphID:         authorization.OwnedGraphID,
		PrimaryGraphID:       authorization.PrimaryGraphID,
		ExpectedPrimaryEpoch: authorization.PrimaryEpoch,
		RequiredPrimaryState: GraphStateReady,
		State:                storage.CheckoutStateReady,
		LastSeen:             r.now().Unix(),
		Cleanup:              cleanup,
	}); err != nil {
		return DemotionCommitResult{}, err
	}
	result := DemotionCommitResult{Committed: true}
	if authorization.OwnedGraphID == "" {
		return result, nil
	}
	return result, r.RetireDedicatedGraph(ctx, authorization.OwnedGraphID)
}

func (r *Reconciler) pendingCleanupEntry(target sagaTarget) (storage.CleanupEntry, error) {
	if target.AttemptID == "" {
		var err error
		target.AttemptID, err = newCleanupAttemptID()
		if err != nil {
			return storage.CleanupEntry{}, err
		}
	}
	payload, err := json.Marshal(target)
	if err != nil {
		return storage.CleanupEntry{}, fmt.Errorf(
			"%w: encoding %s: %w", ErrSagaTarget, target.Kind, err)
	}
	return storage.CleanupEntry{
		CleanupID:       target.cleanupID(),
		OpaqueTargetIDs: string(payload),
		Reason:          string(target.Kind),
		Phase:           storage.CleanupPhasePending,
		PrimaryEpoch:    target.PrimaryEpoch,
		LastProgress:    r.now().Unix(),
	}, nil
}
