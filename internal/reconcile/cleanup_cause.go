package reconcile

import (
	"context"

	"github.com/zzet/gortex/internal/storage"
)

const (
	CauseUntrack               = storage.CleanupCauseUntrack
	CauseForget                = storage.CleanupCauseForget
	CauseReloadRemoved         = storage.CleanupCauseReloadRemoved
	CauseDemotion              = storage.CleanupCauseDemotion
	CauseReconcileRemoval      = storage.CleanupCauseReconcileRemoval
	CauseReconcileAvailability = storage.CleanupCauseReconcileAvailability
)

// ConfigRemovalAllowed names the only authorized reasons to persist untracking.
// Unknown causes, including legacy missing causes, always keep configuration.
func ConfigRemovalAllowed(cause string) bool {
	return storage.CleanupCauseAllowsConfigRemoval(cause)
}

func (r *Reconciler) inferCleanupCause(ctx context.Context, target sagaTarget) (string, error) {
	if target.Cause != "" {
		return target.Cause, nil
	}
	if target.Kind == sagaRetireGraph {
		return CauseDemotion, nil
	}
	if target.CheckoutID != "" {
		intents, err := r.catalog.ListTrackingIntents(ctx, target.CheckoutID)
		if err != nil {
			return "", err
		}
		for _, intent := range intents {
			if !intent.Active && intent.RevokedAt != 0 {
				return CauseUntrack, nil
			}
		}
	}
	return CauseReconcileRemoval, nil
}

// Legacy automatic cleanup must not resume destructive phases merely
// because an old journal omitted the user's authorization cause.
func (r *Reconciler) refuseAutomaticCleanup(ctx context.Context, target sagaTarget) (bool, error) {
	if (target.Kind != sagaRetirePrimaryClosure && target.Kind != sagaForgetCheckout) || ConfigRemovalAllowed(target.Cause) {
		return false, nil
	}
	owner, found, err := r.catalog.GetCheckout(ctx, target.CheckoutID)
	if err != nil || !found {
		return false, err
	}
	family, found, err := r.catalog.GetRepositoryFamily(ctx, owner.FamilyID)
	if err != nil || !found {
		return false, err
	}
	req := observationFrom(owner)
	_, retained, err := r.retainIntended(ctx, &familyPass{family: family, now: r.now()}, owner, &req)
	if !retained && err == nil {
		return false, nil
	}
	if err != nil {
		return true, err
	}
	if err = r.catalog.UpdateCheckoutObservation(ctx, req); err != nil {
		return true, err
	}
	if target.AttemptID != "" {
		return true, r.catalog.DeleteCleanupAttempt(ctx, target.cleanupID(), target.AttemptID)
	}
	return true, r.catalog.DeleteCleanupEntry(ctx, target.cleanupID())
}
