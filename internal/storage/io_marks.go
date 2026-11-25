package storage

import "time"

// ReaderWaitMark is a monotonic, per-process sample of the time the store's
// readers spent waiting before they could read: held at the read gate while
// the WAL reclaim or a checkpoint paused new reads (GateNanos, GateWaits), and
// blocked on the read pool for a free connection (PoolNanos, PoolWaits). Two
// marks bracket a step the way WALWriteMark does for the bytes a step wrote:
// Since gives the readers' wait inside it, attributed to every reader in the
// process, not only the step's own.
//
// The mark also carries what Since does not add up, so a lap whose wall time
// is far above its CPU can be attributed:
//   - WriterPoolNanos/Waits: waits for the writer pool's one connection;
//   - Sleep: time asleep through SQLite's VFS (process-wide), split into the
//     native busy handler and the WAL read-lock retry loop. The driver's Go
//     busy handler is not observable here (store_sqlite/sqlite_sleep_gauge.go);
//   - ReadTxnNanos/ReadTxns: the wall time of the pool's read transactions
//     (a connection's first statement to its return to the pool), which is
//     the store's share of a read-heavy lap.
type ReaderWaitMark struct {
	GateNanos int64 `json:"gate_nanos"`
	GateWaits int64 `json:"gate_waits"`
	PoolNanos int64 `json:"pool_nanos"`
	PoolWaits int64 `json:"pool_waits"`

	WriterPoolNanos int64           `json:"writer_pool_nanos"`
	WriterPoolWaits int64           `json:"writer_pool_waits"`
	Sleep           SQLiteSleepMark `json:"sleep"`
	ReadTxnNanos    int64           `json:"read_txn_nanos"`
	ReadTxns        int64           `json:"read_txns"`
	// Page reads of the pool's read transactions (store_sqlite/sqlite_read_gate.go):
	// found in the connection's page cache, read with a pread (from the log
	// or the database file), and dirty pages spilled; MappedPages are pages
	// served from the memory map instead, process-wide (every connection),
	// which the cache counters never see.
	CacheHits   int64 `json:"cache_hits"`
	CacheMisses int64 `json:"cache_misses"`
	CacheSpills int64 `json:"cache_spills"`
	MappedPages int64 `json:"mapped_pages"`
	// The writer connection's page cache, read at the end of every hold of
	// the write gate (store_sqlite/writer_cache_counters.go). A writer spill means a
	// transaction's dirty pages outgrew the cache and went to the log early.
	WriterCacheHits   int64 `json:"writer_cache_hits"`
	WriterCacheMisses int64 `json:"writer_cache_misses"`
	WriterCacheSpills int64 `json:"writer_cache_spills"`
	// WriterCacheSkipped counts releases whose sample was skipped (a holder
	// still had the connection); their pages land in a later sample.
	WriterCacheSkipped int64 `json:"writer_cache_skipped"`
	// VFS is the page I/O timed at the VFS (store_sqlite/vfs_io_counters.go),
	// process-wide.
	VFS VFSIOMark `json:"vfs"`
}

// Since is the reader wait accumulated between prev and m: the read gate and
// the read pool, as before.
func (m ReaderWaitMark) Since(prev ReaderWaitMark) time.Duration {
	return time.Duration((m.GateNanos - prev.GateNanos) + (m.PoolNanos - prev.PoolNanos))
}

// Split is the lap between prev and m, by kind of wait.
func (m ReaderWaitMark) Split(prev ReaderWaitMark) ReaderWaitSplit {
	return ReaderWaitSplit{
		Gate:          time.Duration(m.GateNanos - prev.GateNanos),
		ReadPool:      time.Duration(m.PoolNanos - prev.PoolNanos),
		WriterPool:    time.Duration(m.WriterPoolNanos - prev.WriterPoolNanos),
		BusySleep:     time.Duration(m.Sleep.BusyNanos - prev.Sleep.BusyNanos),
		WALRetrySleep: time.Duration(m.Sleep.WALRetryNanos - prev.Sleep.WALRetryNanos),
		BusySleeps:    m.Sleep.BusyCalls - prev.Sleep.BusyCalls,
		WALRetries:    m.Sleep.WALRetryCalls - prev.Sleep.WALRetryCalls,
		ReadTxn:       time.Duration(m.ReadTxnNanos - prev.ReadTxnNanos),
		ReadTxns:      m.ReadTxns - prev.ReadTxns,
		CacheHits:     m.CacheHits - prev.CacheHits,
		CacheMisses:   m.CacheMisses - prev.CacheMisses,
		CacheSpills:   m.CacheSpills - prev.CacheSpills,
		MappedPages:   m.MappedPages - prev.MappedPages,

		WriterCacheHits:    m.WriterCacheHits - prev.WriterCacheHits,
		WriterCacheMisses:  m.WriterCacheMisses - prev.WriterCacheMisses,
		WriterCacheSpills:  m.WriterCacheSpills - prev.WriterCacheSpills,
		WriterCacheSkipped: m.WriterCacheSkipped - prev.WriterCacheSkipped,
		VFS:                m.VFS.Since(prev.VFS),
	}
}

