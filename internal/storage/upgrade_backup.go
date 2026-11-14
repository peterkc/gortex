package storage

import "time"

// UpgradeBackup is one finalized pre-upgrade backup on disk.
type UpgradeBackup struct {
	Path    string
	Version int // schema version the backed-up store had
	Size    int64
	ModTime time.Time
}
