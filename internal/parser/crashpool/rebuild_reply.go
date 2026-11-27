package crashpool

import (
	"encoding/binary"
	"fmt"
	"io"
	"math"
)

// A strict reply carries opaque entry bytes, not graph rows. Read the fixed
// prefix before allocating any body so gob cannot allocate from a child count.
// Meta is the entry header when EntrySize is nonzero, otherwise a diagnostic
// truncated to fit MetaSize.
type rebuildReplyPrefix struct {
	Seq, EntrySize uint64
	MetaSize       uint16
}

func writeRebuildReply(w io.Writer, seq uint64, header, entry []byte, failure error) error {
	meta := header
	if failure != nil {
		text := failure.Error()
		meta, entry = []byte(text[:min(len(text), math.MaxUint16)]), nil
	}
	prefix := rebuildReplyPrefix{Seq: seq, EntrySize: uint64(len(entry)), MetaSize: uint16(len(meta))}
	if err := binary.Write(w, binary.BigEndian, prefix); err != nil {
		return err
	}
	if _, err := w.Write(meta); err != nil {
		return err
	}
	if len(entry) == 0 {
		return nil
	}
	_, err := w.Write(entry)
	return err
}

func readRebuildReply(r io.Reader, seq uint64, entryLimit int, resp *extractResponse) error {
	var prefix rebuildReplyPrefix
	if err := binary.Read(r, binary.BigEndian, &prefix); err != nil {
		return err
	}
	if prefix.Seq != seq {
		return fmt.Errorf("crashpool: response seq %d != request seq %d", prefix.Seq, seq)
	}
	if prefix.EntrySize > uint64(max(entryLimit, 0)) {
		return fmt.Errorf("crashpool: rebuild entry has %d bytes, limit %d", prefix.EntrySize, entryLimit)
	}
	meta := make([]byte, int(prefix.MetaSize))
	if _, err := io.ReadFull(r, meta); err != nil {
		return err
	}
	resp.Seq = seq
	if prefix.EntrySize == 0 {
		resp.Err = string(meta)
		return nil
	}
	entry := make([]byte, int(prefix.EntrySize))
	if _, err := io.ReadFull(r, entry); err != nil {
		return err
	}
	resp.RebuildHeader, resp.RebuildEntry = meta, entry
	return nil
}
