package store_pebble

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"maps"
	"slices"

	"github.com/cockroachdb/pebble/v2"
	"github.com/zzet/gortex/internal/graph"
)

var (
	// ErrRebuildContractOverlay forbids sealing with an active or changed overlay.
	ErrRebuildContractOverlay = errors.New("rebuild contract overlay failed")

	rebuildContractUndoPrefix = []byte{'g', '~'}
)

// BeginRebuildContractOverlay makes the early identities visible to all
// private framework readers. It journals the exact displaced node/index
// bytes in scratch. No contract ownership or shape rows are written.
func (b *BuildStore) BeginRebuildContractOverlay(ctx context.Context, ids map[string]string) error {
	return b.withWrites(func() error {
		if ids == nil || b.contractOverlay || b.contractsStarted {
			return fmt.Errorf("%w: missing ids, active overlay, or started contracts stage", ErrRebuildContractOverlay)
		}
		occupied, err := rebuildGuardHasPrefix(ctx, b.pending, rebuildContractUndoPrefix)
		if err != nil {
			return err
		}
		if occupied {
			return fmt.Errorf("%w: undo range occupied", ErrRebuildContractOverlay)
		}
		b.contractOverlay = true
		for _, id := range slices.Sorted(maps.Keys(ids)) {
			if err := ctx.Err(); err != nil {
				return err
			}
			if id == "" || graph.IsProxyID(id) {
				return fmt.Errorf("%w: invalid contract id %q", ErrRebuildContractOverlay, id)
			}
			if err := b.overlayRebuildContract(id, ids[id]); err != nil {
				return err
			}
		}
		return ctx.Err()
	})
}

// overlayValue includes a presence byte, so absence and an empty saved value
// are different both in the journal and in its comparison digest.
func overlayValue(reader pebble.Reader, key []byte) ([]byte, error) {
	return pendingGet(reader, key, func(raw []byte) ([]byte, error) {
		return append([]byte{1}, raw...), nil
	})
}

func (b *BuildStore) overlayRebuildContract(id, file string) error {
	prev, err := b.pendingNode(id)
	if err != nil {
		return err
	}
	if prev != nil && prev.ID != id {
		return fmt.Errorf("%w: displaced node identity %q", ErrRebuildContractOverlay, id)
	}
	// The B2 scope is empty. Native names the contract by the full ID and
	// assigns the elected record's file path. It has no callable/role fields.
	n := &graph.Node{ID: id, Kind: graph.KindContract, Name: id, FilePath: file, Language: "contract"}
	value, err := encodeNode(n)
	if err != nil {
		return err
	}
	batch := b.pending.NewBatch()
	defer batch.Close()
	journal := rebuildContractOverlayBatch{reader: b.pending, batch: batch}
	if err := bufferNodeRecord(journal, n, prev, value); err != nil {
		return err
	}
	if err := batch.Commit(pebble.NoSync); err != nil {
		return err
	}
	b.v3NodeWrites++
	return nil
}

type rebuildContractOverlayBatch struct {
	reader pebble.Reader
	batch  *pebble.Batch
}

func (b rebuildContractOverlayBatch) Set(key, value []byte, opts *pebble.WriteOptions) error {
	if err := b.remember(key, append([]byte{1}, value...)); err != nil {
		return err
	}
	return b.batch.Set(key, value, opts)
}

func (b rebuildContractOverlayBatch) Delete(key []byte, opts *pebble.WriteOptions) error {
	if err := b.remember(key, nil); err != nil {
		return err
	}
	return b.batch.Delete(key, opts)
}

func (b rebuildContractOverlayBatch) remember(key, next []byte) error {
	old, err := overlayValue(b.reader, key)
	if err != nil {
		return err
	}
	digest := sha256.Sum256(next)
	undoKey := append(bytes.Clone(rebuildContractUndoPrefix), key...)
	return b.batch.Set(undoKey, append(digest[:], old...), nil)
}

// EndRebuildContractOverlay first verifies every touched key, including
// deleted indexes. It never restores over an intervening stage mutation.
// Restore uses the journal bytes, not a re-encoded displaced graph node.
func (b *BuildStore) EndRebuildContractOverlay(ctx context.Context) error {
	return b.withWrites(func() error {
		if !b.contractOverlay {
			return fmt.Errorf("%w: no active overlay", ErrRebuildContractOverlay)
		}
		prefix := rebuildContractUndoPrefix
		if err := pendingVisit(ctx, b.pending, prefix, func(r row) error {
			key := r.key[len(prefix):]
			if len(key) == 0 || len(r.value) < sha256.Size ||
				(len(r.value) > sha256.Size && r.value[sha256.Size] != 1) {
				return fmt.Errorf("%w: invalid undo row", ErrRebuildContractOverlay)
			}
			live, err := overlayValue(b.pending, key)
			if err != nil {
				return err
			}
			digest := sha256.Sum256(live)
			if !bytes.Equal(digest[:], r.value[:sha256.Size]) {
				return fmt.Errorf("%w: overlaid key changed %x", ErrRebuildContractOverlay, key)
			}
			return nil
		}); err != nil {
			return err
		}
		if err := pendingVisit(ctx, b.pending, prefix, func(r row) error {
			key := r.key[len(prefix):]
			old := r.value[sha256.Size:]
			if len(old) == 0 {
				return b.pending.Delete(key, pebble.NoSync)
			}
			return b.pending.Set(key, old[1:], pebble.NoSync)
		}); err != nil {
			return err
		}
		if err := b.pending.DeleteRange(prefix, prefixEnd(prefix), pebble.NoSync); err != nil {
			return err
		}
		b.contractOverlay = false
		b.v3NodeWrites++
		return ctx.Err()
	})
}
