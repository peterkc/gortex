package indexer

import (
	"context"
	"time"

	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/reconcile"
	"github.com/zzet/gortex/internal/storage"
)

// CheckoutCatalog is the catalog paired with a generation store.
type CheckoutCatalog interface {
	IsNilMaterializerCatalog() bool
	reconcile.Catalog
	GetCheckoutRoutes(ctx context.Context, checkoutIDs []string) (map[string]storage.CheckoutRoute, error)
	AcquireDedicatedBaseAuthority(ctx context.Context, req storage.AcquireDedicatedBaseAuthorityRequest) (storage.DedicatedBaseAuthority, error)
	AdoptDedicatedBaseGeneration(ctx context.Context, req storage.AdoptDedicatedBaseGenerationRequest) (storage.DedicatedBaseAdoption, error)
	AdoptRefViewGeneration(ctx context.Context, req storage.AdoptRefViewGenerationRequest) error
	AllocateCheckout(ctx context.Context, checkout storage.Checkout) error
	BeginIntentTransitionWithTrackingIntent(
		ctx context.Context, req storage.BeginIntentTransitionRequest,
	) (storage.IntentTransition, bool, error)
	BeginRepositoryCleanup(ctx context.Context, graphID string) (storage.RepositoryCleanupIdentity, bool, error)
	ClaimDedicatedBaseBuild(ctx context.Context, req storage.ClaimDedicatedBaseBuildRequest) (storage.DedicatedBaseBuildClaim, error)
	ClaimRefViewBuild(
		ctx context.Context,
		build storage.RefViewBuild,
		abandonedBefore int64,
	) (storage.RefViewBuild, error)
	CompleteIntentTransition(ctx context.Context, checkoutID, transitionID string) error
	CompleteRefViewBuild(ctx context.Context, req storage.CompleteRefViewBuildRequest) error
	DedicatedBasePublication(ctx context.Context, graphID string) (storage.DedicatedBasePublication, bool, error)
	DeleteCheckoutRoute(ctx context.Context, checkoutID string) error
	DeleteRefView(ctx context.Context, refViewID string) error
	DeleteUnpublishedDedicatedGraph(ctx context.Context, expected storage.DedicatedGraph) error
	FailDedicatedBaseBuild(ctx context.Context, req storage.FailDedicatedBaseBuildRequest) error
	FailRefView(ctx context.Context, req storage.FailRefViewRequest) error
	FindReusableViewGenerations(
		ctx context.Context,
		identity storage.ViewGeneration,
		limit int,
	) ([]storage.ViewGeneration, error)
	FlipCheckoutRoute(ctx context.Context, req storage.FlipCheckoutRouteRequest) error
	FlipCheckoutRouteSlot(ctx context.Context, req storage.FlipCheckoutRouteSlotRequest) error
	GetCheckout(ctx context.Context, checkoutID string) (storage.Checkout, bool, error)
	GetCheckoutPathEvidence(ctx context.Context, checkoutID string) (storage.CheckoutPathEvidence, bool, error)
	GetCheckoutRoute(ctx context.Context, checkoutID string) (storage.CheckoutRoute, bool, error)
	GetDedicatedGraph(ctx context.Context, graphID string) (storage.DedicatedGraph, bool, error)
	GetDedicatedGraphByOwner(
		ctx context.Context, ownerCheckoutID string,
	) (storage.DedicatedGraph, bool, error)
	GetIntentTransition(ctx context.Context, checkoutID string) (storage.IntentTransition, bool, error)
	GetOrCreateRefView(ctx context.Context, view storage.RefView) (storage.RefView, error)
	GetRefView(ctx context.Context, refViewID string) (storage.RefView, bool, error)
	GetRepositoryFamily(ctx context.Context, familyID string) (storage.RepositoryFamily, bool, error)
	GetViewGeneration(ctx context.Context, generationID int64) (storage.ViewGeneration, bool, error)
	InFlightRefViewBuild(
		ctx context.Context,
		key storage.RefViewBuildKey,
		aliveAfter int64,
	) (storage.RefViewBuild, bool, error)
	ListCheckouts(ctx context.Context, familyID string) ([]storage.Checkout, error)
	ListDedicatedGraphs(ctx context.Context, familyID string) ([]storage.DedicatedGraph, error)
	ListIntentTransitions(ctx context.Context) ([]storage.IntentTransition, error)
	ListRefViews(ctx context.Context, graphID string) ([]storage.RefView, error)
	ListRepositoryCleanupGenerations(ctx context.Context, graphID string) ([]int64, error)
	ListRepositoryCleanupOwners(ctx context.Context) ([]storage.RepositoryCleanupIdentity, error)
	ListRepositoryFamilies(ctx context.Context) ([]storage.RepositoryFamily, error)
	ListTrackingIntents(ctx context.Context, checkoutID string) ([]storage.TrackingIntent, error)
	ListViewGenerations(ctx context.Context, filter storage.ViewGenerationFilter) ([]storage.ViewGeneration, error)
	MarkUnreferencedRefViewGenerationSuperseded(
		ctx context.Context,
		generationID int64,
	) (bool, error)
	ObserveDedicatedBaseAdoptions(fn func(storage.DedicatedBaseAdoptionEvent)) func()
	RebaseViewGeneration(ctx context.Context, req storage.RebaseViewGenerationRequest) error
	RecordDedicatedBaseDesire(ctx context.Context, req storage.RecordDedicatedBaseDesireRequest) (storage.DedicatedBaseDesire, error)
	ReleaseRefViewGeneration(
		ctx context.Context,
		refViewID string,
		generationID int64,
	) (bool, error)
	ReleaseRepositoryCleanupPublication(ctx context.Context, expected storage.RepositoryCleanupIdentity) error
	RetirementCandidateShapes(ctx context.Context, ids []int64) (map[int64]storage.RetirementCandidateShape, error)
	SetPrimaryDedicatedGraph(ctx context.Context, req storage.SetPrimaryDedicatedGraphRequest) error
	SetViewGenerationState(ctx context.Context, generationID int64, next storage.ViewGenerationState, expected ...storage.ViewGenerationState) error
	TouchRefViewBuild(ctx context.Context, buildID, buildToken string, lastProgress int64) error
	TouchRefViewSelection(ctx context.Context, req storage.TouchRefViewSelectionRequest) error
	UpdateCheckoutObservation(ctx context.Context, req storage.UpdateCheckoutObservationRequest) error
	UpdateCheckoutRecoveryState(ctx context.Context, checkoutID, incarnation string, expected, next storage.CheckoutState, detail string, now int64) error
	UpdateCheckoutState(ctx context.Context, req storage.UpdateCheckoutStateRequest) error
	UpdateIntentTransitionProgress(
		ctx context.Context,
		checkoutID, transitionID string,
		state storage.IntentTransitionState,
		lastError string,
		lastProgress int64,
	) error
	UpdateRefViewDesire(ctx context.Context, req storage.UpdateRefViewDesireRequest) error
	UpsertCheckoutPathEvidence(ctx context.Context, evidence storage.CheckoutPathEvidence) error
	UpsertCheckoutRoute(ctx context.Context, route storage.CheckoutRoute) error
	UpsertDedicatedGraph(ctx context.Context, dedicated storage.DedicatedGraph) error
	UpsertRepositoryFamily(ctx context.Context, family storage.RepositoryFamily) error
	UpsertTrackingIntent(ctx context.Context, intent storage.TrackingIntent) error
}

