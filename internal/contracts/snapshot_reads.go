package contracts

import "errors"

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
