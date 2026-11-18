package storetest

import (
	"context"
	"errors"
	"slices"
	"testing"
	"time"

	"github.com/zzet/gortex/internal/graph"
)

// The storage contract suite: lifecycle and catalog cases K1 to K14 of
// design-c.md 1.8, written against the neutral operations of 1.2 and run
// unchanged on every backend in Backends.
//
//	storetest_test.TestStorageContract    each name in Backends
//	        | test adapter (storetest.Backend)
//	        v
//	RunStorageContract
//	        | name in Backends?      no -> fail
//	        | Declaration vs table   mismatch -> fail
//	        v
//	K cases over fixture F, each on a fresh Instance in its own directory
//
// A case never skips because a backend lacks a capability it requires. A
// backend states what it lacks in its Declaration; the suite's own table in
// lifecycle_expectations.go decides which absences pass (D14).

// Backends is the suite's backend list. Each lifecycle case, the expectation
// table and the K12 open-path cases run on these names. A backend joins the
// list only when no capability the default profile requires is missing, and
// adding its name here is the only suite change that admits it.
func Backends() []string { return []string{"sqlite"} }

// caseDeadline bounds one K case, so a wedged backend fails the case instead
// of hanging until the package timeout.
const caseDeadline = 2 * time.Minute

// Backend is one backend's entry into the suite. The external test package
// holds one per listed name.
type Backend struct {
	// Name is the backend's name in Backends and in the expectation table.
	Name string
	// Open opens a real instance over dir, a directory the case owns that is
	// empty or holds files an earlier instance left. A nil hook opens the
	// instance exactly as production does. Open registers the instance's
	// Close with t.Cleanup. Before opening the same files again, the case
	// must end the earlier instance with Crash or Close. K11 deliberately
	// keeps a live owner to check that TryOpen refuses an alias.
	Open func(t *testing.T, dir string, hook FaultHook) Instance
	// TryOpen is Open that returns the backend's refusal instead of failing
	// the case.
	TryOpen func(t *testing.T, dir string, hook FaultHook) (Instance, error)
	// Damage changes the files a closed instance left in dir as kind says,
	// in the backend's own file layout. version names the version a payload
	// damage applies to.
	Damage func(t *testing.T, dir string, kind Damage, version Version)
	// DamageSealedFile changes exactly one declared sealed resource in a closed
	// store, never its catalog selection or other resources. A declared kind
	// must be present; any error, including ErrUnsupported, fails the case.
	DamageSealedFile func(ctx context.Context, dir string, version Version, kind string, damage Damage) error
}

// Damage names a change to a backend's files that a case makes to check the
// refusal (K9, K11).
type Damage string

const (
	// DamageTooNewFormat marks the files as written by a newer format.
	DamageTooNewFormat Damage = "too_new_format"
	// DamageCorrupt overwrites the store's header bytes.
	DamageCorrupt Damage = "corrupt"
	// DamageSymlinkEscape moves the store's main file outside dir and leaves
	// a symlink to it.
	DamageSymlinkEscape Damage = "symlink_escape"
	// DamageMissingPayload removes the payload bytes of one version and
	// leaves its catalog record.
	DamageMissingPayload Damage = "missing_payload"
	// Sealed damage leaves the seal's digest unchanged, except wrong recipe
	// and version: those rewrite the envelope and digest consistently, so
	// they test identity validation rather than only checksum validation.
	DamageSealedTorn      Damage = "sealed_torn"
	DamageSealedReordered Damage = "sealed_reordered"
	DamageSealedDigest    Damage = "sealed_digest_mismatch"
	DamageSealedPartial   Damage = "sealed_partial"
	DamageSealedOversized Damage = "sealed_oversized"
	DamageSealedRecipe    Damage = "sealed_wrong_recipe"
	DamageSealedVersion   Damage = "sealed_wrong_version"
	// DamageSealedSymlinkEscape moves the sealed resource's own valid bytes
	// outside dir and leaves a symlink to them, so only the escape can make
	// the backend refuse.
	DamageSealedSymlinkEscape Damage = "sealed_symlink_escape"
)

