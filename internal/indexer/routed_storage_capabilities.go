package indexer

import (
	"context"

	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/storage"
)

type generationIdentity interface {
	ViewGeneration() int64
	SharesSymbolSearchCore(any) bool
}

type cooperativeGenerationCopier interface {
	SharesSymbolSearchCore(any) bool
	CopyPayloadGenerationCooperatively(context.Context, int64, int64, string) (GenerationCopyCounts, error)
}

type indexWriteObserver interface {
	SharesSymbolSearchCore(any) bool
	AnnounceWrite() func()
	WriteMark() storage.VFSIOMark
}

type shadowReplacementStore interface {
	SharesSymbolSearchCore(any) bool
	SupportsShadowReplacementRetirement() bool
	EvictRepoForShadowReplacement(context.Context, string) (int, int, error)
}

type warmupWALWaiter interface {
	SharesSymbolSearchCore(any) bool
	AwaitWarmupWAL(context.Context) error
}

type contractCopyBaselineReader interface {
	ContractInputStateContext(context.Context, string, string) (graph.ContractInputState, bool, error)
	ContractBoundaryReceiptBaselineContext(context.Context, string, string) (*graph.ContractBoundaryReceiptBaseline, error)
}

type generationStoreSource interface {
	AtGenerationStore(int64) graph.Store
}

type viewGenerationCatalog interface {
	GetViewGeneration(context.Context, int64) (storage.ViewGeneration, bool, error)
}

type graphBaseCatalog interface {
	viewGenerationCatalog
	GetCheckout(context.Context, string) (storage.Checkout, bool, error)
}

type dedicatedCohortCatalog interface {
	graphBaseCatalog
	GetDedicatedGraph(context.Context, string) (storage.DedicatedGraph, bool, error)
}

type generationWriteCounterStore interface {
	TakeGenerationWriteCounters(int64) (storage.GenerationWriteCounters, bool)
}
