package indexer

import (
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/zzet/gortex/internal/clones"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/graph/store_pebble"
)

func TestRebuildCloneDiffusionNativeParity(t *testing.T) {
	// A–C has two qualifying bridges with different scores. The stronger
	// bridge sorts first, and re-associating its product changes a score bit.
	pairs := []clones.Pair{
		{A: "a.go::F", B: "b.go::F", Similarity: 0.97},
		{A: "a.go::F", B: "d.go::F", Similarity: 0.91},
		{A: "b.go::F", B: "c.rs::F", Similarity: 0.94},
		{A: "c.rs::F", B: "d.go::F", Similarity: 0.82},
		{A: "weak.go::Hub", B: "weak.go::S00", Similarity: 0.89},
		{A: "weak.go::Hub", B: "weak.go::S01", Similarity: 0.65},
	}
	// Seventeen equally strong spokes force a tie at the 16th neighbor.
	// The first two spokes are exact direct clones, not a semantic pair.
	for i := range 17 {
		pairs = append(pairs, clones.Pair{A: "fan.go::Hub", B: fmt.Sprintf("fan.go::S%02d", i), Similarity: 0.93})
	}
	pairs = append(pairs, clones.Pair{A: "fan.go::S00", B: "fan.go::S01", Similarity: 1})

	build, nodes := rebuildDiffusionFixture(t, pairs)
	want := nativeRebuildDiffusionRows(t, nodes, pairs, 121) // Two bridge pairs + 120 fan pairs - one exact pair.
	require.Equal(t, "a.go::F", want[0].From)
	require.Equal(t, "c.rs::F", want[0].To, "the Rust endpoint participates before D72 projection")
	requireRebuildDiffusionRows(t, build, pairs, want, false)
	slices.Reverse(pairs)
	requireRebuildDiffusionRows(t, build, pairs, want, false)
	requireRebuildTestFileRows(t, build, nodes, nil)
}

func TestRebuildCloneDiffusionGlobalCap(t *testing.T) {
	// 417 independent 16-spoke stars derive 50,040 distinct pairs. The
	// highest-scoring star arrives last and has the latest IDs, so a cut
	// taken before ranking cannot retain all of its pairs.
	pairs := make([]clones.Pair, 0, 417*16)
	for h := range 417 {
		prefix := fmt.Sprintf("star-%03d.go::", h)
		score := 0.94
		if h == 416 {
			score = 0.99
		}
		for s := range 16 {
			pairs = append(pairs, clones.Pair{A: prefix + "Hub", B: fmt.Sprintf("%sS%02d", prefix, s), Similarity: score})
		}
	}
	build, nodes := rebuildDiffusionFixture(t, pairs)
	want := nativeRebuildDiffusionRows(t, nodes, pairs, 50_000)
	require.Equal(t, "star-416.go::S00", want[0].From)
	require.Equal(t, "star-416.go::S01", want[0].To)
	last := want[len(want)-2]
	require.Equal(t, "star-415.go::S06", last.From)
	require.Equal(t, "star-415.go::S11", last.To, "the next pair shares its score and first ID")
	requireRebuildDiffusionRows(t, build, pairs, want, true)
}

func TestRebuildCloneDiffusionFailureAndRetry(t *testing.T) {
	pairs := []clones.Pair{
		{A: "a.go::F", B: "b.go::F", Similarity: 0.95},
		{A: "a.go::F", B: "c.go::F", Similarity: 0.94},
		{A: "a.go::F", B: "d.go::F", Similarity: 0.93},
	}
	build, nodes := rebuildDiffusionFixture(t, pairs)
	want := nativeRebuildDiffusionRows(t, nodes, pairs, 3)
	// The retry omits B. Replaying the original stream could hide stale
	// adjacency or ranked rows from the failed attempt.
	retry := pairs[1:]
	wantRetry := nativeRebuildDiffusionRows(t, nodes, retry, 1)

	t.Run("direct_stream_error", func(t *testing.T) {
		inputErr := errors.New("planted direct stream failure")
		visit := func(yield func(clones.Pair) error) error {
			if err := rebuildDiffusionDirect(pairs)(yield); err != nil {
				return err
			}
			return inputErr
		}
		calls := 0
		count, capped, err := visitRebuildDiffusedCloneEdges(t.Context(), build, visit, func(*graph.Edge) error {
			calls++
			return nil
		})
		require.ErrorIs(t, err, inputErr)
		require.Zero(t, count)
		require.False(t, capped)
		require.Zero(t, calls, "a failed direct stream does not establish the rank")
		requireRebuildDiffusionRows(t, build, retry, wantRetry, false)
		requireRebuildTestFileRows(t, build, nodes, nil)
	})

	t.Run("edge_sink_error", func(t *testing.T) {
		sinkErr := errors.New("planted semantic edge sink failure")
		var got []*graph.Edge
		count, capped, err := visitRebuildDiffusedCloneEdges(t.Context(), build, rebuildDiffusionDirect(pairs), func(e *graph.Edge) error {
			got = append(got, e)
			if len(got) == 3 {
				return sinkErr // The first symmetric pair was only tentative.
			}
			return nil
		})
		require.ErrorIs(t, err, sinkErr)
		require.Zero(t, count)
		require.False(t, capped)
		require.Equal(t, want[:3], got, "stop without yielding the failed edge's reverse")
		requireRebuildDiffusionRows(t, build, retry, wantRetry, false)
		requireRebuildTestFileRows(t, build, nodes, nil)
	})

	t.Run("missing_checked_endpoint", func(t *testing.T) {
		broken := []clones.Pair{pairs[0], {A: "a.go::F", B: "missing.go::F", Similarity: 0.94}}
		calls := 0
		count, capped, err := visitRebuildDiffusedCloneEdges(t.Context(), build, rebuildDiffusionDirect(broken), func(*graph.Edge) error {
			calls++
			return nil
		})
		require.ErrorIs(t, err, store_pebble.ErrRebuildView)
		require.Zero(t, count)
		require.False(t, capped)
		require.Zero(t, calls, "refuse instead of emitting a one-sided pair")
	})
}

