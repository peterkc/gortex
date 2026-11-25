package storage

import (
	"errors"
	"time"
)

// ChainFoldRequest names the chain (oldest first) and the empty building
// generation the fold writes; Owner identifies the caller in logs.
type ChainFoldRequest struct {
	Chain []int64
	To    int64
	Owner string
	// StepTarget may request smaller transactions for a fold sharing the writer
	// with frequent interactive edits. Zero retains the default target.
	StepTarget time.Duration
	// BuildFlight optionally borrows the caller's live destination leader.
	// The caller retains it across Begin retries and through publication/landing;
	// neither a refused Begin nor Release completes borrowed ownership.
	BuildFlight PayloadBuildFlightHandle
}

var (
	// ErrChainFoldStale refuses a fold whose chain is not what it was: a
	// member missing, not ready, or being retired, or `to` not building.
	ErrChainFoldStale = errors.New("store_sqlite: chain fold: the chain is stale")
	// ErrChainFoldBusy refuses a second fold on the same store.
	ErrChainFoldBusy = errors.New("store_sqlite: chain fold: another fold is running")
	// ErrChainFoldYielded reports a step rolled back for an edit; step again.
	ErrChainFoldYielded = errors.New("store_sqlite: chain fold: the step gave way to an edit")
	// ErrChainFoldWALMark reports a step not started over the log's mark;
	// step again once the log is down.
	ErrChainFoldWALMark = errors.New("store_sqlite: chain fold: the log is over its mark")
)
