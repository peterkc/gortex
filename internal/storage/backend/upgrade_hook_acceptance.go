//go:build acceptance

package backend

import "github.com/zzet/gortex/internal/graph/store_sqlite"

// WithAcceptanceUpgradeHook binds a callback to a single managed open.
func WithAcceptanceUpgradeHook(callback func(string, int)) UpgradeHook {
	if callback == nil {
		return UpgradeHook{}
	}
	return UpgradeHook{option: store_sqlite.WithAcceptanceUpgradeHook(callback)}
}