type generationPayloadStore interface {
	EvictFiles(filePaths []string) (nodesRemoved, edgesRemoved int)
	DeleteFileMetasByFiles(repoPrefix string, files []string) error
	EndGenerationBulkLoad() error
	BeginGenerationBulkLoad(generationID int64) (bool, error)
	SharesSymbolSearchCore(other any) bool
	EdgeSourceMasksContext(ctx context.Context) ([]storage.EdgeSourceMask, error)
	OpenContractCoreWindows(repo string) []storage.ContractCoreWindow
	ViewGenerationRecord(context.Context, int64) (storage.ViewGeneration, bool, error)
	CaptureContractPublicationSourceStore(ctx context.Context, expected graph.ContractInputState, witnesses []graph.ContractInputWitness) (graph.Store, error)
	ContractReceiptEvents() (uint64, <-chan struct{})
	ContractCoreWindowLastClose(repo, checkout, path string, before time.Time) (storage.ContractCoreWindowClose, bool)
	ContractCoreWindowClosedAt(repo, checkout, path string, seq uint64) (time.Time, bool)
	AcceptContractInputMutationWithBaselineContext(ctx context.Context, expected graph.ContractInputState, baseline graph.ContractBoundaryReceiptBaseline) error
	GetContractAttachmentContext(ctx context.Context, key graph.ContractAttachmentKey) (*graph.ContractAttachment, error)
	PublishContractAttachmentsWithInputsContext(ctx context.Context, targets []storage.ContractAttachmentPublishTarget, publishedAt int64) ([]storage.ContractAttachmentPublication, error)
	SetSymbolFTSNormalization(repoPrefix, mode string) error
	SetProducerState(row storage.ProducerCompleteness) error
	DerivationStamps(ctx context.Context) (map[string]int, error)
	AllEdgesLight(kinds ...graph.EdgeKind) []*graph.Edge
	AllNodesLight() []*graph.Node
	BatchUpsertSymbolFTS(items []graph.SymbolFTSItem) error
	BulkSetCloneCorpus(repoPrefix string, rows []graph.CloneCorpusRow) error
	CloneCorpusPage(repoPrefix, afterNodeID string, limit int) ([]graph.CloneCorpusRow, error)
	FileMetasForRepo(repoPrefix string) ([]graph.FileMetaRow, error)
	GetRepoNodesLight(repoPrefix string) []*graph.Node
	LoadRefFactsByFiles(repoPrefix string, files []string) ([]graph.RefFact, error)
	LoadRefFactsByTargets(repoPrefix string, targetIDs []string) (map[string][]graph.RefFact, error)
	ScanRepoMemoryEstimates(ctx context.Context) (map[string]graph.RepoMemoryEstimate, error)
	graph.Store
	contractCoreStore
	AddBatchChecked(nodes []*graph.Node, edges []*graph.Edge) error
	CaptureContractProjectionClock(ctx context.Context) (storage.ContractProjectionCursor, error)
	ContractBoundaryReceiptBaselineContext(ctx context.Context, repo, actor string) (*graph.ContractBoundaryReceiptBaseline, error)
	ContractBoundaryReceiptContext(ctx context.Context, repo, actor, file string) (*graph.ContractBoundaryReceipt, bool, error)
	ContractBoundaryReceiptStampsContext(ctx context.Context, repo, actor string, paths []string) (map[string]storage.ContractBoundaryReceiptStamp, error)
	ContractInputStatesForRepoContext(ctx context.Context, repo string) ([]graph.ContractInputState, error)
	ContractProjectionFootprintContext(ctx context.Context, budgetBytes int64) (storage.ContractProjectionFootprint, error)
	ContractReceiptWriteSample(repo string) (begun, done uint64)
	ContractReceiptWriteStates(repo, checkout string, paths []string) map[string]storage.ContractReceiptWriteState
	ContractSeedEdgesContext(ctx context.Context, repo string) ([]*graph.Edge, error)
	ContractSeedNodesContext(ctx context.Context, repo string) ([]*graph.Node, error)
	FileMasksContext(ctx context.Context) ([]storage.FileMask, error)
	FileMasksForContext(ctx context.Context, repo string, paths []string) ([]storage.FileMask, error)
	FileMetasForRepoContext(ctx context.Context, repoPrefix string) ([]graph.FileMetaRow, error)
	GetRepoIndexState(repoPrefix string) (graph.RepoIndexState, bool, error)
	InputManifest(ctx context.Context) (storage.InputManifestMeta, []storage.InputManifestEntry, bool, error)
	IsNilGenerationHandle() bool
	NodeIdentityMasksContext(ctx context.Context) ([]storage.NodeIdentityMask, error)
	PayloadInputRevision() storage.PayloadInputRevision
	PayloadInputRevisionContext(ctx context.Context) (storage.PayloadInputRevision, error)
	ProducerStates() ([]storage.ProducerCompleteness, error)
	PublishedRepoLanguageCountsContext(ctx context.Context, repoPrefix string) (map[string]int, error)
	RemoveEdgesExact(edges []*graph.Edge) int
	RepoLanguageCountsContext(ctx context.Context, repoPrefixes []string) (map[string]map[string]int, error)
	SetEdgeSourceMasks(masks []storage.EdgeSourceMask) error
	SetFileMasks(masks []storage.FileMask) error
	SetNodeIdentityReplacements(nodeIDs []string) error
	SetNodeTombstones(nodeIDs []string) error
	SetProducerStates(rows []storage.ProducerCompleteness) error
	SetRepoIndexState(st graph.RepoIndexState) error
	SymbolSearchCoreKey() any
	VisitContractBoundaryReceiptsContext(ctx context.Context, repo, actor string, files []string, visit func(graph.ContractBoundaryReceipt) error) error
	WALWriteMark() storage.WALWriteMark
	WriteDerivationStamps(ctx context.Context, stamps map[string]int) error
	WriteInputManifest(ctx context.Context, meta storage.InputManifestMeta, entries []storage.InputManifestEntry) error
	WriteMark() storage.VFSIOMark
}

