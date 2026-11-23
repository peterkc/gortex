package store_pebble

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestRebuildShadowedIDsExact(t *testing.T) {
	type claim struct {
		id      string
		receipt uint64
	}
	// A one-byte ID costs 13 bytes: two prefix bytes, the ID, its two-byte
	// terminator and the eight-byte ordinal. An embedded NUL adds an escape byte.
	for _, tc := range []struct {
		name   string
		claims []claim
		count  uint64
		bytes  uint64
		peak   uint64
	}{
		{"three_receipts", []claim{{"x", 1}, {"x", 1}, {"x", 1<<32 | 1}, {"x", 1<<63 | 1}, {"x", 1}}, 1, 26, 13},
		{"one_receipt", []claim{{"x", 1}, {"x", 1}}, 0, 13, 13},
		{"distinct_bytes", []claim{{"a", 1}, {"A", 2}, {"a\x00b", 3}, {"a\x00c", 4}}, 0, 58, 16},
	} {
		t.Run(tc.name, func(t *testing.T) {
			b := newBuild(t, FullProfile)
			stats, err := b.CountRebuildShadowedIDs(t.Context(), tc.bytes, func(emit func(string, uint64) error) error {
				for _, c := range tc.claims {
					if err := emit(c.id, c.receipt); err != nil {
						return err
					}
				}
				return nil
			})
			require.NoError(t, err)
			require.Equal(t, tc.count, stats.Shadowed)
			require.Equal(t, tc.bytes, stats.ScratchBytes)
			require.Equal(t, tc.peak, stats.PeakRowBytes)
			requireRebuildShadowedScratchEmpty(t, b)
		})
	}
}

func TestRebuildShadowedIDsFailureClearsScratch(t *testing.T) {
	receiptErr := errors.New("planted shadowed ID receipt error")
	claimTwice := func(emit func(string, uint64) error) error {
		if err := emit("x", 1); err != nil {
			return err
		}
		return emit("x", 2)
	}
	for _, tc := range []struct {
		name  string
		limit uint64
		cause error
	}{
		{"producer", 39, receiptErr},
		{"capacity", 38, ErrRebuildLinkInputSize}, // Three writes cost 39, although only two keys are live.
		{"cancel", 39, context.Canceled},
	} {
		t.Run(tc.name, func(t *testing.T) {
			b := newBuild(t, FullProfile)
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			stats, err := b.CountRebuildShadowedIDs(ctx, tc.limit, func(emit func(string, uint64) error) error {
				if err := claimTwice(emit); err != nil {
					return err
				}
				switch tc.name {
				case "producer":
					return receiptErr
				case "cancel":
					cancel()
				}
				return emit("y", 3)
			})
			require.ErrorIs(t, err, tc.cause)
			require.Equal(t, RebuildShadowedIDStats{}, stats)
			requireRebuildShadowedScratchEmpty(t, b)

			stats, err = b.CountRebuildShadowedIDs(t.Context(), 26, claimTwice)
			require.NoError(t, err)
			require.Equal(t, uint64(1), stats.Shadowed)
			require.Equal(t, uint64(26), stats.ScratchBytes)
			requireRebuildShadowedScratchEmpty(t, b)
		})
	}
}

func requireRebuildShadowedScratchEmpty(t *testing.T, b *BuildStore) {
	t.Helper()
	occupied, err := rebuildGuardHasPrefix(t.Context(), b.pending, []byte{'g', 'H'})
	require.NoError(t, err)
	require.False(t, occupied, "shadowed ID scratch leaked")
}
