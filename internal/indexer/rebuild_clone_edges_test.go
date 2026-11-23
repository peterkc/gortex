package indexer

import (
	"errors"
	"fmt"
	"iter"
	"maps"
	"slices"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/zzet/gortex/internal/clones"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/graph/store_pebble"
)

func TestRebuildDirectCloneEdgesNativeParity(t *testing.T) {
	// The Go/Rust pair has a lower score and earlier IDs than the exact Go pair.
	// Their length classes do not overlap with those of the exact pair.
	shingles := []uint64{1, 2, 3, 4, 5, 6, 7, 8}
	inputs := []rebuildCloneInput{
		{ID: "a.go::F", FilePath: "a.go", Language: "go", Line: 3, Tokens: 79, Shingles: shingles},
		{ID: "b.rs::F", FilePath: "b.rs", Language: "rust", Line: 7, Tokens: 80, Shingles: []uint64{1, 2, 3, 4, 5, 6, 7, 8, 9}},
		{ID: "y.go::F", FilePath: "y.go", Language: "go", Line: 11, Tokens: 500, Shingles: shingles},
		{ID: "z.go::F", FilePath: "z.go", Language: "go", Line: 17, Tokens: 500, Shingles: shingles},
	}
	var facts []store_pebble.ParseFacts
	for _, input := range inputs {
		facts = append(facts, rebuildCloneInputFixture(input, input.Language))
	}
	facts[2].Edges = []*graph.Edge{{From: inputs[2].ID, To: inputs[3].ID, Kind: graph.EdgeCalls, FilePath: inputs[2].FilePath, Line: 12}}
	build, _ := openRebuildTestFileStore(t)
	receipts := seedRebuildCloneInputReceipts(t, build, facts...)
	const threshold = 0.75
	want, wantStats := nativeRebuildDirectCloneEdges(t, receipts, threshold, len(inputs))
	require.Equal(t, store_pebble.RebuildClonePairStats{Pairs: 2}, wantStats)
	require.Len(t, want, 4)
	require.Equal(t, inputs[2].ID, want[0].From)
	require.Equal(t, inputs[0].ID, want[2].From)
	require.Equal(t, inputs[1].ID, want[2].To, "the deferred Rust endpoint participates before projection")
	require.Less(t, want[2].Confidence, want[0].Confidence)

	view := build.RebuildView()
	var beforeNodes []*graph.Node
	require.NoError(t, view.VisitNodes(t.Context(), func(n *graph.Node) error {
		beforeNodes = append(beforeNodes, n)
		return nil
	}))
	var beforeEdges []*graph.Edge
	require.NoError(t, view.VisitEdges(t.Context(), func(e *graph.Edge) error {
		beforeEdges = append(beforeEdges, e)
		return nil
	}))
	require.Len(t, beforeNodes, len(inputs))
	require.Len(t, beforeEdges, 1)

	t.Run("ordered_rows", func(t *testing.T) {
		var got []*graph.Edge
		stats, err := visitRebuildDirectCloneEdges(t.Context(), build, receipts, threshold, func(e *graph.Edge) error {
			got = append(got, e)
			return nil
		})
		require.NoError(t, err)
		require.Equal(t, wantStats, stats)
		require.Equal(t, want, got, "exact tentative rows in native pair order")
		requireRebuildTestFileRows(t, build, beforeNodes, beforeEdges)
	})

	t.Run("late_receipt_error", func(t *testing.T) {
		receiptErr := errors.New("planted clone receipt failure")
		pass := 0
		faulty := func(yield func(store_pebble.ParseFacts, error) bool) {
			pass++
			for f, err := range receipts {
				if !yield(f, err) {
					return
				}
			}
			// Refuse at the end of H53's finalizing replay, after banking inputs.
			if pass == 2 {
				yield(store_pebble.ParseFacts{}, receiptErr)
			}
		}
		calls := 0
		stats, err := visitRebuildDirectCloneEdges(t.Context(), build, faulty, threshold, func(*graph.Edge) error {
			calls++
			return nil
		})
		require.ErrorIs(t, err, receiptErr)
		require.Zero(t, stats)
		require.Zero(t, calls, "no edge can escape an incomplete cohort")
		requireRebuildTestFileRows(t, build, beforeNodes, beforeEdges)
	})

	t.Run("edge_sink_error", func(t *testing.T) {
		sinkErr := errors.New("planted clone edge sink failure")
		var got []*graph.Edge
		stats, err := visitRebuildDirectCloneEdges(t.Context(), build, receipts, threshold, func(e *graph.Edge) error {
			got = append(got, e)
			if len(got) == 3 {
				return sinkErr // One full pair has already reached the sink.
			}
			return nil
		})
		require.ErrorIs(t, err, sinkErr)
		require.Zero(t, stats, "even the completed pair does not count on failure")
		require.Equal(t, want[:3], got, "stop before the failed edge's reverse direction")
		requireRebuildTestFileRows(t, build, beforeNodes, beforeEdges)
	})
}

