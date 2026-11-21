package store_pebble

import (
	"bytes"
	"context"
	"encoding/binary"

	"github.com/cockroachdb/pebble/v2"
	"github.com/zzet/gortex/internal/graph"
)

var (
	_ graph.CloneShingleWriter = (*BuildStore)(nil)
	_ graph.CloneShingleReader = (*Snapshot)(nil)
)

const shingleRows byte = 1
const shingleOwners byte = 2 // Private reverse lookup only; never sealed.

func shingleKey(repo, id string) []byte {
	return keyStrings([]byte{familyFileMeta, shingleRows}, repo, id)
}

func (b *BuildStore) BulkSetCloneShingles(repo string, rows map[string][]uint64) error {
	return b.withWrites(func() error {
		for id, values := range rows {
			if id == "" {
				continue
			}
			value := make([]byte, 0, len(values)*8)
			for _, v := range values {
				value = binary.LittleEndian.AppendUint64(value, v)
			}
			if err := b.updateShingles(id, shingleKey(repo, id), value); err != nil {
				return err
			}
		}
		return nil
	})
}

func (b *BuildStore) DeleteCloneShingles(ids []string) error {
	return b.withWrites(func() error {
		for id := range uniqueNonempty(ids) {
			if err := b.updateShingles(id, nil, nil); err != nil {
				return err
			}
		}
		return nil
	})
}

// The reverse lookup makes an overwrite or delete by node ID independent
// of repository. It stays private; only repository rows reach the head.
func (b *BuildStore) updateShingles(id string, key, value []byte) error {
	owner := keyStrings([]byte{familyFileMeta, shingleOwners}, id)
	prev, err := pendingGet(b.pending, owner, copyValue)
	if err != nil || (key == nil && prev == nil) {
		return err
	}
	batch := b.pending.NewBatch()
	defer batch.Close()
	if prev != nil && !bytes.Equal(prev, key) {
		if err := batch.Delete(prev, nil); err != nil {
			return err
		}
	}
	if key == nil {
		if err := batch.Delete(owner, nil); err != nil {
			return err
		}
	} else {
		if err := batch.Set(owner, key, nil); err != nil {
			return err
		}
		if err := batch.Set(key, value, nil); err != nil {
			return err
		}
	}
	return batch.Commit(pebble.NoSync)
}

func (s *Snapshot) LoadCloneShingles(repo string) (map[string][]uint64, error) {
	prefix := keyStrings([]byte{familyFileMeta, shingleRows}, repo)
	out := make(map[string][]uint64)
	err := scan(context.Background(), s, prefix, func(key, raw []byte) (bool, error) {
		id, tail, err := readKeyString(key[len(prefix):])
		if err != nil || id == "" || len(tail) != 0 || len(raw)%8 != 0 {
			return false, invalid("invalid clone shingle row")
		}
		values := make([]uint64, len(raw)/8)
		for i := range values {
			values[i] = binary.LittleEndian.Uint64(raw[i*8:])
		}
		out[id] = values
		return true, nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

func checkShingleRow(r row) error {
	if len(r.key) < 2 || r.key[1] != shingleRows || len(r.value)%8 != 0 {
		return invalid("invalid clone shingle row")
	}
	_, rest, err := readKeyString(r.key[2:])
	if err != nil {
		return err
	}
	id, tail, err := readKeyString(rest)
	if err != nil || id == "" || len(tail) != 0 {
		return invalid("invalid clone shingle key")
	}
	return nil
}
