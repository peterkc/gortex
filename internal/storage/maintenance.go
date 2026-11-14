package storage

import "time"

// CloseProgress is the close's progress as the stop path reads it.
type CloseProgress struct {
	PID                  int     `json:"pid"`
	Phase                string  `json:"phase"` // draining | done | failed
	StartedUnixNano      int64   `json:"started_unix_nano"`
	UpdatedUnixNano      int64   `json:"updated_unix_nano"`
	LastProgressUnixNano int64   `json:"last_progress_unix_nano"`
	PendingFrames        int64   `json:"pending_frames"`
	PerFrameNanos        int64   `json:"per_frame_nanos"`
	EstimateMillis       int64   `json:"estimate_millis"`
	ElapsedMillis        int64   `json:"elapsed_millis"`
	FramesCopiedEstimate int64   `json:"frames_copied_estimate"`
	RateFramesPerSec     float64 `json:"rate_frames_per_sec"`
	RemainingMillis      int64   `json:"remaining_millis"`
}

// Since reports how long ago the close last showed progress.
func (p CloseProgress) Since(now time.Time) time.Duration {
	if p.LastProgressUnixNano == 0 {
		return now.Sub(time.Unix(0, p.StartedUnixNano))
	}
	return now.Sub(time.Unix(0, p.LastProgressUnixNano))
}

// MigrationPhase identifies one observable boundary in an in-place schema
// migration. Observers are advisory: they must not block the migration or
// mutate the graph database.
type MigrationPhase string

const (
	MigrationStarted  MigrationPhase = "started"
	MigrationFinished MigrationPhase = "finished"
	MigrationFailed   MigrationPhase = "failed"

	// The backup phases report the pre-upgrade backup Open takes under
	// WithUpgradeBackup, once, before the first step starts. Version is the
	// schema version being backed up and Detail carries the backup path
	// (created, kept) or the reason none was taken (skipped).
	MigrationBackupCreated MigrationPhase = "backup_created"
	MigrationBackupKept    MigrationPhase = "backup_kept"
	MigrationBackupSkipped MigrationPhase = "backup_skipped"
)

// MigrationProgress describes one schema migration step. Elapsed is measured
// from the start of the current step and Error is populated only for a failed
// step. The error is intended for logs and transient startup state; callers
// should avoid persisting it as durable graph data.
type MigrationProgress struct {
	Version int
	Name    string
	Phase   MigrationPhase
	Elapsed time.Duration
	Error   error
	Detail  string
}

// MigrationObserver receives synchronous migration boundaries. It may be nil.
// Implementations should return promptly; a slow observer delays store Open.
type MigrationObserver func(MigrationProgress)
