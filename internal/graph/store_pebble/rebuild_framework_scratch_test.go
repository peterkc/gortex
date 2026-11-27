package store_pebble

import (
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/zzet/gortex/internal/graph"
)

func TestRebuildScratchRowLimit(t *testing.T) {
	b := newBuild(t, FullProfile)
	const limit = 8
	key := []byte("gxa")
	value := []byte("12345")
	before := b.pending.Metrics().Levels[0].TableBytesFlushed
	require.NoError(t, b.putRebuildScratch(key, value, limit, "example"))

	// An oversized replacement must not change the exact-limit row.
	err := b.putRebuildScratch(key, []byte("123456"), limit, "example")
	require.ErrorIs(t, err, ErrRebuildLinkInputSize)
	require.ErrorContains(t, err, "example scratch row")
	got, err := pendingGet(b.pending, key, copyValue)
	require.NoError(t, err)
	require.Equal(t, value, got)

	// A later prefix reader sees immediate writes without an explicit flush.
	replacement := []byte("new")
	require.NoError(t, b.putRebuildScratch(key, replacement, limit, "example"))
	var rows int
	require.NoError(t, pendingVisit(t.Context(), b.pending, []byte("gx"), func(r row) error {
		rows++
		require.Equal(t, key, r.key)
		require.Equal(t, replacement, r.value)
		return nil
	}))
	require.Equal(t, 1, rows)
	require.Equal(t, before, b.pending.Metrics().Levels[0].TableBytesFlushed)
}

func TestRebuildScratchRefusalsKeepLowestCompleteKey(t *testing.T) {
	var refused rebuildScratchRefusals
	require.NoError(t, refused.err())
	for _, line := range []int{20, 30, 10} {
		e := &graph.Edge{From: "caller", To: "target", Kind: graph.EdgeCalls, FilePath: "call.go", Line: line}
		refused.add(graph.EdgeIdentityFor(e), pendingEdgeKey(e))
	}
	var got *RebuildUnknownProvenanceError
	require.ErrorAs(t, refused.err(), &got)
	require.Equal(t, &RebuildUnknownProvenanceError{
		Count: 3,
		First: graph.EdgeIdentity{From: "caller", To: "target", Kind: graph.EdgeCalls, FilePath: "call.go", Line: 10},
	}, got)
}
