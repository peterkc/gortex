package store_pebble

import (
	"crypto/sha256"
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

func TestRebuildContractOverlayRefusesInvalidState(t *testing.T) {
	const id, file = "env::OVERLAY", "settings.rb"
	ids := map[string]string{id: file}
	for _, tc := range []struct {
		name, detail string
		setup        func(*testing.T, *BuildStore)
		run          func(*testing.T, *BuildStore) error
	}{
		{
			name: "nil ids", detail: "missing ids",
			run: func(t *testing.T, b *BuildStore) error {
				return b.BeginRebuildContractOverlay(t.Context(), nil)
			},
		},
		{
			name: "already active", detail: "active overlay",
			setup: func(t *testing.T, b *BuildStore) {
				require.NoError(t, b.BeginRebuildContractOverlay(t.Context(), ids))
			},
		},
		{
			name: "contracts started", detail: "started contracts stage",
			setup: func(_ *testing.T, b *BuildStore) {
				b.contractsStarted = true
			},
		},
		{
			name: "undo range occupied", detail: "undo range occupied",
			setup: func(t *testing.T, b *BuildStore) {
				require.NoError(t, b.pending.Set(rebuildContractUndoPrefix, nil, pebble.NoSync))
			},
		},
		{
			name: "empty id", detail: "invalid contract id",
			run: func(t *testing.T, b *BuildStore) error {
				return b.BeginRebuildContractOverlay(t.Context(), map[string]string{"": file})
			},
		},
		{
			name: "proxy id", detail: "invalid contract id",
			run: func(t *testing.T, b *BuildStore) error {
				return b.BeginRebuildContractOverlay(t.Context(), map[string]string{"remote:repo~proxy": file})
			},
		},
		{
			name: "displaced identity", detail: "displaced node identity",
			setup: func(t *testing.T, b *BuildStore) {
				wrong, err := encodeNode(&graph.Node{ID: "different", Kind: graph.KindVariable, FilePath: file})
				require.NoError(t, err)
				require.NoError(t, b.pending.Set(pendingNodeKey(id), wrong, pebble.NoSync))
			},
		},
		{
			name: "not active", detail: "no active overlay",
			run: func(t *testing.T, b *BuildStore) error {
				return b.EndRebuildContractOverlay(t.Context())
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			b := openContractOverlayFixture(t)
			if tc.setup != nil {
				tc.setup(t, b)
			}
			before := contractOverlayBytes(t, b)
			var err error
			if tc.run != nil {
				err = tc.run(t, b)
			} else {
				err = b.BeginRebuildContractOverlay(t.Context(), ids)
			}
			require.ErrorIs(t, err, ErrRebuildContractOverlay)
			require.ErrorContains(t, err, tc.detail)
			require.Equal(t, before, contractOverlayBytes(t, b))
		})
	}
}

func TestRebuildContractOverlayRefusesInvalidUndo(t *testing.T) {
	for _, tc := range []struct {
		name, key string
		value     []byte
	}{
		{name: "empty restore key", value: make([]byte, sha256.Size)},
		{name: "short digest", key: "short", value: make([]byte, sha256.Size-1)},
		{name: "invalid presence", key: "presence", value: make([]byte, sha256.Size+1)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			b := openContractOverlayFixture(t)
			ctx := t.Context()
			require.NoError(t, b.BeginRebuildContractOverlay(ctx, map[string]string{"env::OVERLAY": "settings.rb"}))
			key := append(append([]byte(nil), rebuildContractUndoPrefix...), tc.key...)
			require.NoError(t, b.pending.Set(key, tc.value, pebble.NoSync))
			before := contractOverlayBytes(t, b)
			err := b.EndRebuildContractOverlay(ctx)
			require.ErrorIs(t, err, ErrRebuildContractOverlay)
			require.ErrorContains(t, err, "invalid undo row")
			require.Equal(t, before, contractOverlayBytes(t, b))
		})
	}
}

func TestRebuildContractOverlayCannotSealSearch(t *testing.T) {
	b := openContractOverlayFixture(t)
	ctx := t.Context()
	require.NoError(t, b.BeginRebuildContractOverlay(ctx, map[string]string{"env::OVERLAY": "settings.rb"}))
	w := Witness{
		Stage:       StageDerived,
		Profile:     b.profile,
		Input:       HeadDigest{1},
		Recipe:      HeadDigest{2},
		Unsupported: deferredFamilies(false),
	}
	before := contractOverlayBytes(t, b)
	tables, err := b.PrepareSearch(ctx, t.TempDir(), w, testHeadIncarnation, func(*graph.Node) (string, bool) {
		t.Fatal("active overlay reached the search renderer")
		return "", false
	})
	require.ErrorIs(t, err, ErrSearchSeal)
	require.ErrorIs(t, err, ErrRebuildContractOverlay)
	require.Equal(t, StageTables{}, tables)
	require.Equal(t, before, contractOverlayBytes(t, b))
}
