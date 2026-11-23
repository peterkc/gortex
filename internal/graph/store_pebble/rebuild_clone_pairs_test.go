package store_pebble

import (
	"errors"
	"fmt"
	"iter"
	"slices"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/zzet/gortex/internal/clones"
)

func TestRebuildClonePairsNativeParity(t *testing.T) {
	b := newBuild(t, FullProfile)
	exact := rebuildCloneTestSignature(1, 1)
	near := exact
	for i := range near[:2*clones.Rows] {
		near[i] = 2
	}
	items := []clones.Item{
		{ID: "z", Sig: exact, TokenCount: 60},
		{ID: "a", Sig: near, TokenCount: 60},
		{ID: "m", Sig: exact, TokenCount: 60},
		{ID: "far", Sig: exact, TokenCount: 500},
		{ID: "b", Sig: exact, TokenCount: 60},
		{ID: "miss", Sig: rebuildCloneTestSignature(1, 3), TokenCount: 60},
	}
	native := clones.NewStratifiedIndex()
	for _, item := range items {
		native.Add(item)
	}
	want, skipped, skippedItems := native.DetectPairsWithStats(0)
	require.Equal(t, []clones.Pair{
		{A: "b", B: "m", Similarity: 1},
		{A: "b", B: "z", Similarity: 1},
		{A: "m", B: "z", Similarity: 1},
		{A: "a", B: "b", Similarity: 0.875},
		{A: "a", B: "m", Similarity: 0.875},
		{A: "a", B: "z", Similarity: 0.875},
	}, want)
	for _, order := range []string{"forward", "reverse"} {
		t.Run(order, func(t *testing.T) {
			var got []clones.Pair
			stats, err := b.VisitRebuildClonePairs(t.Context(), rebuildCloneTestItems(items), 0, func(p clones.Pair) error {
				got = append(got, p)
				return nil
			})
			require.NoError(t, err)
			require.Equal(t, want, got)
			require.Equal(t, RebuildClonePairStats{
				Pairs: uint64(len(want)), SkippedBuckets: uint64(skipped), SkippedBucketItems: uint64(skippedItems),
			}, stats)
		})
		slices.Reverse(items)
	}
}

func TestRebuildClonePairsCollision(t *testing.T) {
	b := newBuild(t, FullProfile)
	// T126's two pairs both hash the bytes "a\x1fb\x1fc" to 0xc9ddc9381a0939ab.
	// In band 0, the all-4 bucket (0xa96e8a795911dbe4) precedes the all-1
	// bucket (0xe513ec437a13fd84), despite raw slots and IDs ordering the other way.
	p := clones.Pair{A: "a", B: "b\x1fc", Similarity: 1}
	q := clones.Pair{A: "a\x1fb", B: "c", Similarity: 1}
	for _, tc := range []struct {
		name       string
		limit      int // zero uses the public entry point
		qTail      uint32
		want       []clones.Pair
		missTokens int
	}{
		{name: "canonical_winner", qTail: 4, want: []clones.Pair{q}},
		{name: "miss_still_claims_hash", qTail: 5},
		{name: "miss_counts_toward_cutoff", limit: 2, qTail: 4, missTokens: 60,
			want: []clones.Pair{p, {A: q.A, B: q.B, Similarity: 49.0 / 64}}},
		{name: "exact_pairs_merge_after_cutoff", limit: 1, qTail: 4, want: []clones.Pair{p, q}},
		{name: "new_class_resets_cutoff", limit: 2, qTail: 4, want: []clones.Pair{q}, missTokens: 40},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tokens := 60 // two classes
			var items []clones.Item
			if tc.missTokens != 0 {
				// These share only band 0, whose all-zero hash sorts ahead of both collision buckets.
				items = []clones.Item{
					{ID: "early-a", Sig: rebuildCloneTestSignature(0, 7), TokenCount: tc.missTokens},
					{ID: "early-b", Sig: rebuildCloneTestSignature(0, 8), TokenCount: tc.missTokens},
				}
			}
			qSig := rebuildCloneTestSignature(4, tc.qTail)
			switch tc.missTokens {
			case 40:
				tokens = 85 // class 1 only; class 0 hits the cutoff on misses
				items = append(items, clones.Item{ID: "early-c", Sig: rebuildCloneTestSignature(0, 9), TokenCount: 40})
			case 60:
				// q reaches the cutoff in band 0. No later band can recover it.
				for band := 1; band < clones.Bands; band++ {
					qSig[band*clones.Rows] = 5
				}
			}
			items = append(items,
				clones.Item{ID: p.B, Sig: rebuildCloneTestSignature(1, 1), TokenCount: tokens},
				clones.Item{ID: q.B, Sig: qSig, TokenCount: tokens},
				clones.Item{ID: p.A, Sig: rebuildCloneTestSignature(1, 1), TokenCount: tokens},
				clones.Item{ID: q.A, Sig: rebuildCloneTestSignature(4, 4), TokenCount: tokens},
			)
			for _, order := range []string{"forward", "reverse"} {
				t.Run(order, func(t *testing.T) {
					var got []clones.Pair
					yield := func(p clones.Pair) error {
						got = append(got, p)
						return nil
					}
					var stats RebuildClonePairStats
					var err error
					if tc.limit == 0 {
						stats, err = b.VisitRebuildClonePairs(t.Context(), rebuildCloneTestItems(items), 0.75, yield)
					} else {
						stats, err = b.visitRebuildClonePairs(t.Context(), rebuildCloneTestItems(items), 0.75, tc.limit, yield)
					}
					require.NoError(t, err)
					require.Equal(t, tc.want, got)
					require.Equal(t, RebuildClonePairStats{Pairs: uint64(len(tc.want))}, stats)
				})
				slices.Reverse(items)
			}
		})
	}
}

