package store_sqlite

import "github.com/zzet/gortex/internal/storage"

const (
	CleanupCauseUntrack               = storage.CleanupCauseUntrack
	CleanupCauseForget                = storage.CleanupCauseForget
	CleanupCauseReloadRemoved         = storage.CleanupCauseReloadRemoved
	CleanupCauseDemotion              = storage.CleanupCauseDemotion
	CleanupCauseReconcileRemoval      = storage.CleanupCauseReconcileRemoval
	CleanupCauseReconcileAvailability = storage.CleanupCauseReconcileAvailability
)

var CleanupCauseAllowsConfigRemoval = storage.CleanupCauseAllowsConfigRemoval
