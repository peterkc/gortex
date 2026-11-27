package store_pebble

import (
	"testing"

	"github.com/cockroachdb/pebble/v2"
	"github.com/stretchr/testify/require"
	"github.com/zzet/gortex/internal/graph"
)

func TestRebuildEdgeCustodyDeletedOrReplaced(t *testing.T) {
	for _, replace := range []bool{false, true} {
		name := "delete-and-reuse"
		if replace {
			name = "same-key-replacement"
		}
		t.Run(name, func(t *testing.T) {
			b := newBuild(t, FullProfile)
			e := &graph.Edge{From: "caller", To: "field", Kind: graph.EdgeAccessesField, FilePath: "source.go", Line: 9}
			key := pendingEdgeKey(e)
			require.NoError(t, b.addEdgeWithCustody(e, rebuildEdgeCustody{proof: rebuildProofReceiver}))
			got, err := readRebuildEdgeCustody(b.pending, key)
			require.NoError(t, err)
			require.Equal(t, rebuildProofReceiver, got.proof)
			if !replace {
				require.NoError(t, b.deleteEdge(key, e))
				got, err = readRebuildEdgeCustody(b.pending, key)
				require.NoError(t, err)
				require.Zero(t, got)
			}
			e.Via = "ordinary replacement"
			require.NoError(t, b.addEdge(e))
			got, err = readRebuildEdgeCustody(b.pending, key)
			require.NoError(t, err)
			require.Zero(t, got)
		})
	}
}

func TestRebuildEdgeCustodyRefusesStalePayload(t *testing.T) {
	b := newBuild(t, FullProfile)
	e := &graph.Edge{From: "caller", To: "field", Kind: graph.EdgeAccessesField, FilePath: "source.go", Line: 9}
	key := pendingEdgeKey(e)
	require.NoError(t, b.addEdgeWithCustody(e, rebuildEdgeCustody{proof: rebuildProofReceiver}))
	// Change only the saved payload, bypassing replacement's normal recorder
	// invalidation. A stale record for this exact key must not prove the row.
	e.Via = "not the installed payload"
	wrong, err := encodeEdge(e)
	require.NoError(t, err)
	require.NoError(t, b.pending.Set(key, wrong, pebble.NoSync))
	got, err := readRebuildEdgeCustody(b.pending, key)
	var unknown *RebuildUnknownProvenanceError
	require.ErrorAs(t, err, &unknown)
	require.Equal(t, graph.EdgeIdentityFor(e), unknown.First)
	require.EqualValues(t, 1, unknown.Count)
	require.Zero(t, got)
}