// GenerationBuilderStore is the store input to the dedicated-base runtime.
type GenerationBuilderStore = generationBuilderStore

type generationBuilderStore interface {
	SymbolSearchCoreKey() any
	RetirePayloadGeneration(ctx context.Context, generationID int64, inUse func(int64) bool) error
	ContractInputIdentityPublishedContext(ctx context.Context, w graph.ContractInputWitness) (bool, error)
	ContractAttachmentProofContext(ctx context.Context, key graph.ContractAttachmentKey) (*graph.ContractAttachment, []graph.ContractInputWitness, error)
	TakeGenerationWriteCounters(generationID int64) (storage.GenerationWriteCounters, bool)
	AtGenerationStore(generationID int64) graph.Store
	WriteWanted() bool
	graph.Store
	AtManagedGenerationStore(generationID int64) (graph.Store, error)
	BeginPayloadGenerationStoreWithStatus(ctx context.Context, req storage.PayloadGenerationRequest) (int64, graph.Store, bool, error)
	CapturePayloadInputWitness(ctx context.Context, generationIDs []int64) (*storage.PayloadInputWitness, error)
	GenerationCorrectionEpoch(g int64) uint64
	IsNilGenerationHandle() bool
	JoinPayloadBuildFlightHandle(ctx context.Context, generationID int64, adopted bool) (storage.PayloadBuildFlightHandle, bool, bool, error)
	Path() string
	PublishPayloadGeneration(ctx context.Context, generationID, publishedAt int64) error
	PublishPayloadGenerationWithInputWitness(ctx context.Context, generationID, publishedAt int64, witness *storage.PayloadInputWitness) error
	ReaderWaitMark() storage.ReaderWaitMark
	SharesSymbolSearchCore(other any) bool
	WALWriteMark() storage.WALWriteMark
}

