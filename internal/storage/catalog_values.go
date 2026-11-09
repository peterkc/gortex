package storage

// CheckoutObservationVerdict is what an observation write did with the request
// it was given. It is the diagnostic the error cannot carry: a refused
// observation is not a failed write, and an accepted one that had to adopt a
// stepped-back clock is not an ordinary one.
type CheckoutObservationVerdict string

const (
	// CheckoutObservationApplied: this observation was the newest the row had
	// seen, and every column it stated landed.
	CheckoutObservationApplied CheckoutObservationVerdict = "applied"
	// CheckoutObservationFenced: a newer observation holds the row, so nothing
	// was written. The row is intact and the caller's pass is simply not the
	// one that counts.
	CheckoutObservationFenced CheckoutObservationVerdict = "fenced"
	// CheckoutObservationRebased: passes kept arriving behind the row's clock,
	// which is a host clock that moved and not an observer that is late, so the
	// fence adopted the new clock domain rather than refusing forever. The
	// write landed.
	CheckoutObservationRebased CheckoutObservationVerdict = "rebased"
)

// CheckoutObservationReport is the diagnostic half of an observation write: it
// says what the write actually recorded, which the error alone cannot, because
// a refused observation is not a refused row.
type CheckoutObservationReport struct {
	// Verdict says whether the write landed, and why not when it did not.
	Verdict CheckoutObservationVerdict
	// Position is the monotonic observation position the row stands at after
	// the call: this write's, when it landed, and the newer observation's when
	// it was refused. It never moves backwards, not even when the host clock
	// does.
	Position int64
	// FencedBy and FencingObservation name the observation that refused this
	// one — its position and the pass clock it was accepted at. Both are 0
	// when nothing refused anything.
	FencedBy           int64
	FencingObservation int64
	// HeadRef, HeadCommit and HeadTree are the head facts the row holds after
	// the call, whether this request stated them or the fence kept them.
	HeadRef, HeadCommit, HeadTree string
	// HeadRefused says the head axis alone was held back: the rest of the
	// request landed, but the head the row carries was published by an
	// adoption this sample is behind, so the sample's head was not written.
	// The row keeps the published head and the refusal is traced durably in
	// last_error.
	HeadRefused bool
	// HeadEpoch is the clock at which an adoption published the head the row
	// holds, and 0 when no adoption published it — the sequence a sample has
	// to reference before it may move that head.
	HeadEpoch int64
}

// Fenced reports the refusal, which is the one verdict a caller has to branch
// on: its sample was superseded and nothing it stated was written.
func (r CheckoutObservationReport) Fenced() bool {
	return r.Verdict == CheckoutObservationFenced
}

// RefViewBuildKey names one coalescing slot: the four columns the partial
// unique index on the in-flight builds is keyed by.
type RefViewBuildKey struct {
	RefViewID        string
	DesiredTree      string
	BaseGenerationID int64
	BuildFingerprint string
}

// RepositoryCleanupIdentity is the durable identity needed to restore a
// process-local owner fence. It deliberately includes generation-zero payload.
type RepositoryCleanupIdentity struct {
	GraphID     string
	CheckoutID  string
	Incarnation string
	RepoPrefix  string
	FamilyID    string
	RootPath    string
}

// RetirementCandidateShape is what the deferred retirement sweep orders its
// candidates by: whether a generation is already fenced and part-swept, how
// large its payload was at publication, and whether any generation is still
// built on it (which refuses its retirement as "based" until that child's row
// is gone).
type RetirementCandidateShape struct {
	State        ViewGenerationState
	StorageBytes int64
	HasChildren  bool
}

// BeginIntentTransitionRequest is the durable admission record for a mode
// change. When TrackingIntent is present, the reason the checkout is becoming
// dedicated and the transition that will do it are committed together.
type BeginIntentTransitionRequest struct {
	Transition     IntentTransition
	Incarnation    string
	TrackingIntent *TrackingIntent
}

// RouteSlot names one of the two generation pointers a checkout's route
// carries: the generation built from the checkout's commit, and the one built
// from its uncommitted working tree.
type RouteSlot string

