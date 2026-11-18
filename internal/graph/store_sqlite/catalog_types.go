package store_sqlite

import (
	"fmt"

	"github.com/zzet/gortex/internal/storage"
)

// State vocabularies for the checkout-lifecycle catalog.
//
// The columns are plain TEXT (see checkoutCatalogSchemaSQL); these constants,
// and the validators below, are the only authority on what may be written.
// Keeping the check in Go rather than in a CHECK constraint means extending a
// vocabulary is a code change, not a migration of every installed database.

const DedicatedGraphReady = storage.DedicatedGraphReady

type CheckoutState = storage.CheckoutState

const (
	CheckoutStateReady                  = storage.CheckoutStateReady
	CheckoutStateAvailabilityGrace      = storage.CheckoutStateAvailabilityGrace
	CheckoutStateRemovalGrace           = storage.CheckoutStateRemovalGrace
	CheckoutStateUnavailable            = storage.CheckoutStateUnavailable
	CheckoutStateReconciling            = storage.CheckoutStateReconciling
	CheckoutStateDemoting               = storage.CheckoutStateDemoting
	CheckoutStateForgetting             = storage.CheckoutStateForgetting
	CheckoutStatePrimaryClosureRetiring = storage.CheckoutStatePrimaryClosureRetiring
)

var checkoutStates = []CheckoutState{
	CheckoutStateReady,
	CheckoutStateAvailabilityGrace,
	CheckoutStateRemovalGrace,
	CheckoutStateUnavailable,
	CheckoutStateReconciling,
	CheckoutStateDemoting,
	CheckoutStateForgetting,
	CheckoutStatePrimaryClosureRetiring,
}

type CheckoutMode = storage.CheckoutMode

const (
	CheckoutModeDedicated = storage.CheckoutModeDedicated
	CheckoutModeAutomatic = storage.CheckoutModeAutomatic
)

var checkoutModes = []CheckoutMode{CheckoutModeDedicated, CheckoutModeAutomatic}

type ViewGenerationState = storage.ViewGenerationState

const (
	ViewGenerationBuilding   = storage.ViewGenerationBuilding
	ViewGenerationReady      = storage.ViewGenerationReady
	ViewGenerationSuperseded = storage.ViewGenerationSuperseded
	ViewGenerationRetiring   = storage.ViewGenerationRetiring
	ViewGenerationFailed     = storage.ViewGenerationFailed
)

var viewGenerationStates = []ViewGenerationState{
	ViewGenerationBuilding,
	ViewGenerationReady,
	ViewGenerationSuperseded,
	ViewGenerationRetiring,
	ViewGenerationFailed,
}

type IntentSourceKind = storage.IntentSourceKind

const (
	IntentSourceCLITrack          = storage.IntentSourceCLITrack
	IntentSourceMCPTrack          = storage.IntentSourceMCPTrack
	IntentSourceManualConfig      = storage.IntentSourceManualConfig
	IntentSourceProjectMembership = storage.IntentSourceProjectMembership
)

var intentSourceKinds = []IntentSourceKind{
	IntentSourceCLITrack,
	IntentSourceMCPTrack,
	IntentSourceManualConfig,
	IntentSourceProjectMembership,
}

type IntentTransitionState = storage.IntentTransitionState

const (
	IntentTransitionPending = storage.IntentTransitionPending
	IntentTransitionRunning = storage.IntentTransitionRunning
	IntentTransitionFailed  = storage.IntentTransitionFailed
)

var intentTransitionStates = []IntentTransitionState{
	IntentTransitionPending,
	IntentTransitionRunning,
	IntentTransitionFailed,
}

type RefViewState = storage.RefViewState

const (
	RefViewPending  = storage.RefViewPending
	RefViewBuilding = storage.RefViewBuilding
	RefViewReady    = storage.RefViewReady
	RefViewStale    = storage.RefViewStale
	RefViewFailed   = storage.RefViewFailed
)

var refViewStates = []RefViewState{
	RefViewPending,
	RefViewBuilding,
	RefViewReady,
	RefViewStale,
	RefViewFailed,
}

type RouteState = storage.RouteState

const (
	RoutePending = storage.RoutePending
	RouteActive  = storage.RouteActive
	RouteRetired = storage.RouteRetired
)

var routeStates = []RouteState{RoutePending, RouteActive, RouteRetired}

type RouteSlot = storage.RouteSlot

const (
	RouteSlotCommit = storage.RouteSlotCommit
	RouteSlotDirty  = storage.RouteSlotDirty
)

var routeSlots = []RouteSlot{RouteSlotCommit, RouteSlotDirty}

type CleanupPhase = storage.CleanupPhase

const (
	CleanupPhasePending  = storage.CleanupPhasePending
	CleanupPhaseGrace    = storage.CleanupPhaseGrace
	CleanupPhaseDeleting = storage.CleanupPhaseDeleting
	CleanupPhaseDone     = storage.CleanupPhaseDone
	CleanupPhaseFailed   = storage.CleanupPhaseFailed
)

