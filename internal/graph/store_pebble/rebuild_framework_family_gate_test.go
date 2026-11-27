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
