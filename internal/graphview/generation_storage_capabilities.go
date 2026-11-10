package graphview

import (
	"context"
	"iter"

	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/storage"
)

type GenerationLayerHandle interface {
	graph.SymbolSearcher
	GenerationCorrectionEpoch(int64) uint64
	MutationRevision() uint64
	NodesByKindsContext(context.Context, []graph.NodeKind) ([]*graph.Node, error)
	ContractAnalysisNodesContext(context.Context, int) ([]*graph.Node, bool, error)
	IsNilGenerationHandle() bool
	AllEdges() []*graph.Edge
	AllNodes() []*graph.Node
	ContractExtractionEdgesContext(ctx context.Context, ids []string, limit int) (map[string][]*graph.Edge, bool, error)
	EdgeCount() int
	EdgeEndpointsFrom(ids []string, kinds []graph.EdgeKind) []graph.EdgeEndpointRow
	EdgeEndpointsRecordedAt(paths []string) []graph.EdgeEndpointRow
	EdgeSourceMasksContext(ctx context.Context) ([]storage.EdgeSourceMask, error)
	EdgesByKind(kind graph.EdgeKind) iter.Seq[*graph.Edge]
	FileIndexFailuresForRepo(repoPrefix string) ([]graph.FileIndexFailure, error)
	FileMasksContext(ctx context.Context) ([]storage.FileMask, error)
	FindIncomingSourcesScoped(ctx context.Context, targetIDs []string, kind graph.EdgeKind, limit int, scope graph.IncomingSourceScope, budget *graph.IncomingSourceBudget) (graph.BoundedIncomingSourceProjection, error)
	FindNodesByName(name string) []*graph.Node
	FindNodesByNameContext(ctx context.Context, name string) ([]*graph.Node, error)
	FindNodesByNames(names []string) map[string][]*graph.Node
	FindNodesByNamesInRepoLanguages(names []string, repoPrefix string, languages []string) map[string][]*graph.Node
	GenerationPayloadPresence() (nodes, edges bool)
	GetFileNodes(filePath string) []*graph.Node
	GetInEdges(nodeID string) []*graph.Edge
	GetInEdgesByNodeIDs(ids []string) map[string][]*graph.Edge
	GetNode(id string) *graph.Node
	GetNodeByQualName(qualName string) *graph.Node
	GetNodeKindsByIDsContext(ctx context.Context, ids []string) (map[string]graph.NodeKindRow, error)
	GetNodesByIDs(ids []string) map[string]*graph.Node
	GetNodesByQualNames(qualNames []string) map[string][]*graph.Node
	GetOutEdges(nodeID string) []*graph.Edge
	GetOutEdgesByNodeIDs(ids []string) map[string][]*graph.Edge
	GetOutEdgesByNodeIDsWithMetadataContext(ctx context.Context, ids []string, limit int) (map[string][]*graph.Edge, bool, error)
	IncomingSourceNodeExists(ctx context.Context, id string, query graph.IncomingSourceNodeQuery) (bool, error)
	LayerContractFileNodesContext(ctx context.Context, _ string, paths []string) (map[string][]*graph.Node, error)
	LayerContractFileProjectionContext(ctx context.Context, _ string, paths []string) (graph.ContractFileProjection, error)
	LayerContractIDProjectionContext(ctx context.Context, ids []string) (graph.ContractFileProjection, error)
	LayerContractRepoOwnerProjectionContext(ctx context.Context, repo string) (graph.ContractFileProjection, error)
	LayerContractRepoProjectionContext(ctx context.Context, repo string) (graph.ContractFileProjection, error)
	NodeCount() int
	NodeIdentityLocalizationSummariesContext(ctx context.Context, ids []string, includeTestFlag bool) ([]*graph.Node, error)
	NodeIdentityMaskSummariesContext(ctx context.Context, masks []storage.NodeIdentityMask) ([]*graph.Node, error)
	NodeIdentityMasksContext(ctx context.Context) ([]storage.NodeIdentityMask, error)
	NodesByKind(kind graph.NodeKind) iter.Seq[*graph.Node]
	NodesInScopeSeq(repoPrefixes, filePaths []string, kinds ...graph.NodeKind) iter.Seq[*graph.Node]
	NodesLightByKindsInScopeSeq(repoPrefixes, filePaths []string, kinds ...graph.NodeKind) iter.Seq[*graph.Node]
	NodesLightInScopeSeq(repoPrefixes, filePaths []string) iter.Seq[*graph.Node]
	PayloadInputRevision() storage.PayloadInputRevision
	PayloadInputRevisionContext(ctx context.Context) (storage.PayloadInputRevision, error)
	ReadConstantValueProjectionContext(ctx context.Context, ids []string, files []graph.ConstantFileKey) (graph.ConstantValueProjection, error)
	ReadIncomingSourceCandidates(ctx context.Context, targetIDs []string, kind graph.EdgeKind) (map[string][]graph.IncomingSourceCandidate, error)
	RecordedEdgesAt(paths []string) []*graph.Edge
	RepoEdgesByKinds(repoPrefixes []string, kinds []graph.EdgeKind) []graph.RepoEdgeRow
	SemanticBindingTypes(sites []graph.SemanticBindingSite) (map[graph.SemanticBindingSite]string, error)
	ViewGeneration() int64
	VisitNodeDeclarationsByNamesContext(ctx context.Context, names []string, yield func(*graph.Node) bool) error
	VisitNodeDeclarationsByNamesInRepoContext(ctx context.Context, names []string, repo string, yield func(*graph.Node) bool) error
	VisitNodesByNameContainingFolded(substr string, yield func(*graph.Node) bool)
	VisitNodesByNameContainingFoldedContext(ctx context.Context, substr string, yield func(*graph.Node) bool) error
	VisitNodesByNameContext(ctx context.Context, name string, yield func(*graph.Node) bool) (err error)
	VisitNodesByNamesContext(ctx context.Context, names []string, yield func(*graph.Node) bool) error
	WithFileEditingContextReader(ctx context.Context, fn func(context.Context, graph.FileEditingContextRows) error) error
}

type materializerCatalog interface {
	IsNilMaterializerCatalog() bool
	GetCheckoutRoute(context.Context, string) (storage.CheckoutRoute, bool, error)
	GetDedicatedGraph(context.Context, string) (storage.DedicatedGraph, bool, error)
	GetViewGeneration(context.Context, int64) (storage.ViewGeneration, bool, error)
}

// MaterializerStore is the store input to a Materializer.
type MaterializerStore = materializerStore

type materializerStore interface {
	graph.Store
	IsNilGenerationHandle() bool
	AtGenerationStore(int64) graph.Store
	GenerationCorrectionEpoch(int64) uint64
	ContractInputIdentityPublishedContext(context.Context, graph.ContractInputWitness) (bool, error)
	ContractAttachmentProofContext(context.Context, graph.ContractAttachmentKey) (*graph.ContractAttachment, []graph.ContractInputWitness, error)
}

type layerCacheSource interface {
	IsNilGenerationHandle() bool
	AtGenerationStore(int64) graph.Store
	GenerationCorrectionEpoch(int64) uint64
}

// GenerationSourceHandle is the exact physical handle borrowed with its layer.
type GenerationSourceHandle interface {
	GenerationLayerHandle
	graph.Store
	contractInputHandle
	contractReadinessHandle
	producerGeneration
}

type producerGeneration interface {
	FileMasks() ([]storage.FileMask, error)
}
