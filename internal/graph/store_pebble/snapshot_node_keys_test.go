package store_pebble

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/graph/store_sqlite"
)

func pairedSnapshot(t *testing.T, nodes []*graph.Node, edges []*graph.Edge) (*store_sqlite.Store, *BuildStore, *Snapshot) {
	t.Helper()
	sql, err := store_sqlite.Open(filepath.Join(t.TempDir(), "seam.sqlite"))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, sql.Close()) })
	b := newBuild(t, FullProfile)
	sql.AddBatch(nodes, edges)
	b.AddBatch(nodes, edges)
	require.NoError(t, b.Publish(t.Context()))
	return sql, b, snapshot(t, b)
}

func TestSnapshotNodeProjectionParity(t *testing.T) {
	nodes := []*graph.Node{
		{ID: "repo/a", Kind: graph.KindFunction, Name: "A", FilePath: "repo/a.go", RepoPrefix: "repo", WorkspaceID: "w", ProjectID: "p", Meta: map[string]any{"doc": "do not surface"}},
		{ID: "outside", Kind: graph.KindType, Name: "B", FilePath: "repo/b.go", RepoPrefix: "repo"},
		{ID: "solo", Kind: graph.KindFile, Name: "solo.go", FilePath: "solo.go"},
	}
	sql, b, s := pairedSnapshot(t, nodes, nil)
	ctx := t.Context()
	ids := []string{"solo", "", "missing", "repo/a", "repo/a", "outside"}
	kinds := []graph.NodeKind{graph.KindFunction, graph.KindFunction, graph.KindFile}
	wantRows, err := sql.GetNodeKindsByIDsContext(ctx, ids)
	require.NoError(t, err)
	gotRows, err := s.GetNodeKindsByIDsContext(ctx, ids)
	require.NoError(t, err)
	require.Equal(t, wantRows, gotRows)
	require.Len(t, gotRows, 3)
	wantMembership, err := sql.GetNodeIDsByKindsContext(ctx, ids, kinds)
	require.NoError(t, err)
	gotMembership, err := s.GetNodeIDsByKindsContext(ctx, ids, kinds)
	require.NoError(t, err)
	require.Equal(t, wantMembership, gotMembership)
	require.Len(t, gotMembership, 2)
	none, err := s.GetNodeIDsByKindsContext(context.TODO(), ids, nil)
	require.NoError(t, err)
	require.Empty(t, none)
	wantPresence, err := sql.GetNodePresenceByIDsContext(ctx, ids)
	require.NoError(t, err)
	gotPresence, err := s.GetNodePresenceByIDsContext(ctx, ids)
	require.NoError(t, err)
	require.Equal(t, wantPresence, gotPresence)
	require.Len(t, gotPresence, 3)
	var want, got [][]graph.NodeSearchKey
	require.NoError(t, sql.ScanNodeSearchKeys(ctx, 2, func(page []graph.NodeSearchKey) bool { want = append(want, page); return true }))
	require.NoError(t, s.ScanNodeSearchKeys(ctx, 2, func(page []graph.NodeSearchKey) bool {
		require.NotNil(t, s.GetNode(page[0].ID))
		got = append(got, page)
		return true
	}))
	require.Equal(t, want, got)
	require.Len(t, got, 2)
	calls := 0
	require.NoError(t, s.ScanNodeSearchKeys(ctx, 1, func(page []graph.NodeSearchKey) bool { require.Len(t, page, 1); calls++; return false }))
	require.Equal(t, 1, calls)
	for _, limit := range []int{0, 1} {
		require.Equal(t, sql.PrefixDiagnostics(limit), s.PrefixDiagnostics(limit))
	}

	b.AddNode(&graph.Node{ID: "new", Name: "New", Kind: graph.KindFunction})
	require.NoError(t, b.Publish(ctx))
	old, err := s.GetNodePresenceByIDsContext(ctx, []string{"new"})
	require.NoError(t, err)
	require.Empty(t, old)
	next, err := snapshot(t, b).GetNodePresenceByIDsContext(ctx, []string{"new"})
	require.NoError(t, err)
	require.Contains(t, next, "new")
	stopped, cancel := context.WithCancel(ctx)
	cancel()
	_, err = s.GetNodeKindsByIDsContext(stopped, ids)
	require.ErrorIs(t, err, context.Canceled)
	_, err = s.GetNodePresenceByIDsContext(stopped, ids)
	require.ErrorIs(t, err, context.Canceled)
	_, err = s.GetNodeIDsByKindsContext(stopped, ids, kinds)
	require.ErrorIs(t, err, context.Canceled)
	require.ErrorIs(t, s.ScanNodeSearchKeys(stopped, 1, func([]graph.NodeSearchKey) bool { t.Error("cancelled read yielded a page"); return false }), context.Canceled)
	require.NoError(t, s.ReadError())
}
