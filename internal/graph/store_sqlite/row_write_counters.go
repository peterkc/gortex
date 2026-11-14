package store_sqlite

import (
	"database/sql"
	"sync/atomic"

	"github.com/zzet/gortex/internal/storage"
)

type RowWriteCounts = storage.RowWriteCounts

var nodeRowsWritten, edgeRowsWritten, symbolFTSRowsWritten, contentFTSRowsWritten atomic.Int64

func rowWriteMark() RowWriteCounts {
	return RowWriteCounts{Nodes: nodeRowsWritten.Load(), Edges: edgeRowsWritten.Load(), SymbolFTS: symbolFTSRowsWritten.Load(), ContentFTS: contentFTSRowsWritten.Load()}
}
func countStatementRows(counter *atomic.Int64, result sql.Result, err error) {
	if err == nil && result != nil {
		if rows, rowsErr := result.RowsAffected(); rowsErr == nil && rows > 0 {
			counter.Add(rows)
		}
	}
}
func countNodeWrite(result sql.Result, err error) (sql.Result, error) {
	countStatementRows(&nodeRowsWritten, result, err)
	return result, err
}
func countEdgeWrite(result sql.Result, err error) (sql.Result, error) {
	countStatementRows(&edgeRowsWritten, result, err)
	return result, err
}
func countSymbolFTSWrite(result sql.Result, err error) (sql.Result, error) {
	countStatementRows(&symbolFTSRowsWritten, result, err)
	return result, err
}
func countContentFTSWrite(result sql.Result, err error) (sql.Result, error) {
	countStatementRows(&contentFTSRowsWritten, result, err)
	return result, err
}

type ReindexWriteCounts = storage.ReindexWriteCounts

var reindexUpdated, reindexDeleted, reindexInserted, reindexFast, reindexGeneric, reindexReceipt atomic.Int64

func reindexWriteMark() ReindexWriteCounts {
	return ReindexWriteCounts{Updated: reindexUpdated.Load(), Deleted: reindexDeleted.Load(), Inserted: reindexInserted.Load(), FastPath: reindexFast.Load(), Generic: reindexGeneric.Load(), ReceiptFallback: reindexReceipt.Load()}
}
func noteReindexWrites(s sqliteReindexSetStats) {
	reindexUpdated.Add(int64(s.updatedRows))
	reindexDeleted.Add(int64(s.deletedRows))
	reindexInserted.Add(int64(s.insertedRows))
	reindexFast.Add(int64(s.fastPath))
	reindexGeneric.Add(int64(s.genericFallback))
	reindexReceipt.Add(int64(s.receiptFallback))
}

// noteTableRows is for existing helpers whose payload table is a parameter.
// Sidecars, FTS ownership maps and catalog tables intentionally contribute zero.
func noteTableRows(table string, result sql.Result, err error) {
	switch table {
	case "nodes":
		countStatementRows(&nodeRowsWritten, result, err)
	case "edges":
		countStatementRows(&edgeRowsWritten, result, err)
	case "symbol_fts":
		countStatementRows(&symbolFTSRowsWritten, result, err)
	case "content_fts":
		countStatementRows(&contentFTSRowsWritten, result, err)
	}
}
