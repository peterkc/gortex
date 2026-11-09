package storage

import "time"

// WALReclaimStats is the bounded reclaim's telemetry. Pause is how long the
// read gate was closed per attempt that closed it; ReaderWait is what readers
// actually spent held at the gate (per held read), measured on their side.
type WALReclaimStats struct {
	EditBurstWriterHold                        DurationHistogram
	EditBurstGateClosures                      int64
	DeferredEditBurst, DeferredInteractiveRead int64
	BulkExceptionHolds, HardCapExceptionHolds  int64

	// WriterHold covers each reclaim acquisition, including credit reacquisition.
	// WriterWait measures the first queued foreground waiter per reclaim hold.
	// Snapshots never wait on the current writer holder.
	WriterHold, WriterWait DurationHistogram
	// Closed admission before final reset, and from final-reset start until reopening.
	PauseDrain, PauseTruncate DurationHistogram

	// One first observed cause per final non-reset reclaim attempt, including
	// writer-demand refusal. Parked writer precedes intent if both are seen.
	CancelledWriterParked, CancelledWriteIntent, CancelledOther int64
	// Lower-bound sampled high-water since this store opened, not an exact
	// instantaneous or warm-run peak. Status reads never reset it.
	ObservedPeakWALBytes int64
	WALPeakObserved      bool
	ThresholdBytes       int64
	Attempts             int64 // attempts that passed the size check and the lease
	UrgentAttempts       int64 // admitted attempts whose final urgency was set
	Resets               int64 // TRUNCATE checkpoints that reset the log
	OpenGateResets       int64 // resets reached without closing the read gate
	// LastResortRuns counts attempts at walReclaimLastResortBytes (each may
	// hold the writer up to walReclaimMaxWriterHold).
	LastResortRuns int64
	WriterHoldMax  time.Duration
	WriterHoldLast time.Duration
	// AdaptiveWriterAttempts counts attempts selecting the longer urgent slice
	// permitted after a completed slow copy acquires a new WAL tail.
	AdaptiveWriterAttempts  int64
	AdaptiveWriterHoldMax   time.Duration
	AdaptiveWriterBudgetMax time.Duration
	Deferrals               int64 // attempts that gave up and backed off
	Skips                   int64 // refused: bulk lease/connection, checkpoint in flight
	Failures                int64 // driver/I/O errors (also backed off)
	FramesReclaimed         int64 // WAL frames discarded by successful resets
	BytesReclaimed          int64 // -wal bytes returned to the filesystem
	PauseCount              int64
	PauseTotal              time.Duration
	PauseMax                time.Duration
	PauseLast               time.Duration
	ReaderWaits             int64
	ReaderWaitTotal         time.Duration
	ReaderWaitMax           time.Duration
	Backoff                 time.Duration // current backoff after the last attempt
	LastOutcome             string
	LastReason              string
	// Build-lane yield (checkpoint_cycle_yield.go): PASSIVE attempts deferred
	// while a mutation cycle held the lane, reclaim attempts refused for the
	// same reason, background attempts of either kind a cycle cut short, and
	// PASSIVE attempts forced past the deferral bound.
	CycleDeferrals int64
	CycleRefusals  int64
	CycleYields    int64
	CycleForced    int64
	// CycleCeilingRuns counts reclaim attempts run despite a held lane
	// because the WAL was over its ceiling.
	CycleCeilingRuns int64
	CeilingBytes     int64
	// LeaseOverrides counts reclaim attempts run inside a generation bulk
	// window because the WAL was over its ceiling.
	LeaseOverrides int64
	// RetirementWaits counts retirement chunks that waited for the reclaim
	// with the WAL over its ceiling; RetirementWaitTimeouts the waits that
	// ran out and proceeded anyway.
	RetirementWaits        int64
	RetirementWaitTimeouts int64
	// RetirementEditYields counts retirement chunks that waited for an
	// edit-path writer (an announced mutation or a writer parked on the
	// gate); RetirementEditYieldTimeouts the waits that ran out.
	RetirementEditYields        int64
	RetirementEditYieldTimeouts int64
	// The incremental shrink (wal_shrink.go): big logs reset in place, the
	// slices that shrank their file afterwards, the bytes those slices
	// returned, and the longest writer hold of one slice.
	ShrinkInPlaceResets int64
	ShrinkSlices        int64
	ShrinkBytes         int64
	ShrinkSliceHoldMax  time.Duration
}