// FaultHook is called at each fault point the backend declares. A non-nil
// result stops the running operation at that point (FR-6).
type FaultHook func(point string) error

// Version identifies one repository version. It is the backend's own
// locator: cases compare it for equality and never decode it. Empty means
// no version.
type Version string

// Checkout is one tracked working tree of a repository.
type Checkout struct {
	// Repository is the repository identity: the graph's repo prefix.
	Repository string
	// ID names the checkout; two checkouts of one repository differ here.
	ID string
	// Root is the git working tree.
	Root string
}

// Inputs is the complete input identity of a build (1.2).
type Inputs struct {
	Tree         string
	Commit       string
	Config       string
	Extractors   string
	Resolver     string
	Dependencies string
}

// BuildRequest asks for a private build of one checkout version (B).
type BuildRequest struct {
	Checkout string
	Inputs   Inputs
	// Parent, when set, makes this an incremental build over that published
	// version: it carries only changed contributions and removed files, and
	// serves the parent's other content through composition.
	Parent Version
	// Operation is the caller's operation ID. Repeating an operation with the
	// same inputs finds the same attempt; another operation never takes over
	// a live owner's attempt (1.2).
	Operation string
}

// Payload is one contribution of graph facts to a private build (G on
// private work). Producers lists the producers that finished with this
// contribution, including a producer whose output is empty.
type Payload struct {
	Nodes     []*graph.Node
	Edges     []*graph.Edge
	Files     []graph.FileMetaRow
	Symbols   []graph.SymbolFTSItem
	Vectors   []graph.VectorItem
	Producers []string
	// RemovedFiles lists files an incremental build removes from its parent.
	RemovedFiles []string
}

// Publication is what a checkout serves now: the selected version and the
// guard a publish must present to replace it. Guard is opaque.
type Publication struct {
	Version Version
	Guard   string
}

// Provenance is what a served version was built from.
type Provenance struct {
	Inputs Inputs
	// Producers is every producer recorded complete, sorted.
	Producers []string
}

// State is a stored version's lifecycle state (1.4). A version T removed is
// absent from the inventory; it has no state.
type State string

const (
	// StateBuilding is private work that still accepts writes.
	StateBuilding State = "building"
	// StateIncomplete is an attempt that failed or was abandoned before S. It
	// can never be sealed or published.
	StateIncomplete State = "incomplete"
	// StateSealed is a sealed version, published or not.
	StateSealed State = "sealed"
	// StateRetiring is a version behind a retirement fence: nothing new may
	// reference it, and its reclaim may be partly done.
	StateRetiring State = "retiring"
)

// Stored is one version in an instance's inventory.
type Stored struct {
	Version Version
	State   State
}

// ErrStaleGuard reports a publish whose expected publication is no longer
// current. An adapter wraps its backend's own stale-guard error in it.
var ErrStaleGuard = errors.New("storetest: stale publication guard")

// ErrNotTracked reports a checkout with no route, including after untrack.
var ErrNotTracked = errors.New("storetest: checkout is not tracked")

// ErrFenced reports a new reference refused by a version's retirement fence.
var ErrFenced = errors.New("storetest: version is fenced for retirement")

// ErrUnsupported reports an optional capability the backend declares
// unsupported (D14). An adapter wraps its refusal in it.
var ErrUnsupported = errors.New("storetest: unsupported by this backend")