func rebuildDiffusionFixture(t *testing.T, pairs []clones.Pair) (*store_pebble.BuildStore, []*graph.Node) {
	t.Helper()
	build, _ := openRebuildTestFileStore(t)
	seen := make(map[string]bool)
	var nodes []*graph.Node
	for _, p := range pairs {
		for _, id := range []string{p.A, p.B} {
			if seen[id] {
				continue
			}
			seen[id] = true
			path, name, _ := strings.Cut(id, "::")
			language := "go"
			if strings.HasSuffix(path, ".rs") {
				language = "rust"
			}
			nodes = append(nodes, &graph.Node{
				ID: id, Name: name, Kind: graph.KindFunction, Language: language,
				FilePath: path, StartLine: len(nodes) + 1, EndLine: len(nodes) + 2,
			})
		}
	}
	build.AddBatch(nodes, nil)
	return build, nodes
}

func rebuildDiffusionDirect(pairs []clones.Pair) func(func(clones.Pair) error) error {
	return func(yield func(clones.Pair) error) error {
		for _, p := range pairs {
			if err := yield(p); err != nil {
				return err
			}
		}
		return nil
	}
}

// Graph scans are not insertion-ordered. Record the native AddBatch calls
// to observe native rank and constructor output without reproducing either.
type rebuildDiffusionOracle struct {
	*graph.Graph
	rows []*graph.Edge
}

func (g *rebuildDiffusionOracle) AddBatch(nodes []*graph.Node, edges []*graph.Edge) {
	g.Graph.AddBatch(nodes, edges)
	g.rows = append(g.rows, edges...)
}

func nativeRebuildDiffusionRows(t *testing.T, nodes []*graph.Node, pairs []clones.Pair, wantPairs int) []*graph.Edge {
	t.Helper()
	oracle := &rebuildDiffusionOracle{Graph: graph.New()}
	oracle.Graph.AddBatch(nodes, nil)
	direct := make(map[[2]string]struct{}, len(pairs))
	for _, p := range pairs {
		direct[[2]string{p.A, p.B}] = struct{}{}
	}
	count, edges := diffuseSimilarityEdges(oracle, pairs, direct)
	require.Equal(t, wantPairs, count)
	require.Equal(t, 2*wantPairs, edges)
	require.Len(t, oracle.rows, edges)
	return oracle.rows
}

func requireRebuildDiffusionRows(t *testing.T, build *store_pebble.BuildStore, pairs []clones.Pair, want []*graph.Edge, wantCapped bool) {
	t.Helper()
	n := 0
	count, capped, err := visitRebuildDiffusedCloneEdges(t.Context(), build, rebuildDiffusionDirect(pairs), func(e *graph.Edge) error {
		if n >= len(want) {
			return fmt.Errorf("unexpected semantic edge %q to %q after %d rows", e.From, e.To, n)
		}
		// Full row equality includes the exact positive float64 score in
		// Confidence and Meta, along with direction, locality and origin.
		require.Equal(t, want[n], e, "semantic row %d", n)
		n++
		return nil
	})
	require.NoError(t, err)
	require.Equal(t, len(want)/2, count)
	require.Equal(t, wantCapped, capped)
	require.Equal(t, len(want), n)
}
