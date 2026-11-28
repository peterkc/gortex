package contracts

import (
	"context"
	"errors"

	"github.com/zzet/gortex/internal/graph"
)

// ReadScope is an intersection of exact graph identities. An empty list is
// unrestricted in that dimension; []string{""} selects the standalone repo.
// Scope names every record the answer depends on, including expanded cohorts.
type ReadScope struct {
	RepoPrefixes []string
	FilePaths    []string
	Types        []ContractType
	ContractIDs  []string
}

var ErrContractCoverageIncomplete = errors.New("contract coverage is incomplete in the requested scope")

// SnapshotReader is the optional checked capability of a selected rebuild
// snapshot. It never falls back to an attachment or a different reader.
type SnapshotReader interface {
	// ContractCoverageContext reports partial coverage for a positive answer.
	// Set requireComplete for absence and count answers; a matching omission
	// then returns ErrContractCoverageIncomplete, not a usable answer.
	ContractCoverageContext(ctx context.Context, scope ReadScope, requireComplete bool) (partial bool, err error)
	ContractShapesContext(ctx context.Context, ids []string) (map[string]*Shape, error)
	FullNodesByIDsContext(ctx context.Context, ids []string) (map[string]*graph.Node, error)
}
