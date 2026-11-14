package graphview

import (
	"context"

	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/storage"
)

// contractPhysicalSource identifies a plain handle on an open physical core.
type contractPhysicalSource interface {
	SharesSymbolSearchCore(other any) bool
	ViewGeneration() int64
}

// contractRegistrySource supplies the clocks for the analysis layer's core.
type contractRegistrySource interface {
	contractPhysicalSource
	PayloadInputRevisionContext(context.Context) (storage.PayloadInputRevision, error)
	GenerationCorrectionEpoch(int64) uint64
}

type contractInputHandle interface {
	ViewGeneration() int64
	ContractInputStateContext(context.Context, string, string) (graph.ContractInputState, bool, error)
}

type contractReadinessHandle interface {
	ContractWorkContext(context.Context) ([]graph.ContractWork, error)
	ContractWorkForScopeContext(context.Context, string, string) ([]graph.ContractWork, error)
	GetContractStateContext(context.Context, string) (graph.ContractState, bool, error)
	ProducerStates() ([]storage.ProducerCompleteness, error)
}
