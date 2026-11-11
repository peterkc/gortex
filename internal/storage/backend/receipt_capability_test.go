package backend_test

import (
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/graph/store_sqlite"
)

// SQLite is the daemon's store. If it loses the indexer's optional fan-out
// receipt methods, every shipped mutation silently falls back to the store-wide
// axis. Fail to compile the backend tests instead.
var _ interface {
	BeginMutationReceiptOwningFanout() graph.MutationReceiptToken
	RecordMutationFanoutTruncationIn(graph.MutationReceiptToken, graph.ReceiptFanoutTruncation)
} = (*store_sqlite.Store)(nil)
