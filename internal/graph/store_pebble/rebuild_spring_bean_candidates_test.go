package store_pebble

import (
	"testing"

	"github.com/cockroachdb/pebble/v2"
	"github.com/stretchr/testify/require"
	"github.com/zzet/gortex/internal/graph"
)

func TestRebuildSpringBeanConsumersRefuseOccupiedScratch(t *testing.T) {
	b := newBuild(t, FullProfile)
	key := springBeanScratch('t', "Service")
	require.NoError(t, b.pending.Set(key, []byte("owner"), pebble.NoSync))
	err := b.VisitSpringBeanConsumers(t.Context(), "repo",
		func(*graph.Edge) string { return "" },
		func(*graph.Node) (string, map[string]struct{}) { return "", nil },
		func(string, string, *graph.Edge) error { return nil },
	)
	require.ErrorIs(t, err, errInvalidRecord)
	require.ErrorContains(t, err, "Spring bean scratch range occupied")
	owner, err := pendingGet(b.pending, key, func(raw []byte) (string, error) { return string(raw), nil })
	require.NoError(t, err)
	require.Equal(t, "owner", owner)
}
