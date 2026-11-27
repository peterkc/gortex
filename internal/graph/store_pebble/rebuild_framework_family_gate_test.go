package store_pebble

import (
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/zzet/gortex/internal/graph"
)

func TestRebuildFamilyDropRequiresSavedPayload(t *testing.T) {
	b, err := OpenBuild(filepath.Join(t.TempDir(), "working"), FullProfile)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, b.Close()) })
	e := &graph.Edge{From: "a", To: "b", Kind: graph.EdgeReferences, FilePath: "a.ts", Line: 2,
		Meta: map[string]any{"marker": "current"}}
	b.AddEdge(e)
	key := pendingEdgeKey(e)
	want, err := pendingGet(b.pending, key, copyValue)
	require.NoError(t, err)
	require.NotEmpty(t, want)
	require.NoError(t, b.checkRebuildFamilyDrop(key, want))
	changed := *e
	changed.Meta = map[string]any{"marker": "stale"}
	stale, err := encodeEdge(&changed)
	require.NoError(t, err)
	require.ErrorIs(t, b.checkRebuildFamilyDrop(key, stale), ErrRebuildUnknownProvenance)
	got, err := pendingGet(b.pending, key, copyValue)
	require.NoError(t, err)
	require.Equal(t, want, got)
}

func TestRebuildFamilyDropRemovesExactPendingRows(t *testing.T) {
	b := newBuild(t, FullProfile)
	b.AddNode(&graph.Node{ID: "a", Language: "typescript"})
	b.AddNode(&graph.Node{ID: "b", Language: "java"})
	drop := &graph.Edge{From: "a", To: "b", Kind: graph.EdgeReferences, FilePath: "a.ts", Line: 2,
		Meta: map[string]any{"synth": "drop"}}
	keep := *drop
	keep.Line = 3
	keep.Meta = map[string]any{"synth": "keep"}
	proof := rebuildEdgeCustody{proof: rebuildProofUntrackedMove}
	require.NoError(t, b.putEdgeWithCustody(drop, proof))
	require.NoError(t, b.putEdgeWithCustody(&keep, proof))
	want := rebuildFrameworkCustodyRows(t, b)
	key := pendingEdgeKey(drop)
	for _, k := range [][]byte{key, pendingReverseKey(drop, key), rebuildLinkRecorderKey(key)} {
		require.Contains(t, want, string(k))
		delete(want, string(k))
	}
	count, err := b.RebuildFrameworkFamilyGate(t.Context(), func(e *graph.Edge) bool { return e.Meta["synth"] == "drop" },
		func(from, to string) bool { return from == "typescript" && to == "java" })
	require.NoError(t, err)
	require.EqualValues(t, 1, count)
	require.Equal(t, want, rebuildFrameworkCustodyRows(t, b))
}
