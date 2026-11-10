package store_sqlite

import (
	"encoding/binary"

	"github.com/zzet/gortex/internal/storage"
)

// Per-edit write-ahead-log accounting.
//
// An edit's durable cost is the WAL it appends: every page a write
// transaction dirties becomes one frame, and until a checkpoint copies the
// frames back and a reset rewinds the log, those frames are what a pinned
// reader keeps alive. WALWriteMark samples the log position and
// WALWrittenBetween turns two samples into the frames and bytes appended in
// between, so a build or edit report can carry the WAL it cost.
//
// The position comes from the shared wal-index (-shm) header, read without a
// lock (see readWALIndexSnapshot): mxFrame is the number of valid frames in
// the log and the header's salt pair changes every time the log is reset. A
// reset between two marks means the frames written before it are no longer
// countable; the delta then reports only the frames since the reset (a lower
// bound) and says so. Frames written by any connection of the database count,
// so a concurrent writer's frames land in the same delta: the figure is "WAL
// appended while this edit ran", which is exactly the log growth a reader
// pinned across the edit would hold.

type WALWriteMark = storage.WALWriteMark

type WALWriteDelta = storage.WALWriteDelta

// walFrameHeaderBytes is the fixed header in front of every WAL frame.
const walFrameHeaderBytes = storage.WALFrameHeaderBytes

// WALWriteMark samples the store's write-ahead-log position. It costs one
// small file read and never blocks a writer or a reader.
func (s *Store) WALWriteMark() WALWriteMark {
	if s == nil || s.storeCore == nil || s.dbPath == "" {
		return WALWriteMark{}
	}
	return readWALWriteMark(s.dbPath)
}

// readWALWriteMark reads the first copy of the wal-index header of dbPath:
// szPage (u16, 1 meaning 65536) at offset 14, mxFrame (u32) at 16 and the
// salt pair (copied verbatim from the WAL header) at 32, in the byte order the
// header was written in by this machine.
func readWALWriteMark(dbPath string) WALWriteMark {
	// Through the process's kept descriptor: closing a descriptor of the -shm
	// would release this process's SQLite locks on it (wal_index_file.go).
	var hdr [48]byte
	if err := readWALIndexHeader(dbPath, hdr[:]); err != nil {
		return WALWriteMark{}
	}
	mark := WALWriteMark{Valid: true, MxFrame: binary.NativeEndian.Uint32(hdr[16:20])}
	copy(mark.Salt[:], hdr[32:40])
	switch size := binary.NativeEndian.Uint16(hdr[14:16]); size {
	case 0:
		mark.PageSize = 0
	case 1:
		mark.PageSize = 65536
	default:
		mark.PageSize = uint32(size)
	}
	return mark
}

// WALWrittenBetween is the log growth from before to after.
func WALWrittenBetween(before, after WALWriteMark) WALWriteDelta {
	return storage.WALWrittenBetween(before, after)
}