// CheckoutGenerationStore is the store input to the contract analysis runtime.
type CheckoutGenerationStore = checkoutGenerationStore

type checkoutGenerationStore interface {
	SymbolSearchViewGeneration() int64
	OpenContractCoreWindows(repo string) []storage.ContractCoreWindow
	ContractCoreOrphanEvents() (uint64, <-chan struct{})
	CopyGenerationPayloadWhole(ctx context.Context, from, to int64) (counts storage.GenerationCopyCounts, err error)
	AnnounceCheckoutRefresh() (release func())
	GetContractAttachmentContext(ctx context.Context, key graph.ContractAttachmentKey) (*graph.ContractAttachment, error)
	RetirePayloadGenerationSlice(
		ctx context.Context,
		generationID int64,
		inUse func(int64) bool,
		maxElapsed time.Duration,
	) error
	RequestWALReclaim()
	RetirePayloadGenerationQuantum(ctx context.Context, generationID int64, inUse func(int64) bool) (storage.PayloadRetirementProgress, error)
	RetirePayloadGenerationQuantumFenced(ctx context.Context, generationID int64, inUse func(int64) bool) (storage.PayloadRetirementProgress, error)
	StorageFailures() []storage.StorageFailure
	generationBuilderStore
	BeginChainFoldHandle(ctx context.Context, req storage.ChainFoldRequest) (storage.ChainFoldHandle, error)
	BeginDerivedCorrectionHandle(ctx context.Context, req storage.DerivedCorrectionRequest) (storage.DerivedCorrectionHandle, error)
	FlattenGenerationChain(ctx context.Context, chain []int64, to int64) (counts storage.GenerationCopyCounts, err error)
	GenerationsWithStaleDerivation(ctx context.Context, pass string, version int) ([]storage.StaleDerivationGeneration, error)
	MarkPayloadGenerationSuperseded(ctx context.Context, generationID int64) error
	PayloadBuildFlightActive(generationID int64) bool
	PreparePayloadGenerationPublicationHandle(ctx context.Context, generationID int64) (storage.PreparedPayloadPublication, error)
	RetirePayloadGeneration(ctx context.Context, generationID int64, inUse func(int64) bool) error
}

