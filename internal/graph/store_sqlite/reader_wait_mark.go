package store_sqlite

import (
	"github.com/zzet/gortex/internal/storage"
)

type ReaderWaitMark = storage.ReaderWaitMark

// ReaderWaitMark samples the cumulative reader waits. Atomics and the pools'
// statistics: it never blocks.
func (s *Store) ReaderWaitMark() ReaderWaitMark {
	var m ReaderWaitMark
	if s == nil || s.storeCore == nil {
		return m
	}
	if g := s.readGate; g != nil {
		m.GateNanos = g.waitNanos.Load()
		m.GateWaits = g.waits.Load()
		m.ReadTxnNanos = g.readNanos.Load()
		m.ReadTxns = g.readTxns.Load()
		m.CacheHits = g.cacheHits.Load()
		m.CacheMisses = g.cacheMisses.Load()
		m.CacheSpills = g.cacheSpills.Load()
	}
	if s.db != nil {
		st := s.db.Stats()
		m.PoolNanos = int64(st.WaitDuration)
		m.PoolWaits = st.WaitCount
	}
	if s.writerDB != nil && s.writerDB != s.db {
		st := s.writerDB.Stats()
		m.WriterPoolNanos = int64(st.WaitDuration)
		m.WriterPoolWaits = st.WaitCount
	}
	m.Sleep = sqliteSleepMark()
	m.MappedPages = sqliteMappedPages.Load()
	m.WriterCacheHits = s.writerCache.hits.Load()
	m.WriterCacheMisses = s.writerCache.misses.Load()
	m.WriterCacheSpills = s.writerCache.spills.Load()
	m.WriterCacheSkipped = s.writerCache.skipped.Load()
	m.VFS = s.WriteMark()
	return m
}

type ReaderWaitSplit = storage.ReaderWaitSplit