// Instance is one opened backend. Its operations are the neutral operations
// of design-c.md 1.2. R, recovery, is Backend.Open over files an earlier
// instance left: the new instance reconciles them before it serves. Later
// cases add the authored-data operations of K13.
type Instance interface {
	// Declaration reports the backend's own inventory, which the suite
	// compares with its expectation table (D14).
	Declaration() Declaration
	// PrepareSealedFile builds req with payload through the backend's native
	// writers and seals it, without publishing. The returned build must hold
	// a valid sealed resource of kind, or this operation refuses. It may not
	// substitute a different kind or silently omit the requested resource.
	PrepareSealedFile(ctx context.Context, kind string, req BuildRequest, payload Payload) (Build, error)
	// Track records a checkout so builds and routes can name it (C).
	Track(ctx context.Context, checkout Checkout) error
	// Begin starts a private build, or rejoins the one already building
	// with the same checkout and inputs (B). A parent behind a retirement
	// fence returns an error wrapping ErrFenced.
	Begin(ctx context.Context, req BuildRequest) (Build, error)
	// Current returns the checkout's publication; Version is empty when the
	// checkout serves nothing yet. With no route, including after Untrack, it
	// returns an error wrapping ErrNotTracked. After Close it returns an
	// error that does not wrap ErrNotTracked, and does not panic.
	Current(ctx context.Context, checkout string) (Publication, error)
	// Open pins the checkout's current publication for reading (O). A
	// checkout that serves nothing returns an error.
	Open(ctx context.Context, checkout string) (View, error)
	// Retire retires a version (T): it fences new references, reclaims the
	// version's inventory and forgets it. A referenced or pinned version
	// refuses or defers its reclaim; either way it stays readable.
	Retire(ctx context.Context, version Version) error
	// Inventory lists every version the instance stores, in any order.
	Inventory(ctx context.Context) ([]Stored, error)
	// Operation returns the version a recorded publish operation published.
	// An unknown or pruned operation ID returns an error.
	Operation(ctx context.Context, id string) (Version, error)
	// Untrack withdraws a checkout as one guarded transition (C): its route
	// goes, its versions are recorded for cleanup, and other checkouts keep
	// theirs. A stale expected publication changes nothing and returns an
	// error wrapping ErrStaleGuard. After the route goes, Current returns an
	// error wrapping ErrNotTracked.
	Untrack(ctx context.Context, checkout string, expected Publication) error
	// Link selects repository to's current version for from's cross-repository
	// edges as one guarded transition (C, 1.6).
	Link(ctx context.Context, from, to string) error
	// PutAuthored writes a user-authored record of a repository (5.2).
	PutAuthored(ctx context.Context, repository string, record AuthoredRecord) error
	// Authored lists a repository's authored records, sorted by ID.
	Authored(ctx context.Context, repository string) ([]AuthoredRecord, error)
	// Export writes the authored records and tracking choices as one set
	// (K13). A backend without the export capability wraps ErrUnsupported.
	Export(ctx context.Context) ([]byte, error)
	// Import reads a set Export wrote into this instance side by side (K13).
	// A backend without the import capability wraps ErrUnsupported.
	Import(ctx context.Context, set []byte) error
	// Settle waits until the instance reports no pending work and returns how
	// many rebuild jobs it started since it opened (K14).
	Settle(ctx context.Context) (rebuilds int, err error)
	// Crash releases ownership and every handle without sealing, publishing,
	// checkpointing or flushing private work. An adapter limited to a clean
	// Close must document that difference. Old Build handles must refuse.
	Crash() error
	// Close closes the instance; its files stay. A second Close does nothing.
	Close() error
}

// Build is the owned handle of one private build.
type Build interface {
	Version() Version
	// Store is the build's private work as the existing graph store (G).
	Store() graph.Store
	// Write adds a contribution to the private version.
	Write(ctx context.Context, payload Payload) error
	// Seal validates the complete private output and ends writes (S).
	Seal(ctx context.Context) error
	// Publish makes the sealed version the checkout's publication if the
	// checkout still serves expected (P), under operation ID operation. A
	// stale guard changes nothing and returns an error wrapping
	// ErrStaleGuard. Repeating a completed operation with the same version
	// returns its recorded result without publishing again.
	Publish(ctx context.Context, expected Publication, operation string) error
}