var cleanupPhases = []CleanupPhase{
	CleanupPhasePending,
	CleanupPhaseGrace,
	CleanupPhaseDeleting,
	CleanupPhaseDone,
	CleanupPhaseFailed,
}

// Catalog errors. Every guarded transition reports its refusal through one of
// these, so a caller can tell "someone else moved first" (retry with fresh
// state) from "this write was malformed" (a bug) without string matching.
var (
	// ErrCatalogStaleGuard means a guarded transition's expected precondition
	// — an incarnation, a route epoch, a primary epoch, or a generation still
	// being in the building state — no longer matched the stored row. Nothing
	// was written.
	ErrCatalogStaleGuard = storage.ErrCatalogStaleGuard

	// ErrCatalogNotFound means the addressed catalog row does not exist.
	ErrCatalogNotFound = storage.ErrCatalogNotFound

	// ErrCatalogGenerationReferenced means a view generation cannot be deleted
	// because a route, a ref view, a dedicated graph, or another generation's
	// base pointer still names it.
	ErrCatalogGenerationReferenced = storage.ErrCatalogGenerationReferenced

	// ErrCatalogIntentTransitionActive means the checkout already has an
	// in-flight intent transition; only one may exist at a time.
	ErrCatalogIntentTransitionActive = storage.ErrCatalogIntentTransitionActive

	// ErrCatalogInvalidValue means a write carried a value outside the state
	// vocabulary, or left a required identifier empty.
	ErrCatalogInvalidValue = storage.ErrCatalogInvalidValue

	// ErrRefViewBuildInFlight means another attempt is already building the
	// same work — the same ref view, tree, base generation and fingerprint.
	// It is the coalescing index reporting itself through ClaimRefViewBuild,
	// and the caller's signal to wait on the attempt that is already running
	// rather than start a second one that would produce the same payload.
	ErrRefViewBuildInFlight = storage.ErrRefViewBuildInFlight
)

type RepositoryFamily = storage.RepositoryFamily

type Checkout = storage.Checkout

type TrackingIntent = storage.TrackingIntent

type IntentTransition = storage.IntentTransition

type CheckoutPathEvidence = storage.CheckoutPathEvidence

type DedicatedGraph = storage.DedicatedGraph

type ViewGeneration = storage.ViewGeneration

type ViewGenerationFilter = storage.ViewGenerationFilter

type ViewLayer = storage.ViewLayer

type CheckoutRoute = storage.CheckoutRoute

type RefView = storage.RefView

type RefViewBuild = storage.RefViewBuild

type UpdateRefViewDesireRequest = storage.UpdateRefViewDesireRequest

type AdoptRefViewGenerationRequest = storage.AdoptRefViewGenerationRequest

type TouchRefViewSelectionRequest = storage.TouchRefViewSelectionRequest

type FailRefViewRequest = storage.FailRefViewRequest

type CompleteRefViewBuildRequest = storage.CompleteRefViewBuildRequest

type CleanupEntry = storage.CleanupEntry

type UpdateCheckoutStateRequest = storage.UpdateCheckoutStateRequest

type UpdateCheckoutObservationRequest = storage.UpdateCheckoutObservationRequest

func validateUpdateCheckoutObservationRequest(r UpdateCheckoutObservationRequest) error {
	if err := requireCatalogID("checkout_id", r.CheckoutID); err != nil {
		return err
	}
	if err := requireCatalogID("incarnation", r.Incarnation); err != nil {
		return err
	}
	return requireCatalogValue("state", r.State, checkoutStates)
}

type FlipCheckoutRouteRequest = storage.FlipCheckoutRouteRequest

type FlipCheckoutRouteSlotRequest = storage.FlipCheckoutRouteSlotRequest

type SetPrimaryDedicatedGraphRequest = storage.SetPrimaryDedicatedGraphRequest

// validCatalogValue reports whether value is one of allowed.
func validCatalogValue[T ~string](value T, allowed []T) bool {
	for _, candidate := range allowed {
		if value == candidate {
			return true
		}
	}
	return false
}

// requireCatalogValue rejects a value outside its vocabulary.
func requireCatalogValue[T ~string](field string, value T, allowed []T) error {
	if validCatalogValue(value, allowed) {
		return nil
	}
	return fmt.Errorf("%w: %s %q", ErrCatalogInvalidValue, field, string(value))
}

// optionalCatalogValue rejects a non-empty value outside its vocabulary and
// accepts the empty string as "not captured".
func optionalCatalogValue[T ~string](field string, value T, allowed []T) error {
	if value == "" {
		return nil
	}
	return requireCatalogValue(field, value, allowed)
}

// requireCatalogID rejects an empty identifier. Every catalog key is a caller-
// supplied opaque string, so the empty one would otherwise become a real row.
func requireCatalogID(field, value string) error {
	if value == "" {
		return fmt.Errorf("%w: %s must not be empty", ErrCatalogInvalidValue, field)
	}
	return nil
}

func validateRepositoryFamily(f RepositoryFamily) error {
	if err := requireCatalogID("family_id", f.FamilyID); err != nil {
		return err
	}
	if err := requireCatalogID("common_dir_identity", f.CommonDirIdentity); err != nil {
		return err
	}
	return requireCatalogID("state", f.State)
}

