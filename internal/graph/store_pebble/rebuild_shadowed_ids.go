package store_pebble

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"strings"

	"github.com/cockroachdb/pebble/v2"
	"github.com/zzet/gortex/internal/graph"
)

// RebuildShadowedIDStats describes a complete receipt census. ScratchBytes
// counts encoded keys and values written, including updates. SpillBytes is
// the pending DB's observed L0 flush volume during the census; it can include
// other pending families. PeakRowBytes accounts for the largest encoded write.
type RebuildShadowedIDStats struct {
	Shadowed, ShadowedEdges, ScratchBytes, SpillBytes, PeakRowBytes uint64
}

// CountRebuildShadowedIDs counts node IDs and complete edge identities
// claimed by different receipts without keeping claimants in memory. Use the
// same nonzero ordinal for every claim in a receipt and a different ordinal
// for each other receipt. The producer must
// propagate sink errors. Only this pass owns the scratch range; no graph row
// changes, and no successful count is returned on capacity or cleanup failure.
func (b *BuildStore) CountRebuildShadowedIDs(ctx context.Context, maxScratchBytes uint64,
	claims func(node func(string, uint64) error, edge func(graph.EdgeIdentity, uint64) error) error,
) (RebuildShadowedIDStats, error) {
	return b.censusRebuildClaims(ctx, maxScratchBytes, claims, nil)
}

const rebuildClaimRowBytes = 16 << 20

var rebuildClaimPrefix = []byte{'g', 'H'}

func (b *BuildStore) censusRebuildClaims(ctx context.Context, maxScratchBytes uint64,
	claims func(node func(string, uint64) error, edge func(graph.EdgeIdentity, uint64) error) error,
	finish func(*RebuildShadowedIDStats, func([]byte, []byte) error) error,
) (stats RebuildShadowedIDStats, err error) {
	prefix := rebuildClaimPrefix
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
	put := func(key, value []byte) error {
		rowBytes := uint64(len(key)) + uint64(len(value))
		if rowBytes > rebuildClaimRowBytes {
			return fmt.Errorf("%w: shadowed ID scratch row", ErrRebuildLinkInputSize)
		}
		if rowBytes > maxScratchBytes-stats.ScratchBytes {
			return fmt.Errorf("%w: shadowed ID scratch exceeds %d bytes", ErrRebuildLinkInputSize, maxScratchBytes)
		}
		if err := b.pending.Set(key, value, pebble.NoSync); err != nil {
			return err
		}
		stats.ScratchBytes += rowBytes
		stats.PeakRowBytes = max(stats.PeakRowBytes, rowBytes)
		return nil
	}
	remember := func(key []byte, receipt uint64, shadowed *uint64) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		if receipt == 0 {
			return invalid("empty shadowed ID receipt ordinal")
		}
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
		if present {
			receipt = 0 // counted; a third claimant cannot count again
		}
		var value [8]byte
		binary.BigEndian.PutUint64(value[:], receipt)
		if err := put(key, value[:]); err != nil {
			return err
		}
		if present {
			(*shadowed)++
		}
		return nil
	}
	claimNode := func(id string, receipt uint64) error {
		if id == "" {
			return invalid("empty shadowed node ID")
		}
		size := len(prefix) + rebuildShadowedKeyStringBytes(id)
		if size+8 > rebuildClaimRowBytes {
			return fmt.Errorf("%w: shadowed ID scratch row", ErrRebuildLinkInputSize)
		}
		key := make([]byte, len(prefix), size)
		copy(key, prefix)
		return remember(appendKeyString(key, id), receipt, &stats.Shadowed)
	}
	// An empty first string cannot be a node ID. Use it to separate the
	// two key types without another range or changing node scratch costs.
	edgePrefix := keyStrings([]byte{'g', 'H'}, "")
	claimEdge := func(edge graph.EdgeIdentity, receipt uint64) error {
		size := len(edgePrefix) + rebuildShadowedKeyStringBytes(edge.From) + rebuildShadowedKeyStringBytes(string(edge.Kind)) +
			rebuildShadowedKeyStringBytes(edge.To) + rebuildShadowedKeyStringBytes(edge.FilePath) + 8 // line
		if size+8 > rebuildClaimRowBytes {
			return fmt.Errorf("%w: shadowed edge scratch row", ErrRebuildLinkInputSize)
		}
		key := make([]byte, len(edgePrefix), size)
		copy(key, edgePrefix)
		key = appendKeyString(key, edge.From)
		key = appendKeyString(key, string(edge.Kind))
		key = appendKeyString(key, edge.To)
		key = appendKeyString(key, edge.FilePath)
		return remember(appendKeyLine(key, edge.Line), receipt, &stats.ShadowedEdges)
	}
	if err := claims(claimNode, claimEdge); err != nil {
		return stats, err
	}
	if err := ctx.Err(); err != nil {
		return stats, err
	}
	if finish != nil {
		if err := finish(&stats, put); err != nil {
			return stats, err
		}
	}
	return stats, ctx.Err()
}

func rebuildShadowedKeyStringBytes(s string) int {
	// Match appendKeyString, including NUL escaping and the terminator.
	return len(s) + strings.Count(s, "\x00") + 2
}