const (
	// RouteSlotCommit is the commit_generation_id pointer.
	RouteSlotCommit RouteSlot = "commit"
	// RouteSlotDirty is the dirty_generation_id pointer.
	RouteSlotDirty RouteSlot = "dirty"
)

// CleanupPhase is how far a journal entry has progressed.
type CleanupPhase string

const (
	// CleanupPhasePending is recorded but not started.
	CleanupPhasePending CleanupPhase = "pending"
	// CleanupPhaseGrace is waiting out its grace deadline.
	CleanupPhaseGrace CleanupPhase = "grace"
	// CleanupPhaseDeleting is actively removing its targets.
	CleanupPhaseDeleting CleanupPhase = "deleting"
	// CleanupPhaseDone finished; the entry is kept as evidence.
	CleanupPhaseDone CleanupPhase = "done"
	// CleanupPhaseFailed stopped part-way and needs another attempt.
	CleanupPhaseFailed CleanupPhase = "failed"
)

// RepositoryFamily is one physical repository object store — the identity a
// primary checkout and all of its linked worktrees share.
type RepositoryFamily struct {
	FamilyID          string
	CommonDirIdentity string
	DisplayRemote     string
	State             string
	PrimaryEpoch      int64
	CreatedAt         int64 // unix seconds
	LastSeen          int64 // unix seconds
}

// Checkout is one working copy the daemon knows about.
type Checkout struct {
	CheckoutID  string
	Incarnation string
	FamilyID    string
	RootPath    string
	GitDir      string
	AdminName   string

	State         CheckoutState
	DesiredMode   CheckoutMode
	EffectiveMode CheckoutMode

	Locked   bool
	Prunable bool

	HeadRef    string
	HeadCommit string
	HeadTree   string

	LastAccessible       int64 // unix seconds
	UnavailableSince     int64 // unix seconds
	AvailabilityDeadline int64 // unix seconds
	RemovalDetectedAt    int64 // unix seconds
	RemovalDeadline      int64 // unix seconds
	RemovalEvidence      string

	// ActiveIntentTransitionID is empty when no transition is in flight; it is
	// stored as NULL in that case.
	ActiveIntentTransitionID string

	LastSeen  int64 // unix seconds
	LastError string
}

// TrackingIntent is one reason the daemon tracks a checkout.
type TrackingIntent struct {
	IntentID      string
	CheckoutID    string
	SourceKind    IntentSourceKind
	SourceLocator string
	Active        bool
	CreatedAt     int64 // unix seconds
	RevokedAt     int64 // unix seconds
	LastError     string
}

// IntentTransition is the single in-flight mode change for a checkout. The
// prior mode and state fields may be empty when the transition did not need to
// capture them; when set they must name a valid vocabulary value.
type IntentTransition struct {
	TransitionID       string
	CheckoutID         string
	Cause              string
	PriorDesiredMode   CheckoutMode
	PriorEffectiveMode CheckoutMode
	RequestedMode      CheckoutMode
	PriorCheckoutState CheckoutState
	SourceSnapshotHash string
	State              IntentTransitionState
	CreatedAt          int64 // unix seconds
	LastProgress       int64 // unix seconds
	LastError          string
}

// DedicatedGraph is a graph built for one checkout. ActiveGenerationID is 0
// when no generation is published yet.
type DedicatedGraph struct {
	GraphID            string
	OwnerCheckoutID    string
	RepoPrefix         string
	FamilyID           string
	IsPrimaryBase      bool
	ActiveGenerationID int64
	State              string
}

// ViewGeneration is one build of a view. GenerationID is assigned by the
// store; BaseGenerationID is 0 when the generation has no lower layer.
type ViewGeneration struct {
	GenerationID         int64
	OwnerKind            string
	GraphID              string
	LayerID              string
	CheckoutID           string
	GenerationKind       string
	BaseGenerationID     int64
	LowerViewFingerprint string
	TreeOID              string
	ProvenanceCommitOID  string
	ConfigHash           string
	ExtractorVersions    string
	ResolverVersion      string
	// Empty denotes legacy/unproven derived dependency provenance.
	DependencyRevision string
	State              ViewGenerationState
	CoveredFiles       int64
	AffectedFiles      int64
	StorageBytes       int64
	Completeness       string
	CreatedAt          int64 // unix seconds
	PublishedAt        int64 // unix seconds
	LastSelected       int64 // unix seconds
	Error              string
}

