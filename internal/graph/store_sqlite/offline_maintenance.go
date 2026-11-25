package store_sqlite

import (
	"time"

	"github.com/zzet/gortex/internal/graph"
)

// OfflineMaintenance exposes the file-only operations without opening or owning
// a Store. Each operation keeps its existing read or removal policy.
type OfflineMaintenance struct{}

func (OfflineMaintenance) ReadCloseProgress(path string) (CloseProgress, bool) {
	return ReadCloseProgress(path)
}

func (OfflineMaintenance) ReadRepoIndexStates(path string) (map[string]graph.RepoIndexState, error) {
	return ReadRepoIndexStates(path)
}

func (OfflineMaintenance) RemoveUpgradeBackups(path string) ([]UpgradeBackup, error) {
	return RemoveUpgradeBackups(path)
}

func (OfflineMaintenance) PruneUpgradeBackups(path string, now time.Time) ([]string, error) {
	return PruneUpgradeBackups(path, UpgradeBackupRetention, now)
}
