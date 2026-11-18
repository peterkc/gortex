package mcp

import (
	"context"

	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/storage"
)

type analysisSource interface {
	ViewGeneration() int64
	AtGenerationStore(int64) graph.Store
	ViewScopedAnalysisStore() graph.Store
	SharesSymbolSearchCore(any) bool
}

type analysisWriteBinder interface {
	BindAnalysisWriteContext(context.Context) graph.AnalysisGenerationStore
	SharesSymbolSearchCore(any) bool
	IsNilGenerationHandle() bool
}

type outlineSource interface {
	MutationRevision() uint64
	SharesSymbolSearchCore(any) bool
}

type exclusionSource interface {
	NodeIDsByKindsBoundedContext(context.Context, []graph.NodeKind, int) ([]string, bool, error)
	SymbolSearchCoreKey() any
	ViewGeneration() int64
	SharesSymbolSearchCore(any) bool
	IsNilGenerationHandle() bool
}

type nativeRankSource interface {
	SymbolFTSStats(context.Context) (storage.SymbolFTSStats, error)
	SymbolFTSScoringSnapshot(context.Context, []string) (storage.SymbolFTSStats, map[string]int64, error)
	SymbolFTSPrefixHits(context.Context, int64, []string) (map[string]int64, error)
	SymbolFTSGenerationRows(context.Context, int64, string, int64, int) ([]storage.SymbolFTSRow, int64, error)
	SymbolFTSGenerationStats(context.Context, int64) (storage.SymbolFTSStats, error)
	TakeFreshSymbolFTSRows(int64) ([]storage.SymbolFTSRow, bool)
	TokenizeSymbolFTS(context.Context, []string) ([][]string, error)
	SharesSymbolSearchCore(any) bool
	IsNilGenerationHandle() bool
}

type checkoutCatalog interface {
	IsNilMaterializerCatalog() bool
	GetCheckout(context.Context, string) (storage.Checkout, bool, error)
	GetCheckoutRoute(context.Context, string) (storage.CheckoutRoute, bool, error)
	GetDedicatedGraph(context.Context, string) (storage.DedicatedGraph, bool, error)
	ListDedicatedGraphs(context.Context, string) ([]storage.DedicatedGraph, error)
	CheckoutsByRootPaths(context.Context, []string) ([]storage.Checkout, error)
	ListRepositoryFamilies(context.Context) ([]storage.RepositoryFamily, error)
	ListCheckouts(context.Context, string) ([]storage.Checkout, error)
}

func (s *Server) checkoutCatalog() checkoutCatalog {
	if s == nil || s.materializer == nil {
		return nil
	}
	catalog, ok := s.materializer.Catalog.(checkoutCatalog)
	if !ok || catalog.IsNilMaterializerCatalog() {
		return nil
	}
	return catalog
}

type checkoutRecordReader interface {
	GetCheckout(context.Context, string) (storage.Checkout, bool, error)
}

type checkoutRouteReader interface {
	GetCheckoutRoute(context.Context, string) (storage.CheckoutRoute, bool, error)
}

type producerWithdrawer interface {
	WithdrawProducer(context.Context, int64, string, string) error
	IsNilGenerationHandle() bool
}

type selectedMaskReader interface {
	FileMasksForContext(context.Context, string, []string) ([]storage.FileMask, error)
}