// ViewGenerationFilter narrows a ListViewGenerations scan. Every field is
// optional and the ones that are set are ANDed, so the zero filter enumerates
// the newest generations in the table.
//
// It exists because a generation is otherwise only reachable through something
// that points at it — a route, a ref view, a base pointer, or an id an owner
// happens to still be holding. A process that dies between superseding a
// generation and retiring it drops the last of those, and without an
// enumeration the payload is unreachable for the life of the database.
type ViewGenerationFilter struct {
	// States restricts the scan to these lifecycle states. Empty accepts all
	// of them.
	States []ViewGenerationState
	// CheckoutID restricts the scan to the generations built for one
	// checkout. Empty accepts all of them, including the generations that
	// name no checkout at all.
	CheckoutID string
	// GraphID restricts the scan to one graph. Empty accepts all of them.
	GraphID string
	// MissingGraph accepts only generations whose non-empty graph_id no
	// longer names a dedicated graph. This is the durable retirement backlog
	// left when graph deletion wins a race with process shutdown.
	MissingGraph bool
	// OwnerKind restricts the scan to one owner vocabulary. Empty accepts all
	// of them.
	OwnerKind string
	// BeforeGenerationID restricts the scan to generations older than this
	// exclusive cursor. Zero starts at the newest generation.
	BeforeGenerationID int64
	// Limit bounds the rows one call returns. 0 — and anything above the cap —
	// takes maxViewGenerationListing.
	Limit int
}

// ViewLayer names the git state a generation is built over.
type ViewLayer struct {
	LayerID      string
	Kind         string
	GraphID      string
	CheckoutID   string
	TargetRef    string
	TargetCommit string
	TargetTree   string
}

// CheckoutRoute is where a checkout's queries currently land. The generation
// pointers are 0 when unset.
type CheckoutRoute struct {
	CheckoutID         string
	GraphID            string
	CommitGenerationID int64
	DirtyGenerationID  int64
	RouteEpoch         int64
	State              RouteState
}

// RefView is a named view of one graph at a selector.
type RefView struct {
	RefViewID     string
	GraphID       string
	SelectorKind  string
	SelectorValue string

	DesiredRef    string
	DesiredCommit string
	DesiredTree   string

	ActiveGenerationID int64
	ActiveRef          string
	ActiveCommit       string
	ActiveTree         string

	EnrichmentProfile       string
	DesiredBuildFingerprint string
	ActiveBuildFingerprint  string

	RouteEpoch   int64
	State        RefViewState
	ExactView    bool
	LastResolved int64 // unix seconds
	LastSelected int64 // unix seconds
	LastError    string
}

// RefViewBuild is one build attempt for a ref view.
type RefViewBuild struct {
	BuildID            string
	RefViewID          string
	DesiredRef         string
	DesiredCommit      string
	DesiredTree        string
	BaseGenerationID   int64
	EnrichmentProfile  string
	BuildFingerprint   string
	GenerationID       int64
	CapturedRouteEpoch int64
	State              ViewGenerationState
	BuildToken         string
	CreatedAt          int64 // unix seconds
	LastProgress       int64 // unix seconds
	Error              string
}

// UpdateRefViewDesireRequest records what a selection just resolved the view's
// selector to.
//
// The route epoch is not a guard here and not a parameter: the desire write
// bumps it only when the tree or the fingerprint actually changed, so two
// selections that resolve to the same state do not invalidate each other's
// in-flight builds, while a selection that re-targets the view does exactly
// that.
type UpdateRefViewDesireRequest struct {
	RefViewID string

	DesiredRef              string
	DesiredCommit           string
	DesiredTree             string
	DesiredBuildFingerprint string

	State        RefViewState
	LastResolved int64 // unix seconds
	LastSelected int64 // unix seconds
}