// View is a pinned reader of one served version (O). Release ends the pin;
// the view must not be used after it.
type View interface {
	Version() Version
	Provenance(ctx context.Context) (Provenance, error)
	Reader() graph.Reader
	Release()
}

// RunStorageContract runs every lifecycle and catalog case on backend.
// lookup returns the adapter of any listed backend, for K13's pairs.
func RunStorageContract(t *testing.T, backend Backend, lookup func(name string) (Backend, bool)) {
	t.Helper()
	listed := Backends()
	if !slices.Contains(listed, backend.Name) {
		t.Fatalf("backend %q is not in the suite's backend list %v", backend.Name, listed)
	}
	if k14IsChild() {
		k14RunChild(t, backend)
		return
	}
	contract := t.Name()
	validateKnownGaps(t, expectations(listed))
	run := func(name string, body func(*testing.T)) {
		t.Run(name, func(t *testing.T) { runContractCase(t, backend.Name, name, body) })
	}
	run("D14/DeclarationMatchesTheExpectationTable", func(t *testing.T) {
		checkDeclaration(t, backend, listed)
	})
	run("K1/ColdChainServesFixtureFAtE0", func(t *testing.T) { testK1ColdChain(t, backend) })
	run("K1/OldPinKeepsE0AndNewPinSeesE1", func(t *testing.T) { testK1UpdatePins(t, backend) })
	run("K1/SecondCheckoutStaysIndependent", func(t *testing.T) { testK1SecondCheckout(t, backend) })
	run("K1/PrivateVersionIsNotServed", func(t *testing.T) { testK1PrivateNotServed(t, backend) })
	run("K2/LoserWithStaleGuardChangesNothing", func(t *testing.T) { testK2StaleGuard(t, backend) })
	run("K2/SealedAndPublishedHandlesRefuseWrites", func(t *testing.T) { testK2EndedHandles(t, backend) })
	run("K2/RetiringHandleRefuses", func(t *testing.T) { testK2RetiringHandle(t, backend) })
	run("K2/SealRequiresBuilding", func(t *testing.T) { testK2SealRequiresBuilding(t, backend) })
	run("K3/RecoveryServesOldOrNewComplete", func(t *testing.T) { testK3InterruptedPublish(t, backend) })
	run("K3/TornAndReorderedSealedFilesRefusePublish", func(t *testing.T) { testK3DamagedSealedCandidate(t, backend) })
	run("K3/SealedDigestCheckedAtFirstMap", func(t *testing.T) { testK3SealedDigestAtFirstMap(t, backend) })
	run("K11/LocalSealedFileBoundary", func(t *testing.T) { testK11LocalSealedFiles(t, backend) })
	run("K3/AcknowledgedPublishSurvives", func(t *testing.T) { testK3AcknowledgedPublishSurvives(t, backend) })
	run("K5/OrphansAreNotServed", func(t *testing.T) { testK5OrphansNotServed(t, backend) })
	run("K5/MismatchedInputsDoNotAdopt", func(t *testing.T) { testK5MismatchedInputsNotAdopted(t, backend) })
	run("K5/ReclaimTouchesOnlyUnreferenced", func(t *testing.T) { testK5ReclaimOnlyUnreferenced(t, backend) })
	run("K6/RetireKeepsActivePublication", func(t *testing.T) { testK6RetireKeepsActivePublication(t, backend) })
	run("K6/PinDefersRetire", func(t *testing.T) { testK6PinDefersRetire(t, backend) })
	run("K7/CrashedRetireResumes", func(t *testing.T) { testK7InterruptedRetire(t, backend) })
	run("K7/CancelledRetireResumes", func(t *testing.T) { testK7CancelledRetire(t, backend) })
	run("K8/DefaultProfileCapabilitiesPresent", func(t *testing.T) { testK8RequiredCapabilities(t, backend) })
	run("K8/OptionalAccelerationMatchesReferencePath", func(t *testing.T) { testK8OptionalAccelerationMatchesReference(t, backend) })
	run("K10/IncrementalEqualsClean", func(t *testing.T) { testK10IncrementalEqualsClean(t, backend) })
	run("K10/IdenticalContributionsDeduplicate", func(t *testing.T) { testK10IdenticalContributionsDeduplicate(t, backend) })
	run("K11/TooNewFormatRefusesAndKeepsFiles", func(t *testing.T) { testK11RefusesDamagedStore(t, backend, DamageTooNewFormat) })
	run("K11/CorruptStoreRefusesAndKeepsFiles", func(t *testing.T) { testK11RefusesDamagedStore(t, backend, DamageCorrupt) })
	run("K13/AuthoredSurvivesRebuildAndRollback", func(t *testing.T) { testK13AuthoredSurvivesRebuildAndRollback(t, backend) })
	run("K13/BackendPairs", func(t *testing.T) { testK13BackendPairs(t, backend, lookup) })
	run("K13/TableRequiresCutoverCapabilities", func(t *testing.T) { testK13TableRequiresCutover(t) })
	run("K14/NoChangeRestart", func(t *testing.T) { testK14NoChangeRestart(t, backend, contract) })
	run("K14/CounterSeesSameContentAndScratchWrites", func(t *testing.T) { testK14CounterSeesWrites(t, backend, contract) })
	run("K2/PublishRequiresSealedCandidate", func(t *testing.T) { testK2PublishRequiresSeal(t, backend) })
	run("K2/ExpiredOwnerRefuses", func(t *testing.T) { testK2ExpiredOwner(t, backend) })
	run("K3/FailedWriteRefusesSealAndPublish", func(t *testing.T) { testK3FailedWriteRefusesSeal(t, backend) })
	run("K3/CancelledWriteRefusesSealAndPublish", func(t *testing.T) { testK3CancelledWriteRefusesSeal(t, backend) })
	run("K4/LostReplyReturnsRecordedResult", func(t *testing.T) { testK4LostReply(t, backend) })
	run("K5/SealedOrphanIsAdopted", func(t *testing.T) { testK5SealedOrphanAdopted(t, backend) })
	run("K5/UnsealedOrphanIsNotAdopted", func(t *testing.T) { testK5UnsealedOrphanNotAdopted(t, backend) })
	run("K5/LiveOwnerRefusesAdoption", func(t *testing.T) { testK5LiveOwnerRefusesAdoption(t, backend) })
	run("K6/RetireKeepsPreviousPublication", func(t *testing.T) { testK6RetireKeepsPreviousPublication(t, backend) })
	run("K6/SelectionRace", func(t *testing.T) { testK6SelectionRace(t, backend) })
	run("K9/MissingActivePayloadIsVisible", func(t *testing.T) { testK9MissingPayloadIsVisible(t, backend) })
	run("K9/LinkIsAGuardedTransition", func(t *testing.T) { testK9LinkIsGuarded(t, backend) })
	run("K10/ConflictingDuplicateRefusesSeal", func(t *testing.T) { testK10ConflictingDuplicateRefusesSeal(t, backend) })
	run("K11/SymlinkEscapeRefusesAndKeepsFiles", func(t *testing.T) { testK11RefusesDamagedStore(t, backend, DamageSymlinkEscape) })
	run("K11/AliasOfALiveStoreRefuses", func(t *testing.T) { testK11AliasOfLiveStoreRefuses(t, backend) })
	run("K9/UntrackIsAllOrNone", func(t *testing.T) { testK9UntrackIsAllOrNone(t, backend) })
	run("K9/StaleUntrackGuardChangesNothing", func(t *testing.T) { testK9StaleUntrackChangesNothing(t, backend) })
	run("K13/RemoveAndReAddKeepsAuthoredAndRevivesNothing", func(t *testing.T) { testK13RemoveAndReAdd(t, backend) })
}

// caseContext is the deadline-bounded context of one case.
func caseContext(t *testing.T) context.Context {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), caseDeadline)
	t.Cleanup(cancel)
	return ctx
}
