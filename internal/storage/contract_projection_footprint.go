package storage

import (
	"github.com/zzet/gortex/internal/graph"
)

// ContractProjectionDeclaration is physical potential membership, including
// non-constants which count toward a name query's limit. It contains no value
// equality proof: touching its ID can still require fresh full-node reads.
type ContractProjectionDeclaration struct {
	ID, Name, Kind, FilePath, RepoPrefix string
}

type ContractProjectionEdge struct {
	From, To, Kind, FilePath string
	Line                     int
}

type ContractProjectionConstant struct {
	ID, RepoPrefix, FilePath string
}

// ContractProjectionFootprint owns a bounded physical sparse layer frontier.
// It is NOT a selected-view certificate: callers must account for inherited
// rows revealed by removed masks, census/source policy, and current debt. A
// successful read says nothing about equality of full node/edge/value answers.
// No store, reader, transaction, source or lease survives in this result.
type ContractProjectionFootprint struct {
	Cursor        ContractProjectionCursor
	Declarations  []ContractProjectionDeclaration
	Edges         []ContractProjectionEdge
	Constants     []ContractProjectionConstant
	Bindings      []graph.SemanticBindingSite
	Files         []FileMask
	Identities    []NodeIdentityMask
	EdgeSources   []EdgeSourceMask
	Producers     []ProducerCompleteness
	RetainedBytes int64
}