type contractEvidenceStore interface {
	AddBatchChecked(nodes []*graph.Node, edges []*graph.Edge) error
	graph.Store
	Close() (closeErr error)
	GetNodePresenceByIDsContext(ctx context.Context, ids []string) (map[string]struct{}, error)
	GetNodesByIDsContext(ctx context.Context, ids []string) (map[string]*graph.Node, error)
	GetOutEdgesByNodeIDsWithMetadataContext(ctx context.Context, ids []string, limit int) (map[string][]*graph.Edge, bool, error)
	LayerContractFileProjectionContext(ctx context.Context, _ string, paths []string) (graph.ContractFileProjection, error)
}

// generationAt preserves the selected backend's exact borrowed handle.
func generationAt(source generationStoreSource, generation int64) generationPayloadStore {
	handle := source.AtGenerationStore(generation)
	if handle == nil {
		return nil
	}
	return handle.(generationPayloadStore)
}

type ContractReservationStore interface {
	FencePayloadGenerationRetirement(ctx context.Context, generationID int64, inUse func(int64) bool) error
	IsNilGenerationHandle() bool
	Path() string
}

type contractPublicationStore interface {
	IsNilGenerationHandle() bool
	Path() string
	FencePayloadGenerationRetirement(ctx context.Context, generationID int64, inUse func(int64) bool) error
	graph.Store
	BeginPayloadGenerationStore(ctx context.Context, req storage.PayloadGenerationRequest) (int64, graph.Store, error)
	GetContractAttachmentContext(ctx context.Context, key graph.ContractAttachmentKey) (*graph.ContractAttachment, error)
	PublishContractAttachmentsWithInputsContext(ctx context.Context, targets []storage.ContractAttachmentPublishTarget, publishedAt int64) ([]storage.ContractAttachmentPublication, error)
	RetirePayloadGeneration(ctx context.Context, generationID int64, inUse func(int64) bool) error
}

type retiringGenerationStore interface {
	IsNilGenerationHandle() bool
	RetirePayloadGeneration(ctx context.Context, generationID int64, inUse func(int64) bool) error
}
