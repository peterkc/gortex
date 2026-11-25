package indexer

import (
	"context"
	"time"

	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/storage"
)

// contractCoreStore applies receipts within the selected physical namespace.
type contractCoreStore interface {
	PendingContractWorkForScopeContext(ctx context.Context, repo, checkout string) ([]graph.ContractWork, error)
	AcceptContractBoundaryReceiptsContext(ctx context.Context, expected []graph.ContractBoundaryReceipt) error
	AcceptContractInputMutationWithReceiptsContext(ctx context.Context, expected graph.ContractInputState, receipts []graph.ContractBoundaryReceipt) error
	BeginContractInputMutationContext(ctx context.Context, expected *graph.ContractInputState, next graph.ContractInputState, work []graph.ContractWork) error
	BeginContractInputMutationWithReceiptsContext(ctx context.Context, expected *graph.ContractInputState, next graph.ContractInputState, work []graph.ContractWork, receipts []graph.ContractBoundaryReceipt) error
	ContractBoundaryReceiptForVersionContext(ctx context.Context, repo, actor, file, version string) (*graph.ContractBoundaryReceipt, bool, error)
	ContractBoundaryReceiptsForLookupKeysContext(ctx context.Context, keys []string, limit int) ([]graph.ContractBoundaryReceipt, error)
	ContractBoundaryReceiptsForPathsContext(ctx context.Context, repo, actor string, paths []string) (map[string]*graph.ContractBoundaryReceipt, error)
	ContractCoreWindowGaps(repo string, paths []string, since time.Time) []storage.ContractCoreWindowGap
	ContractInputStateContext(ctx context.Context, repo, checkout string) (graph.ContractInputState, bool, error)
	ContractReceiptWriteSeqs(repo, checkout string, paths []string) map[string]uint64
	EndContractCoreWrites(repo, checkout string, seqs map[string]uint64)
	OrphanContractCoreWrites(repo, checkout string, seqs map[string]uint64)
	RestartContractFullRootCensusContext(ctx context.Context, expected, next graph.ContractInputState, work []graph.ContractWork) error
	SetContractBoundaryReceiptBaselineContext(ctx context.Context, row graph.ContractBoundaryReceiptBaseline) error
	SetContractBoundaryReceiptsContext(ctx context.Context, rows []graph.ContractBoundaryReceipt) error
	SetContractBoundaryReceiptsWithSourcesContext(ctx context.Context, rows []graph.ContractBoundaryReceipt, sources []graph.ContractBoundaryReceiptSource) error
	SetContractInputStateWithWorkContext(ctx context.Context, expected *graph.ContractInputState, next graph.ContractInputState, work []graph.ContractWork) error
	SetContractInputStateWithWorkReceiptsAndSourcesContext(ctx context.Context, expected *graph.ContractInputState, next graph.ContractInputState, work []graph.ContractWork, receipts []graph.ContractBoundaryReceipt, sources []graph.ContractBoundaryReceiptSource) error
	SettleContractReceiptWrites(repo, checkout string, seqs map[string]uint64)
	ViewGeneration() int64
}

type primaryContractCoreStore interface {
	contractCoreStore
	SharesSymbolSearchCore(any) bool
}
