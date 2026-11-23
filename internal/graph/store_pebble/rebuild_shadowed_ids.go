package store_pebble

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"strings"

	"github.com/cockroachdb/pebble/v2"
)

// RebuildShadowedIDStats describes a complete receipt census. ScratchBytes
// counts encoded keys and values written, including updates. SpillBytes is
// the pending DB's observed L0 flush volume during the census; it can include
// other pending families. PeakRowBytes accounts for the largest encoded write.
type RebuildShadowedIDStats struct {
	Shadowed, ScratchBytes, SpillBytes, PeakRowBytes uint64
}

// CountRebuildShadowedIDs counts IDs claimed by different receipts without
// keeping claimants in memory. Use the same nonzero ordinal for every claim in
// a receipt and a different ordinal for each other receipt. The producer must
// propagate sink errors. Only this pass owns the scratch range; no graph row
// changes, and no successful count is returned on capacity or cleanup failure.
func (b *BuildStore) CountRebuildShadowedIDs(ctx context.Context, maxScratchBytes uint64,
	claims func(func(id string, receipt uint64) error) error,
) (stats RebuildShadowedIDStats, err error) {
	prefix := []byte{'g', 'H'}
	occupied, err := rebuildGuardHasPrefix(ctx, b.pending, prefix)
	if err != nil {
		return stats, err
	}
	if occupied {
		return stats, invalid("shadowed ID scratch range occupied")
	}
	before := b.pending.Metrics().Levels[0].TableBytesFlushed
	defer func() {
		err = errors.Join(err, b.pending.DeleteRange(prefix, prefixEnd(prefix), pebble.NoSync))
		if err != nil {
			stats = RebuildShadowedIDStats{}
			return
		}
		stats.SpillBytes = b.pending.Metrics().Levels[0].TableBytesFlushed - before
	}()
	const maxRowBytes = 16 << 20
	if err := claims(func(id string, receipt uint64) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		if id == "" || receipt == 0 {
			return invalid("empty node ID or receipt ordinal")
		}
		// Account for embedded NUL escaping and the key terminator before
		// allocating the row. The value is one fixed-width receipt ordinal.
		rowBytes := uint64(len(prefix)+len(id)+strings.Count(id, "\x00")+2) + 8
		if rowBytes > maxRowBytes {
			return fmt.Errorf("%w: shadowed ID scratch row", ErrRebuildLinkInputSize)
		}
		key := make([]byte, len(prefix), int(rowBytes)-8)
		copy(key, prefix)
		key = appendKeyString(key, id)
		present := false
		first, err := pendingGet(b.pending, key, func(v []byte) (uint64, error) {
			if len(v) != 8 {
				return 0, invalid("shadowed ID receipt ordinal")
			}
			present = true
			return binary.BigEndian.Uint64(v), nil
		})
		if err != nil {
			return err
		}
		if present && (first == 0 || first == receipt) {
			return nil // already counted, or another claim from the same file
		}
		if rowBytes > maxScratchBytes-stats.ScratchBytes {
			return fmt.Errorf("%w: shadowed ID scratch exceeds %d bytes", ErrRebuildLinkInputSize, maxScratchBytes)
		}
		if present {
			receipt = 0 // counted; a third claimant cannot count again
		}
		var value [8]byte
		binary.BigEndian.PutUint64(value[:], receipt)
		if err := b.pending.Set(key, value[:], pebble.NoSync); err != nil {
			return err
		}
		stats.ScratchBytes += rowBytes
		stats.PeakRowBytes = max(stats.PeakRowBytes, rowBytes)
		if present {
			stats.Shadowed++
		}
		return nil
	}); err != nil {
		return stats, err
	}
	return stats, ctx.Err()
}