// AdoptRefViewGenerationRequest points a ref view at the generation a finished
// build produced, and closes the attempt that produced it.
//
// The three expectations are the compare-and-set: the route epoch the build
// captured when it was claimed, plus the tree and fingerprint it was built
// for. A view that was re-targeted while the build ran matches none of them,
// so the adoption changes nothing and reports ErrCatalogStaleGuard rather than
// serving a payload for a state the selector has left.
//
// BuildID and BuildToken are the fourth expectation, and the reason the two
// writes are one: the claim is the right to publish, so a build whose slot was
// reclaimed while it ran may not adopt behind its successor, and a build the
// view refuses may not be recorded as having published.
type AdoptRefViewGenerationRequest struct {
	RefViewID          string
	ExpectedRouteEpoch int64

	ExpectedDesiredTree             string
	ExpectedDesiredBuildFingerprint string

	// BuildID and BuildToken name the claim the adoption is made under. An
	// empty BuildID adopts without closing an attempt.
	BuildID    string
	BuildToken string
	// LastProgress is the clock stamped on the closed attempt.
	LastProgress int64 // unix seconds

	GenerationID           int64
	ActiveRef              string
	ActiveCommit           string
	ActiveTree             string
	ActiveBuildFingerprint string

	ExactView    bool
	LastResolved int64 // unix seconds
	LastSelected int64 // unix seconds
}

// TouchRefViewSelectionRequest re-stamps what a selection observed without
// changing what the view serves: the ref and commit the selector resolves to
// now, and the selection clock. It is the write behind a ref that moved to a
// different commit carrying the same tree — the payload is unchanged, so only
// the metadata moves.
type TouchRefViewSelectionRequest struct {
	RefViewID          string
	ExpectedRouteEpoch int64

	ActiveRef    string
	ActiveCommit string
	LastResolved int64 // unix seconds
	LastSelected int64 // unix seconds
}

// FailRefViewRequest records why a selection could not be served. It never
// touches the active pointer: a view whose newest build failed keeps serving
// what it was serving, labelled inexact by whoever reads it.
type FailRefViewRequest struct {
	RefViewID          string
	ExpectedRouteEpoch int64

	LastError    string
	LastResolved int64 // unix seconds
}

// CompleteRefViewBuildRequest ends one build attempt. BuildToken is the proof
// that the caller still owns the attempt it started, and the building state is
// the other half of the guard, so an attempt can only be completed once.
type CompleteRefViewBuildRequest struct {
	BuildID    string
	BuildToken string

	State        ViewGenerationState
	GenerationID int64
	LastProgress int64 // unix seconds
	Error        string
}

// CleanupEntry is one unit of deferred deletion work.
type CleanupEntry struct {
	CleanupID       string
	OpaqueTargetIDs string
	Reason          string
	Phase           CleanupPhase
	GraceDeadline   int64 // unix seconds
	PrimaryEpoch    int64
	LastProgress    int64 // unix seconds
	LastError       string
}

// UpdateCheckoutStateRequest is a guarded checkout-state write. Incarnation is
// the expectation, not a new value: a request naming the previous incarnation
// of a recreated path changes nothing and reports ErrCatalogStaleGuard.
type UpdateCheckoutStateRequest struct {
	CheckoutID    string
	Incarnation   string
	State         CheckoutState
	DesiredMode   CheckoutMode
	EffectiveMode CheckoutMode
	LastSeen      int64 // unix seconds
	LastError     string
}

// UpdateCheckoutObservationRequest is everything one reconciliation pass may
// change about a checkout it just looked at: the state axis, the availability
// clock, the removal clock, and the git / filesystem facts it observed.
//
// Incarnation is the expectation, not a new value — the same guard
// UpdateCheckoutStateRequest carries. The identity columns are absent on
// purpose: an observation describes the row it found, it never re-keys it.
// The mode columns are absent for the same kind of reason: how a checkout is
// served is the mode transition's business, and an observer that carried the
// modes along would be able to revert one it never meant to touch.
type UpdateCheckoutObservationRequest struct {
	CheckoutID  string
	Incarnation string

	State CheckoutState

	RootPath string
	GitDir   string
	Locked   bool
	Prunable bool

	HeadRef    string
	HeadCommit string
	HeadTree   string

	LastAccessible       int64 // unix seconds
	UnavailableSince     int64 // unix seconds
	AvailabilityDeadline int64 // unix seconds
	RemovalDetectedAt    int64 // unix seconds
	RemovalDeadline      int64 // unix seconds
	RemovalEvidence      string

	LastSeen  int64 // unix seconds
	LastError string
}

