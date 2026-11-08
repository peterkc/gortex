package storage

// CheckoutState is the lifecycle state of one working copy.
type CheckoutState string

// CheckoutMode is how a checkout is served: from its own graph, or from the
// family's shared automatic lane.
type CheckoutMode string

// ViewGenerationState is the lifecycle state of one view generation. It also
// governs ref_view_builds.state: a build's state is the state of the
// generation it is producing, and the in-flight coalescing index keys on the
// building value.
type ViewGenerationState string

// IntentSourceKind is who asked for a checkout to be tracked.
type IntentSourceKind string

// IntentTransitionState is the progress of the single in-flight mode change a
// checkout may have. There is no terminal success value: completing a
// transition deletes the row, because UNIQUE(checkout_id) is what stops a
// second transition from starting and a retained success row would block it
// forever. A failed transition is retained deliberately — it keeps the slot
// occupied until something resolves it.
type IntentTransitionState string

// RefViewState is the lifecycle state of a named view of a graph.
type RefViewState string

// RouteState is the lifecycle state of one checkout's route.
type RouteState string

const (
	// CheckoutStateReady is the steady state: the path is present and served.
	CheckoutStateReady CheckoutState = "checkout_ready"
	// CheckoutStateAvailabilityGrace means the path stopped answering but its
	// availability deadline has not expired, so nothing is torn down yet.
	CheckoutStateAvailabilityGrace CheckoutState = "availability_grace"
	// CheckoutStateRemovalGrace means Git authoritatively stopped listing the
	// checkout but its removal deadline has not expired. Queries fall back to
	// the family primary while the identity remains recoverable.
	CheckoutStateRemovalGrace CheckoutState = "removal_grace"
	// CheckoutStateUnavailable retains an intended checkout after serving
	// layers expire. Its identity and tracking configuration survive recovery.
	CheckoutStateUnavailable CheckoutState = "checkout_unavailable"
	// CheckoutStateReconciling means a reconciler is bringing the checkout's
	// effective mode in line with its desired mode.
	CheckoutStateReconciling CheckoutState = "reconciling"
	// CheckoutStateDemoting means the checkout is dropping from a dedicated
	// graph back to the automatic lane.
	CheckoutStateDemoting CheckoutState = "demoting"
	// CheckoutStateForgetting means the checkout is being removed from the
	// catalog and its dependent rows released.
	CheckoutStateForgetting CheckoutState = "forgetting_checkout"
	// CheckoutStatePrimaryClosureRetiring means the whole family's primary
	// closure is retiring and this checkout is going with it.
	CheckoutStatePrimaryClosureRetiring CheckoutState = "primary_closure_retiring"
)

const (
	// CheckoutModeDedicated serves the checkout from its own graph.
	CheckoutModeDedicated CheckoutMode = "dedicated"
	// CheckoutModeAutomatic serves the checkout from the family's shared lane.
	CheckoutModeAutomatic CheckoutMode = "automatic"
)

const (
	// ViewGenerationBuilding is the only mutable state.
	ViewGenerationBuilding ViewGenerationState = "building"
	// ViewGenerationReady is published and immutable.
	ViewGenerationReady ViewGenerationState = "ready"
	// ViewGenerationSuperseded is a ready generation a newer one replaced.
	ViewGenerationSuperseded ViewGenerationState = "superseded"
	// ViewGenerationRetiring is queued for deletion once nothing points at it.
	ViewGenerationRetiring ViewGenerationState = "retiring"
	// ViewGenerationFailed never reached ready.
	ViewGenerationFailed ViewGenerationState = "failed"
)

const (
	// IntentSourceCLITrack is an explicit `gortex` command.
	IntentSourceCLITrack IntentSourceKind = "cli_track"
	// IntentSourceMCPTrack is an explicit tool call over MCP.
	IntentSourceMCPTrack IntentSourceKind = "mcp_track"
	// IntentSourceManualConfig is a checkout named in configuration.
	IntentSourceManualConfig IntentSourceKind = "manual_config"
	// IntentSourceProjectMembership is a checkout tracked because it belongs
	// to a tracked project rather than because anyone named it.
	IntentSourceProjectMembership IntentSourceKind = "project_membership"
)

const (
	// IntentTransitionPending is created but not yet started.
	IntentTransitionPending IntentTransitionState = "pending"
	// IntentTransitionRunning is being applied.
	IntentTransitionRunning IntentTransitionState = "running"
	// IntentTransitionFailed stopped part-way and holds the slot.
	IntentTransitionFailed IntentTransitionState = "failed"
)

const (
	// RefViewPending has a desired selector but nothing resolved yet.
	RefViewPending RefViewState = "pending"
	// RefViewBuilding has an in-flight build for its desired fingerprint.
	RefViewBuilding RefViewState = "building"
	// RefViewReady is serving its active generation.
	RefViewReady RefViewState = "ready"
	// RefViewStale is still serving, but the desired selector has moved on.
	RefViewStale RefViewState = "stale"
	// RefViewFailed could not resolve or build.
	RefViewFailed RefViewState = "failed"
)

const (
	// RoutePending has no generation to serve from yet.
	RoutePending RouteState = "pending"
	// RouteActive is serving.
	RouteActive RouteState = "active"
	// RouteRetired no longer serves; its generations may be released.
	RouteRetired RouteState = "retired"
)

// DedicatedGraphReady is a dedicated graph that can serve queries. This is
// distinct from ViewGenerationReady, which describes a published generation.
const DedicatedGraphReady = "graph_ready"

// DedicatedGraphClosing is durable withdrawal, not successful cleanup. Its
// owner and prefix remain reserved until the cleanup saga deletes the graph.
const DedicatedGraphClosing = "closing"
