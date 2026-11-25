package storage

// OwnershipMode is what a generation claims about a masked path or source. The
// two vocabularies below differ: a file may be replaced, deleted or declared
// read-only context, while an edge source may only be replaced.
type OwnershipMode string

// ProducerState is how complete one producer's contribution to a generation is.
type ProducerState string

// FileMask is one file-level ownership claim.
type FileMask struct {
	RepoPrefix string
	FilePath   string
	Mode       OwnershipMode
}

// EdgeSourceMask is one source node whose outgoing edge set the generation
// replaces without owning the node's file.
type EdgeSourceMask struct {
	SourceID string
	Mode     OwnershipMode
}

// ProducerCompleteness is one producer's contribution state for a generation.
type ProducerCompleteness struct {
	Producer string
	State    ProducerState
	Reason   string
}

const (
	// OwnershipReplace claims the generation's own payload supersedes the
	// layer below, including when that payload is empty.
	OwnershipReplace OwnershipMode = "replace"
	// OwnershipDelete claims the path is gone and nothing below shows through.
	OwnershipDelete OwnershipMode = "delete"
	// OwnershipContext claims NOTHING. It records that the generation needed
	// the path to resolve its own change set and deliberately carries no
	// payload of its own for it, so the layer below keeps answering for the
	// file whole — identity, adjacency, derived outputs and all.
	//
	// It is the third mode rather than the absence of a mask because the two
	// statements are different: an unmentioned path is one nothing was ever
	// said about, while a context path is one a generation read, decided was
	// unchanged by its own change, and declined to claim. The difference is
	// what makes "a generation claims exactly its change set" checkable at
	// publish instead of being an agreement between two lists — see the
	// context arm of ValidateGenerationMasks, which refuses the mask over any
	// payload at all.
	OwnershipContext OwnershipMode = "context"
)

const (
	// ProducerStateComplete means the producer finished and its payload is whole.
	ProducerStateComplete ProducerState = "complete"
	// ProducerStateIncomplete means the producer finished but its payload is
	// partial, so an absence it leaves behind is not a fact.
	ProducerStateIncomplete ProducerState = "incomplete"
	// ProducerStateBuilding means the producer is still running.
	ProducerStateBuilding ProducerState = "building"
	// ProducerStateUnavailable means the producer could not run at all.
	ProducerStateUnavailable ProducerState = "unavailable"
	// ProducerStateDisabledByConfig means the producer was switched off, so its
	// absence is intended rather than a failure.
	ProducerStateDisabledByConfig ProducerState = "disabled_by_config"
)
