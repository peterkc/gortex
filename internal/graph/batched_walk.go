package graph

import "context"

// KindAdjacencyBatchReader reads bounded adjacency for IDs in request order.
// A positive limit caps emitted edges across all IDs. Every returned Total
// describes the complete adjacency before kind filtering and capping.
type KindAdjacencyBatchReader interface {
	ReadKindAdjacencyBatch(ctx context.Context, ids []string, direction Direction, kinds []EdgeKind, limit int) (map[string]KindAdjacency, bool, error)
}

const DirectionBoth Direction = 2

type WalkCap string

const (
	WalkNodeCap WalkCap = "node"
	WalkHopCap  WalkCap = "hop"
)

// WalkResult contains the discovery-ordered hops at each depth. An empty
// StoppedBy means no node or hop limit cut the walk short.
type WalkResult struct {
	Levels    [][]FrontierHop
	StoppedBy WalkCap
}

type Walker interface {
	Walk(ctx context.Context, seeds []string, direction Direction, kinds []EdgeKind, maxDepth, nodeCap, hopCap int) (WalkResult, error)
}
