package storage

// DedicatedBaseIdentity is the semantic identity of a complete committed view.
// Commit provenance is deliberately not part of tree-equivalent payload reuse.
type DedicatedBaseIdentity struct {
	TreeOID, ConfigHash, ExtractorVersions, ResolverVersion string
	// DependencyRevision identifies derived dependency inputs, not parent parse
	// policy. Empty preserves legacy/unproven output identity only.
	DependencyRevision string
}

type DedicatedBaseOwner struct {
	CheckoutID, Incarnation string
}

type DedicatedBaseAuthority struct {
	GraphID              string
	Owner                DedicatedBaseOwner
	RepoPrefix, FamilyID string
	// GenerationFloor is an exclusive creation high-water mark. Immutable
	// generations do not otherwise persist checkout incarnation provenance.
	GenerationFloor int64
	Epoch           int64
	Token           string
}

type DedicatedBaseDesire struct {
	Authority DedicatedBaseAuthority
	Epoch     int64
	Identity  DedicatedBaseIdentity
}

type AcquireDedicatedBaseAuthorityRequest struct {
	GraphID       string
	Owner         DedicatedBaseOwner
	ExpectedEpoch int64
	ExpectedToken string
	Token         string
}

type RecordDedicatedBaseDesireRequest struct {
	Authority            DedicatedBaseAuthority
	ExpectedDesiredEpoch int64
	Identity             DedicatedBaseIdentity
}

type ClaimDedicatedBaseBuildRequest struct {
	// ExistingGenerationID selects validation-only mode when positive: the
	// same current generation and attempt must exist in a live attempt state.
	// This mode never binds, allocates or writes; zero preserves normal claims.
	ExistingGenerationID                               int64
	Desire                                             DedicatedBaseDesire
	ExpectedActiveGenerationID                         int64
	AttemptToken                                       string
	BaseGenerationID                                   int64
	LayerID, LowerViewFingerprint, ProvenanceCommitOID string
	CreatedAt                                          int64
}

type DedicatedBaseBuildClaim struct {
	Desire                                                     DedicatedBaseDesire
	AttemptToken                                               string
	GenerationID, BaseGenerationID, ExpectedActiveGenerationID int64
	LayerID, LowerViewFingerprint                              string
	// Status is allocated, building, or ready. An adopted claim returns ready
	// with AlreadyAdopted=true; its original attempt is preserved without writes.
	Status         string
	AlreadyAdopted bool
}

type AdoptDedicatedBaseGenerationRequest struct{ Claim DedicatedBaseBuildClaim }

// DedicatedBaseAdoption is what one adoption installed.
//
// PreviousGenerationID is the active pointer the adoption replaced, so a caller
// can tell an advance (previous != GenerationID) from a replay of the pointer
// that was already installed. TreeOID and CommitOID are the committed point the
// adopted generation represents, read from the generation row rather than from
// the request: a claim that resolved to an already-ready candidate names the
// tree that candidate was built for, which is the tree the base actually holds.
// HeadAdvanced reports whether the owner checkout's head columns moved with it;
// false means a later observation had already moved them past this base, which
// is not a failure (see AdoptDedicatedBaseGeneration).
//
// PreviousSuperseded reports that the head this adoption replaced was labelled
// superseded in the same transaction. It is false whenever there was nothing to
// label — a first adoption, a replay, a previous head this adoption still
// descends from, or one already discarded by an earlier pass.
//
// AdoptedRestored reports the mirror write: the generation this adoption
// installed was itself carrying a superseded label from an earlier replacement
// and was cleared back to ready, because a graph's live head must not describe
// itself as replaced. It is false for the ordinary adoption of a generation
// that was already ready.
//
// A replay — AlreadyAdopted, the pointer already installed — reports the
// pointers alone. It moved nothing, so it reads nothing: the idle
// observe-claim-adopt cycle a warm daemon runs stays a cycle that performs no
// catalog work beyond the guards it has to check.
type DedicatedBaseAdoption struct {
	GenerationID         int64
	PreviousGenerationID int64
	TreeOID              string
	CommitOID            string
	HeadAdvanced         bool
	AlreadyAdopted       bool
	PreviousSuperseded   bool
	AdoptedRestored      bool
}

type FailDedicatedBaseBuildRequest struct {
	Claim DedicatedBaseBuildClaim
	Error string
}

type DedicatedBasePublication struct {
	Desire              DedicatedBaseDesire
	Claim               DedicatedBaseBuildClaim
	AttemptState, Error string
}

// DedicatedBaseAdoptionEvent is one adopted committed base, announced to the
// observers registered on the store it was adopted in.
//
// It exists because the two halves of a base advance are owned by different
// layers and share nothing but the database. The publisher that adopts holds no
// coordinator registry; the checkout lifecycle that runs the dependents' build
// loops never sees a publication. Without an announcement a dependent learns
// that its base moved only when its own poll comes round, which is the latency
// this event removes — it carries no payload and no authority, only the fact
// that the pointer moved and what it moved to.
type DedicatedBaseAdoptionEvent struct {
	GraphID    string
	FamilyID   string
	RepoPrefix string
	Owner      DedicatedBaseOwner
	Adoption   DedicatedBaseAdoption
}

// Advanced reports an event that moved the active pointer, as opposed to a
// replay of one that was already installed. A replay changes nothing a
// dependent could observe, so it is not a reason to wake one.
func (e DedicatedBaseAdoptionEvent) Advanced() bool {
	return e.Adoption.GenerationID > 0 && !e.Adoption.AlreadyAdopted &&
		e.Adoption.PreviousGenerationID != e.Adoption.GenerationID
}
