package store_pebble

import (
	"testing"

	"github.com/cockroachdb/pebble/v2"
	"github.com/stretchr/testify/require"
	"github.com/zzet/gortex/internal/graph"
)

func openContractOverlayFixture(t *testing.T) *BuildStore {
	t.Helper()
	b, err := OpenBuild(t.TempDir(), FullProfile)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, b.Close()) })
	return b
}

func contractOverlayBytes(t *testing.T, b *BuildStore) map[string]string {
	t.Helper()
	rows := make(map[string]string)
	require.NoError(t, pendingVisit(t.Context(), b.pending, nil, func(r row) error {
		rows[string(r.key)] = string(r.value)
		return nil
	}))
	return rows
}

func TestRebuildContractOverlayRestoresPrivateBytes(t *testing.T) {
	const id, extra, from, oldTo, newTo = "env::COLLISION", "env::NEW", "caller", "unresolved::old", "unresolved::new"
	const oldFile, newFile = "src/main.go", "config/settings.rb"
	old := &graph.Node{
		ID: id, Name: "Collision", QualName: "main.Collision", Kind: graph.KindMethod,
		Language: "go", FilePath: oldFile, RepoPrefix: "displaced",
		StartLine: 27, Meta: map[string]any{"test_stamp": "kept byte for byte"},
	}
	edge := &graph.Edge{From: from, To: oldTo, Kind: graph.EdgeCalls, FilePath: oldFile, Line: 80}
	moved := *edge
	moved.To = newTo
	ctx := t.Context()
	moves := []graph.EdgeReindex{{Edge: &moved, OldTo: oldTo}}
	b, control := openContractOverlayFixture(t), openContractOverlayFixture(t)
	for _, store := range []*BuildStore{b, control} {
		store.AddNode(old)
		store.AddEdge(edge)
	}
	before := contractOverlayBytes(t, b)
	require.NotEmpty(t, before)
	require.Equal(t, before, contractOverlayBytes(t, control))
	require.NoError(t, control.ApplyLinkReindexes(ctx, moves))
	want := contractOverlayBytes(t, control)
	require.NotEqual(t, before, want, "control made a real edge move")

	require.NoError(t, b.BeginRebuildContractOverlay(ctx, map[string]string{id: newFile, extra: newFile}))
	snapshot := b.pending.NewSnapshot()
	defer snapshot.Close()
	for _, nodeID := range []string{id, extra} {
		got, err := pendingGet(snapshot, pendingNodeKey(nodeID), decodeNode)
		require.NoError(t, err)
		require.Equal(t, &graph.Node{ID: nodeID, Name: nodeID, Kind: graph.KindContract, FilePath: newFile, Language: "contract"}, got)
	}
	for _, key := range [][]byte{
		keyStrings([]byte{familyFile}, oldFile, id),
		keyStrings([]byte{familyRepo}, old.RepoPrefix, id),
		rebuildCandidateKey(nameRebuildScoped, old),
		rebuildCandidateKey(nameQualified, old),
	} {
		v, err := pendingGet(snapshot, key, copyValue)
		require.NoError(t, err)
		require.Nil(t, v)
	}
	require.NoError(t, b.ApplyLinkReindexes(ctx, moves))
	require.NoError(t, b.EndRebuildContractOverlay(ctx))
	require.Equal(t, want, contractOverlayBytes(t, b))
}

func TestRebuildContractOverlayRefusesInterveningChange(t *testing.T) {
	const first, second, file = "env::FIRST", "env::SECOND", "settings.rb"
	for _, name := range []string{"replace node", "delete node", "delete index", "revive displaced index"} {
		t.Run(name, func(t *testing.T) {
			b := openContractOverlayFixture(t)
			b.AddNode(&graph.Node{ID: second, Name: "second", Kind: graph.KindFunction, FilePath: "old.go", Language: "go"})
			ctx := t.Context()
			require.NoError(t, b.BeginRebuildContractOverlay(ctx, map[string]string{first: file, second: file}))
			switch name {
			case "replace node":
				b.AddNode(&graph.Node{ID: second, Name: "new", Kind: graph.KindType, FilePath: file})
			case "delete node":
				require.NoError(t, b.pending.Delete(pendingNodeKey(second), pebble.NoSync))
			case "delete index":
				require.NoError(t, b.pending.Delete(keyStrings([]byte{familyFile}, file, second), pebble.NoSync))
			case "revive displaced index":
				require.NoError(t, b.pending.Set(keyStrings([]byte{familyFile}, "old.go", second), []byte(second), pebble.NoSync))
			}
			changed := contractOverlayBytes(t, b)
			require.ErrorIs(t, b.EndRebuildContractOverlay(ctx), ErrRebuildContractOverlay)
			require.Equal(t, changed, contractOverlayBytes(t, b), "nothing is restored after any mismatch")
		})
	}
}

func TestRebuildContractOverlayCannotSeal(t *testing.T) {
	b := openContractOverlayFixture(t)
	ctx, dir := t.Context(), t.TempDir()
	require.NoError(t, b.BeginRebuildContractOverlay(ctx, map[string]string{}))
	_, err := b.PrepareFileGraph(ctx, dir, Witness{}, "")
	require.ErrorIs(t, err, ErrRebuildContractOverlay)
	_, err = b.PrepareRelations(ctx, dir, FileGraphBase{}, nil)
	require.ErrorIs(t, err, ErrRebuildContractOverlay)
	require.ErrorIs(t, b.Publish(ctx), ErrRebuildContractOverlay)
	base := RelationsBase{files: FileGraphBase{build: b}}
	_, err = b.PrepareContracts(ctx, dir, base, func(func(RebuildContractBatch, error) bool) {
		t.Fatal("active overlay reached contract batch iteration")
	}, ContractCoverage{})
	require.ErrorIs(t, err, ErrRebuildContractOverlay)
}