func TestRebuildClonePairsBucketBoundary(t *testing.T) {
	b := newBuild(t, FullProfile)
	for _, count := range []int{256, 257} {
		t.Run(fmt.Sprint(count), func(t *testing.T) {
			var want []clones.Pair
			var wantStats RebuildClonePairStats
			if count == 256 {
				want = []clones.Pair{{A: "000", B: "001", Similarity: 49.0 / 64}}
				wantStats.Pairs = 1
			} else {
				wantStats.SkippedBuckets = clones.Bands
				wantStats.SkippedBucketItems = clones.Bands * uint64(count)
			}
			items := make([]clones.Item, count)
			for i := range items {
				tail := uint32(1)
				if count == 256 && i >= 2 {
					// Only 000 and 001 can clear the threshold.
					tail += uint32(i)
				}
				item := clones.Item{ID: fmt.Sprintf("%03d", i), Sig: rebuildCloneTestSignature(1, tail), TokenCount: 40}
				if count == 256 && i == 1 {
					// 000 and 001 must meet in band 0, not in a smaller bucket.
					for band := 1; band < clones.Bands; band++ {
						item.Sig[band*clones.Rows] = 2
					}
				}
				items[count-1-i] = item
			}
			var got []clones.Pair
			stats, err := b.VisitRebuildClonePairs(t.Context(), rebuildCloneTestItems(items), 0.75, func(p clones.Pair) error {
				got = append(got, p)
				return nil
			})
			require.NoError(t, err)
			require.Equal(t, want, got)
			require.Equal(t, wantStats, stats)
		})
	}
}

func TestRebuildClonePairsFailureClearsScratch(t *testing.T) {
	b := newBuild(t, FullProfile)
	sig := rebuildCloneTestSignature(1, 1)
	a := clones.Item{ID: "a", Sig: sig, TokenCount: 40}
	z := clones.Item{ID: "z", Sig: sig, TokenCount: 40}
	valid := rebuildCloneTestItems([]clones.Item{z, a})
	receiptErr := errors.New("planted clone receipt error")
	sinkErr := errors.New("planted clone pair sink error")
	receipt := func(yield func(clones.Item, error) bool) {
		if yield(z, nil) && yield(a, nil) {
			yield(clones.Item{}, receiptErr)
		}
	}
	accept := func(clones.Pair) error { return nil }
	for _, tc := range []struct {
		name  string
		items iter.Seq2[clones.Item, error]
		yield func(clones.Pair) error
		cause error
	}{
		{"duplicate_id", rebuildCloneTestItems([]clones.Item{z, a, z}), accept, nil},
		{"receipt", receipt, accept, receiptErr},
		{"sink", valid, func(clones.Pair) error { return sinkErr }, sinkErr},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := t.Context()
			_, err := b.VisitRebuildClonePairs(ctx, tc.items, 0, tc.yield)
			if tc.cause != nil {
				require.ErrorIs(t, err, tc.cause)
			} else {
				require.ErrorContains(t, err, "duplicate clone item ID")
			}
			occupied, err := rebuildGuardHasPrefix(ctx, b.pending, []byte{'g', 'L'})
			require.NoError(t, err)
			require.False(t, occupied, "clone pair scratch leaked after refusal")

			var got []clones.Pair
			stats, err := b.VisitRebuildClonePairs(ctx, valid, 0, func(p clones.Pair) error {
				got = append(got, p)
				return nil
			})
			require.NoError(t, err)
			require.Equal(t, []clones.Pair{{A: "a", B: "z", Similarity: 1}}, got)
			require.Equal(t, RebuildClonePairStats{Pairs: 1}, stats)
		})
	}
}

func rebuildCloneTestSignature(first, tail uint32) (sig clones.Signature) {
	for i := range sig {
		sig[i] = tail
	}
	for i := range clones.Rows {
		sig[i] = first
	}
	return sig
}

func rebuildCloneTestItems(items []clones.Item) iter.Seq2[clones.Item, error] {
	return func(yield func(clones.Item, error) bool) {
		for _, item := range items {
			if !yield(item, nil) {
				return
			}
		}
	}
}
