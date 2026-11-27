package store_pebble

import (
	"path/filepath"
	"testing"

	"github.com/cockroachdb/pebble/v2"
	"github.com/stretchr/testify/require"
	"github.com/zzet/gortex/internal/graph"
)

func TestRebuildGRPCRejectsInvalidScratch(t *testing.T) {
	b, err := OpenBuild(filepath.Join(t.TempDir(), "working"), FullProfile)
	require.NoError(t, err)
	t.Cleanup(func() {
		require.NoError(t, b.Close())
	})
	ctx := t.Context()
	var ran bool
	receipts := func(func(ParseFacts, error) bool) {
		ran = true
	}
	rebind := func(*graph.Edge, string, string, string, string) bool {
		ran = true
		return false
	}
	for _, limit := range []int{0, goldBufferBytes + 1} {
		stats, err := b.RebuildGRPC(ctx, receipts, limit, rebind)
		require.ErrorIs(t, err, ErrRebuildGRPC)
		require.ErrorIs(t, err, ErrRebuildLinkInputSize)
		require.Zero(t, stats)
	}
	key := rebuildGRPCKey('s', "occupied")
	require.NoError(t, b.pending.Set(key, []byte("owner"), pebble.NoSync))
	stats, err := b.RebuildGRPC(ctx, receipts, 1, rebind)
	require.ErrorIs(t, err, ErrRebuildGRPC)
	require.ErrorContains(t, err, "gRPC scratch range occupied")
	require.Zero(t, stats)
	owner, err := pendingGet(b.pending, key, func(raw []byte) (string, error) {
		return string(raw), nil
	})
	require.NoError(t, err)
	require.Equal(t, "owner", owner)
	require.False(t, ran)
	require.NoError(t, b.RebuildView().VisitEdges(ctx, func(*graph.Edge) error {
		t.Fatal("a refusal must not add edges")
		return nil
	}))
}