func validateCheckout(c Checkout) error {
	if err := requireCatalogID("checkout_id", c.CheckoutID); err != nil {
		return err
	}
	if err := requireCatalogID("incarnation", c.Incarnation); err != nil {
		return err
	}
	if err := requireCatalogID("family_id", c.FamilyID); err != nil {
		return err
	}
	if err := requireCatalogValue("state", c.State, checkoutStates); err != nil {
		return err
	}
	if err := requireCatalogValue("desired_mode", c.DesiredMode, checkoutModes); err != nil {
		return err
	}
	return requireCatalogValue("effective_mode", c.EffectiveMode, checkoutModes)
}

func validateTrackingIntent(i TrackingIntent) error {
	if err := requireCatalogID("intent_id", i.IntentID); err != nil {
		return err
	}
	if err := requireCatalogID("checkout_id", i.CheckoutID); err != nil {
		return err
	}
	if err := requireCatalogValue("source_kind", i.SourceKind, intentSourceKinds); err != nil {
		return err
	}
	return requireCatalogID("source_locator", i.SourceLocator)
}

func validateIntentTransition(t IntentTransition) error {
	if err := requireCatalogID("transition_id", t.TransitionID); err != nil {
		return err
	}
	if err := requireCatalogID("checkout_id", t.CheckoutID); err != nil {
		return err
	}
	if err := requireCatalogID("cause", t.Cause); err != nil {
		return err
	}
	if err := requireCatalogValue("state", t.State, intentTransitionStates); err != nil {
		return err
	}
	if err := optionalCatalogValue("prior_desired_mode", t.PriorDesiredMode, checkoutModes); err != nil {
		return err
	}
	if err := optionalCatalogValue("prior_effective_mode", t.PriorEffectiveMode, checkoutModes); err != nil {
		return err
	}
	if err := optionalCatalogValue("requested_mode", t.RequestedMode, checkoutModes); err != nil {
		return err
	}
	return optionalCatalogValue("prior_checkout_state", t.PriorCheckoutState, checkoutStates)
}

func validateDedicatedGraph(g DedicatedGraph) error {
	if err := requireCatalogID("graph_id", g.GraphID); err != nil {
		return err
	}
	if err := requireCatalogID("repo_prefix", g.RepoPrefix); err != nil {
		return err
	}
	if err := requireCatalogID("family_id", g.FamilyID); err != nil {
		return err
	}
	return requireCatalogID("state", g.State)
}

func validateViewGeneration(g ViewGeneration) error {
	if err := requireCatalogID("owner_kind", g.OwnerKind); err != nil {
		return err
	}
	if err := requireCatalogID("generation_kind", g.GenerationKind); err != nil {
		return err
	}
	return requireCatalogValue("state", g.State, viewGenerationStates)
}

func validateViewLayer(l ViewLayer) error {
	if err := requireCatalogID("layer_id", l.LayerID); err != nil {
		return err
	}
	if err := requireCatalogID("kind", l.Kind); err != nil {
		return err
	}
	return requireCatalogID("graph_id", l.GraphID)
}

func validateCheckoutRoute(r CheckoutRoute) error {
	if err := requireCatalogID("checkout_id", r.CheckoutID); err != nil {
		return err
	}
	if err := requireCatalogID("graph_id", r.GraphID); err != nil {
		return err
	}
	return requireCatalogValue("state", r.State, routeStates)
}

func validateRefView(v RefView) error {
	if err := requireCatalogID("ref_view_id", v.RefViewID); err != nil {
		return err
	}
	if err := requireCatalogID("graph_id", v.GraphID); err != nil {
		return err
	}
	if err := requireCatalogID("selector_kind", v.SelectorKind); err != nil {
		return err
	}
	if err := requireCatalogID("selector_value", v.SelectorValue); err != nil {
		return err
	}
	if err := requireCatalogID("enrichment_profile", v.EnrichmentProfile); err != nil {
		return err
	}
	return requireCatalogValue("state", v.State, refViewStates)
}

func validateRefViewBuild(b RefViewBuild) error {
	if err := requireCatalogID("build_id", b.BuildID); err != nil {
		return err
	}
	if err := requireCatalogID("ref_view_id", b.RefViewID); err != nil {
		return err
	}
	if err := requireCatalogID("build_fingerprint", b.BuildFingerprint); err != nil {
		return err
	}
	if err := requireCatalogID("build_token", b.BuildToken); err != nil {
		return err
	}
	return requireCatalogValue("state", b.State, viewGenerationStates)
}

func validateCleanupEntry(e CleanupEntry) error {
	if err := requireCatalogID("cleanup_id", e.CleanupID); err != nil {
		return err
	}
	if err := requireCatalogID("opaque_target_ids", e.OpaqueTargetIDs); err != nil {
		return err
	}
	if err := requireCatalogID("reason", e.Reason); err != nil {
		return err
	}
	return requireCatalogValue("phase", e.Phase, cleanupPhases)
}