func TestRebuildDirectCloneEdgesMixedBucketSuppressed(t *testing.T) {
	const count = 257
	shingles := []uint64{1, 2, 3, 4, 5, 6, 7, 8}
	facts := make([]store_pebble.ParseFacts, 0, count)
	for i := range count {
		path, language := fmt.Sprintf("f%03d.rs", i), "rust"
		if i < 2 {
			path, language = fmt.Sprintf("f%03d.go", i), "go"
		}
		facts = append(facts, rebuildCloneInputFixture(rebuildCloneInput{
			ID: path + "::F", FilePath: path, Language: language, Line: i + 1, Tokens: 40, Shingles: shingles,
		}, language))
	}
	build, _ := openRebuildTestFileStore(t)
	receipts := seedRebuildCloneInputReceipts(t, build, facts...)
	want, wantStats := nativeRebuildDirectCloneEdges(t, receipts, 0, count)
	require.Empty(t, want)
	require.Equal(t, store_pebble.RebuildClonePairStats{
		SkippedBuckets: clones.Bands, SkippedBucketItems: clones.Bands * count,
	}, wantStats)

	var got []*graph.Edge
	stats, err := visitRebuildDirectCloneEdges(t.Context(), build, receipts, 0, func(e *graph.Edge) error {
		got = append(got, e)
		return nil
	})
	require.NoError(t, err)
	require.Equal(t, wantStats, stats)
	require.Equal(t, want, got, "removing Rust before banking would let the identical Go pair survive")
}

func nativeRebuildDirectCloneEdges(t *testing.T, receipts iter.Seq2[store_pebble.ParseFacts, error], threshold float64, bodies int) ([]*graph.Edge, store_pebble.RebuildClonePairStats) {
	t.Helper()
	oracle := graph.New()
	for f, err := range receipts {
		require.NoError(t, err)
		for _, n := range f.Nodes {
			clone := *n
			clone.Meta = maps.Clone(n.Meta)
			clone.Meta[cloneShinglesMetaKey] = slices.Clone(f.Shingles[n.ID])
			oracle.AddNode(&clone)
		}
	}
	items, corpus := finaliseCloneSignaturesFromNodes(oracle, "")
	require.Equal(t, bodies, corpus)
	require.Len(t, items, bodies)
	pairs, skipped, skippedItems := clones.DetectPairsStratifiedWithStats(items, threshold)
	materialized, edges := materializeClonePairs(oracle, pairs, graph.EdgeSimilarTo, nil)
	require.Equal(t, len(pairs), materialized)
	require.Equal(t, 2*len(pairs), edges)
	rows := oracle.AllEdges()
	require.Len(t, rows, edges)

	// Graph scans are not insertion-ordered. Use the native pair order to
	// pick the two exact, already-materialized rows for each pair.
	byEndpoints := make(map[[2]string]*graph.Edge, len(rows))
	for _, e := range rows {
		byEndpoints[[2]string{e.From, e.To}] = e
	}
	var ordered []*graph.Edge
	for _, p := range pairs {
		ab := byEndpoints[[2]string{p.A, p.B}]
		ba := byEndpoints[[2]string{p.B, p.A}]
		require.NotNil(t, ab)
		require.NotNil(t, ba)
		ordered = append(ordered, ab, ba)
	}
	return ordered, store_pebble.RebuildClonePairStats{
		Pairs: uint64(len(pairs)), SkippedBuckets: uint64(skipped), SkippedBucketItems: uint64(skippedItems),
	}
}
