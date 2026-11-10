package store_sqlite

import (
	"context"

	"github.com/zzet/gortex/internal/graph"
)

// PayloadBuildFlightHandle borrows backend-owned payload build ownership.
type PayloadBuildFlightHandle interface {
	Wait(context.Context) error
	Complete(error)
}

// ChainFoldHandle drives one backend-owned chain fold.
type ChainFoldHandle interface {
	Step(context.Context) (bool, error)
	Release(context.Context) error
	Counts() (GenerationCopyCounts, int, int)
	InterruptionCounts() (int, int)
}

// PreparedPayloadPublication publishes one backend-prepared generation.
type PreparedPayloadPublication interface {
	Publish(context.Context, int64) error
}

// DerivedCorrectionHandle drives one backend-owned derived correction.
type DerivedCorrectionHandle interface {
	ReplaceSourceEdges(context.Context, []string, []*graph.Edge, []*graph.Node) error
	Finish(context.Context) (DerivedCorrectionStats, error)
}