// FlipCheckoutRouteRequest repoints one checkout's route. ExpectedRouteEpoch
// is the compare-and-set token; a successful flip stores epoch+1. Generation
// pointers of 0 clear the corresponding column.
type FlipCheckoutRouteRequest struct {
	CheckoutID         string
	ExpectedRouteEpoch int64
	GraphID            string
	CommitGenerationID int64
	DirtyGenerationID  int64
	State              RouteState
}

// FlipCheckoutRouteSlotRequest repoints one slot of a checkout's route.
// ExpectedRouteEpoch is the compare-and-set token; a successful flip stores
// epoch+1. A GenerationID of 0 clears the slot. The other slot is not named by
// the statement and keeps whatever it held.
type FlipCheckoutRouteSlotRequest struct {
	CheckoutID         string
	Slot               RouteSlot
	GenerationID       int64
	ExpectedRouteEpoch int64
	State              RouteState
}

// SetPrimaryDedicatedGraphRequest promotes one graph to its family's primary
// base. ExpectedPrimaryEpoch is the compare-and-set token on the family row; a
// successful promotion stores epoch+1.
type SetPrimaryDedicatedGraphRequest struct {
	FamilyID             string
	GraphID              string
	ExpectedPrimaryEpoch int64
	LastSeen             int64 // unix seconds
}

// UntrackAuthorizationPlan names the durable operation an explicit untrack is
// authorizing. Cleanup plans start a cleanup-journal entry; demotion starts an
// intent transition that is completed only after its replacement route exists.
type UntrackAuthorizationPlan string

const (
	UntrackAuthorizationForget         UntrackAuthorizationPlan = "forget"
	UntrackAuthorizationPrimaryClosure UntrackAuthorizationPlan = "primary_closure"
	UntrackAuthorizationDemote         UntrackAuthorizationPlan = "demote"
)

// AuthorizeUntrackRequest is the complete compare-and-set for an explicit
// untrack. Every identity named here came from the preview. The catalog checks
// them and the active intent set in one write transaction, then records the
// durable work item in that same transaction before intent revocation becomes
// visible.
type AuthorizeUntrackRequest struct {
	Plan UntrackAuthorizationPlan

	CheckoutID  string
	Incarnation string
	FamilyID    string

	OwnedGraphID         string
	PrimaryGraphID       string
	ExpectedPrimaryEpoch int64
	RequiredPrimaryState string

	RevokedAt      int64
	RevocableKinds []IntentSourceKind

	Cleanup    *CleanupEntry
	Transition *IntentTransition
}

// AuthorizeUntrackResult reports the atomic preflight and the durable work item
// that now owns cleanup. Existing carries a journal/transition written by an
// earlier attempt; callers resume it rather than minting competing work.
type AuthorizeUntrackResult struct {
	Revoked    []TrackingIntent
	Blocked    []TrackingIntent
	Cleanup    *CleanupEntry
	Transition *IntentTransition
	Existing   bool
}

// CommitAuthorizedDemotionRequest atomically makes a prepared automatic route
// authoritative. When a dedicated graph has to be retired, Cleanup is inserted
// in the same transaction as the mode flip, so a crash cannot leave an
// automatic checkout with an unjournalled private graph.
type CommitAuthorizedDemotionRequest struct {
	CheckoutID   string
	Incarnation  string
	FamilyID     string
	TransitionID string

	OwnedGraphID         string
	PrimaryGraphID       string
	ExpectedPrimaryEpoch int64
	RequiredPrimaryState string

	State    CheckoutState
	LastSeen int64
	Cleanup  *CleanupEntry
}