const (
	VFSFileOther = iota
	VFSFileMain
	VFSFileWAL
)

// VFSIOMark is a sample of the page I/O counters, process-wide.
type VFSIOMark struct {
	// Available identifies physical VFS counters. FallbackWAL is sampled only
	// by a store-backed mark when those hooks are unavailable.
	Available   bool
	FallbackWAL WALWriteMark `json:"-"`
	// [file][writer]: file 0 other, 1 main database, 2 log; writer 1 is the
	// writer connection.
	ReadCalls, ReadNanos, ReadBytes    [3][2]int64
	WriteCalls, WriteNanos, WriteBytes [3][2]int64
	Rows                               RowWriteCounts
	Reindex                            ReindexWriteCounts
	Transactions                       int64
}

// Since is the lap between prev and m.
func (m VFSIOMark) Since(prev VFSIOMark) VFSIOSplit {
	d := func(a, b [3][2]int64, f, w int) int64 { return a[f][w] - b[f][w] }
	split := VFSIOSplit{
		OtherWALWriteBytes:   d(m.WriteBytes, prev.WriteBytes, VFSFileWAL, 0),
		Rows:                 m.Rows.Since(prev.Rows),
		Reindex:              m.Reindex.Since(prev.Reindex),
		Transactions:         m.Transactions - prev.Transactions,
		WriterMainWrites:     d(m.WriteCalls, prev.WriteCalls, VFSFileMain, 1),
		WriterMainWriteTime:  time.Duration(d(m.WriteNanos, prev.WriteNanos, VFSFileMain, 1)),
		WriterMainWriteBytes: d(m.WriteBytes, prev.WriteBytes, VFSFileMain, 1),
		WriterMainReads:      d(m.ReadCalls, prev.ReadCalls, VFSFileMain, 1),
		WriterWALReads:       d(m.ReadCalls, prev.ReadCalls, VFSFileWAL, 1),
		OtherMainReads:       d(m.ReadCalls, prev.ReadCalls, VFSFileMain, 0),
		OtherWALReads:        d(m.ReadCalls, prev.ReadCalls, VFSFileWAL, 0),
		WriterMainReadTime:   time.Duration(d(m.ReadNanos, prev.ReadNanos, VFSFileMain, 1)),
		WriterWALReadTime:    time.Duration(d(m.ReadNanos, prev.ReadNanos, VFSFileWAL, 1)),
		OtherMainReadTime:    time.Duration(d(m.ReadNanos, prev.ReadNanos, VFSFileMain, 0)),
		OtherWALReadTime:     time.Duration(d(m.ReadNanos, prev.ReadNanos, VFSFileWAL, 0)),
		WriterWALWrites:      d(m.WriteCalls, prev.WriteCalls, VFSFileWAL, 1),
		OtherMainWrites:      d(m.WriteCalls, prev.WriteCalls, VFSFileMain, 0),
		WriterWALWriteTime:   time.Duration(d(m.WriteNanos, prev.WriteNanos, VFSFileWAL, 1)),
		OtherMainWriteTime:   time.Duration(d(m.WriteNanos, prev.WriteNanos, VFSFileMain, 0)),
		WriterWALWriteBytes:  d(m.WriteBytes, prev.WriteBytes, VFSFileWAL, 1),
		OtherMainWriteBytes:  d(m.WriteBytes, prev.WriteBytes, VFSFileMain, 0),
	}
	if !m.Available {
		wal := WALWrittenBetween(prev.FallbackWAL, m.FallbackWAL)
		if wal.Valid {
			split.WriterWALWriteBytes = wal.Bytes
			split.WALReset = wal.Reset
		}
	}
	return split
}

// WALWriteDelta is the log growth between two marks.
type WALWriteDelta struct {
	// Valid is false when either mark was unreadable.
	Valid bool `json:"valid"`
	// Frames is the number of frames appended between the marks.
	Frames int64 `json:"frames"`
	// Bytes is Frames times the on-disk frame size (page plus the 24-byte
	// frame header).
	Bytes int64 `json:"bytes"`
	// Reset reports that the log was reset between the marks; Frames and
	// Bytes then count only the frames since the reset.
	Reset bool `json:"reset,omitempty"`
}

// WALFrameHeaderBytes is the fixed header in front of every WAL frame.
const WALFrameHeaderBytes = 24

// WALWrittenBetween is the log growth from before to after.
func WALWrittenBetween(before, after WALWriteMark) WALWriteDelta {
	if !before.Valid || !after.Valid {
		return WALWriteDelta{}
	}
	delta := WALWriteDelta{Valid: true}
	switch {
	case before.Salt == after.Salt && after.MxFrame >= before.MxFrame:
		delta.Frames = int64(after.MxFrame - before.MxFrame)
	default:
		// The log was reset (the salt moved, or mxFrame went backwards on an
		// uninitialised header): only the frames since the reset are known.
		delta.Reset = true
		delta.Frames = int64(after.MxFrame)
	}
	pageSize := after.PageSize
	if pageSize == 0 {
		pageSize = before.PageSize
	}
	if pageSize > 0 {
		delta.Bytes = delta.Frames * int64(pageSize+WALFrameHeaderBytes)
	}
	return delta
}
