//go:build acceptance

package serverstack

import "github.com/zzet/gortex/internal/storage/backend"

// SetAcceptanceUpgradeHook binds one callback to this config's actual Open.
// No setter exists in an ordinary build.
func SetAcceptanceUpgradeHook(cfg *SharedServerConfig, callback func(string, int)) {
	cfg.upgradeHook = backend.WithAcceptanceUpgradeHook(callback)
}
