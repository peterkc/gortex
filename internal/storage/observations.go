package storage

import "time"

// StorageFailure is one payload generation's last storage-layer maintenance
// failure: the generation it happened to, and the bounded, path-free sentence
// SafeStorageFailureReason renders for it.
//
// It carries no error, no path and no SQL. A readiness payload is read by a
// person and shipped off a machine, and the full cause — which is kept on the
// *StorageError the failing call returned, and logged by the caller — belongs
// in the daemon log rather than in a status poll.
type StorageFailure struct {
	GenerationID int64  `json:"generation_id"`
	Reason       string `json:"reason"`
}

// WALPassiveStats describes the real periodic PASSIVE producer since open.
// Refusals are not admitted Attempts. FramesBackfilledTotal is an observed
// prefix-advance lower bound; reset/unreadable observations are counted
// separately instead of being invented as zero-progress checkpoints.
type WALPassiveStats struct {
	Attempts, Complete, Incomplete, ZeroProgress     int64
	DeferredBuildLane                                int64
	LongestBuildLaneDeferral                         time.Duration
	RefusedLease, RefusedInflight, StoodAsideReclaim int64
	YieldCancelled, Failed                           int64
	FramesBackfilledTotal, FramesUnobserved          int64
	LastOKAt                                         time.Time
	LastOutcome                                      string
}

// RowWriteCounts counts directly changed rows in the four graph payload tables.
// Statement RowsAffected excludes trigger writes (including generation census
// triggers). Like VFS counters, these are process-wide activity, not committed
// cardinality: successful statements in a later rolled-back transaction count.
// INSERT ... RETURNING helpers add their returned-row census once per statement.
type RowWriteCounts struct {
	Nodes      int64 `json:"nodes"`
	Edges      int64 `json:"edges"`
	SymbolFTS  int64 `json:"symbol_fts"`
	ContentFTS int64 `json:"content_fts"`
}

// ReindexWriteCounts is committed set-reindex work over an interval. Generic
// simulation is reported separately from the receipt-safety fallback so a log
// never calls all delete/insert work an in-place conversion.
type ReindexWriteCounts struct {
	Updated, Deleted, Inserted         int64
	FastPath, Generic, ReceiptFallback int64
}

// VFSIOSplit is one lap's page I/O: reads and writes of the main database file
// and of the log, by the writer connection and by every other connection.
type VFSIOSplit struct {
	OtherWALWriteBytes                                             int64
	WALReset                                                       bool
	WriterMainReads, WriterWALReads, OtherMainReads, OtherWALReads int64
	WriterMainReadTime, WriterWALReadTime                          time.Duration
	OtherMainReadTime, OtherWALReadTime                            time.Duration
	WriterMainWrites, WriterWALWrites, OtherMainWrites             int64
	WriterMainWriteTime, WriterWALWriteTime, OtherMainWriteTime    time.Duration
	WriterMainWriteBytes, WriterWALWriteBytes, OtherMainWriteBytes int64
	Rows                                                           RowWriteCounts
	Reindex                                                        ReindexWriteCounts
	Transactions                                                   int64
}

// ReaderWaitSplit is one lap's store waits, each on its own.
type ReaderWaitSplit struct {
	Gate, ReadPool, WriterPool time.Duration
	BusySleep, WALRetrySleep   time.Duration
	BusySleeps, WALRetries     int64
	ReadTxn                    time.Duration
	ReadTxns                   int64
	CacheHits, CacheMisses     int64
	CacheSpills, MappedPages   int64
	WriterCacheHits            int64
	WriterCacheMisses          int64
	WriterCacheSpills          int64
	WriterCacheSkipped         int64
	VFS                        VFSIOSplit
}

// SQLiteSleepMark samples process-wide VFS sleeps. Busy counters exclude the
// driver's cancellable Go busy handler; an installed gauge can report zero
// even while that handler waits for a lock.
type SQLiteSleepMark struct {
	Installed     bool  `json:"installed"`
	VFSOnly       bool  `json:"vfs_only"`
	BusyNanos     int64 `json:"busy_nanos"`
	BusyCalls     int64 `json:"busy_calls"`
	WALRetryNanos int64 `json:"wal_retry_nanos"`
	WALRetryCalls int64 `json:"wal_retry_calls"`
	MaxNanos      int64 `json:"max_nanos"`
}

// WALWriteMark is one sample of the log position.
type WALWriteMark struct {
	// Valid is false when the store has no readable wal-index (an in-memory
	// database, a closed store, a store not in WAL mode yet).
	Valid bool
	// MxFrame is the number of valid frames in the log.
	MxFrame uint32
	// Salt identifies the log incarnation; a reset changes it.
	Salt [8]byte
	// PageSize is the database page size recorded in the wal-index header.
	PageSize uint32
}

func (r RowWriteCounts) Total() int64 { return r.Nodes + r.Edges + r.SymbolFTS + r.ContentFTS }

func (r RowWriteCounts) Since(p RowWriteCounts) RowWriteCounts {
	return RowWriteCounts{r.Nodes - p.Nodes, r.Edges - p.Edges, r.SymbolFTS - p.SymbolFTS, r.ContentFTS - p.ContentFTS}
}

func (r ReindexWriteCounts) Since(p ReindexWriteCounts) ReindexWriteCounts {
	return ReindexWriteCounts{r.Updated - p.Updated, r.Deleted - p.Deleted, r.Inserted - p.Inserted, r.FastPath - p.FastPath, r.Generic - p.Generic, r.ReceiptFallback - p.ReceiptFallback}
}

// WALBytes includes all connections: writer TLS classification is best-effort
// when more than one store has a writer in the same process.
func (d VFSIOSplit) WALBytes() int64 { return d.WriterWALWriteBytes + d.OtherWALWriteBytes }
