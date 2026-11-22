package store_pebble

import (
	"bytes"
	"path/filepath"
	"slices"
	"testing"

	"github.com/cockroachdb/pebble/v2"
	"github.com/stretchr/testify/require"
	"github.com/zzet/gortex/internal/graph"
)

func TestRebuildSearchStageMatchesLegacySeal(t *testing.T) {
	for _, profile := range []Profile{DefaultProfile, FullProfile} {
		t.Run(string(profile), func(t *testing.T) {
			ctx := t.Context()
			worker, head, base, declaration, _, _, _ := relationsStageFixture(t, profile)
			nodes := []*graph.Node{
				{ID: "src.go::Run", Name: "Run", Kind: graph.KindFunction, RepoPrefix: "new", FilePath: "new/src.go"},
				{ID: "src.go::Run#param:p", Name: "p", Kind: graph.KindParam, RepoPrefix: "new", FilePath: "new/src.go"},
				{ID: "external-call::dep::example.org/lib", Name: "Lib", Kind: graph.KindModule},
			}
			worker.AddBatch(nodes, nil)
			derived := publishRebuildSearchDerived(t, worker, head, base)
			pinned := stageRead(t, head)
			require.NoError(t, worker.UpsertSymbolFTS(declaration.ID, "stale receipt"))
			wantTokens := map[string]string{nodes[0].ID: "run arg", nodes[1].ID: "arg", nodes[2].ID: "lib"}
			stage, err := worker.PrepareSearch(ctx, t.TempDir(), derived.Witness, derived.Incarnation, func(n *graph.Node) (string, bool) {
				text, ok := wantTokens[n.ID]
				return text, ok
			})
			require.NoError(t, err)
			require.NotEmpty(t, stage.Files)
			_, err = worker.OpenSnapshot()
			require.ErrorIs(t, err, ErrUnpublishedBuild)
			_, err = head.PublishStage(ctx, stage)
			require.NoError(t, err)
			current := stageRead(t, head)
			w, err := current.Publication()
			require.NoError(t, err)
			require.Equal(t, StageSearch, w.Stage)
			require.Equal(t, derived.Witness.Version(), w.Version())
			require.Equal(t, w.Families, relationsSavedWitness(t, current))
			before, err := pinned.Publication()
			require.NoError(t, err)
			require.Equal(t, derived.Witness, before)
			var pending *ErrStagePending
			require.ErrorAs(t, pinned.RequireStage(StageSearch), &pending)
			require.Equal(t, StageDerived, pending.Published)
			require.Empty(t, rebuildSearchRows(t, pinned, []byte{familySymbol}))

			// Independent, explicit token rows in reverse node and document order.
			legacy, err := OpenBuild(filepath.Join(t.TempDir(), "legacy"), profile)
			require.NoError(t, err)
			t.Cleanup(func() { require.NoError(t, legacy.Close()) })
			legacy.AddNode(declaration)
			for _, n := range slices.Backward(nodes) {
				legacy.AddNode(n)
				require.NoError(t, legacy.UpsertSymbolFTS(n.ID, wantTokens[n.ID]))
			}
			require.NoError(t, legacy.Publish(ctx))
			oracle, err := legacy.OpenSnapshot()
			require.NoError(t, err)
			t.Cleanup(func() { require.NoError(t, oracle.Close()) })
			saved := rebuildSearchRows(t, current, []byte{familySymbol})
			require.NotEmpty(t, saved)
			require.Equal(t, rebuildSearchRows(t, oracle, []byte{familySymbol}), saved)
			stats, err := point(current, searchKey("stats"), searchReadStats)
			require.NoError(t, err)
			require.Equal(t, searchStats{Rows: 3, Tokens: 4, Count: 3, Repos: []string{"", "new", "old"}}, stats)
			docs := make(map[string]bool)
			for _, r := range rebuildSearchRows(t, current, searchKey("doc")) {
				d, err := searchReadDoc(r.value)
				require.NoError(t, err)
				docs[d.ID] = d.NodePresent
			}
			require.Equal(t, map[string]bool{nodes[0].ID: true, nodes[1].ID: profile == FullProfile, nodes[2].ID: true}, docs)
		})
	}
}

func TestRebuildSearchStageCorruptNodeDoesNotChangeHead(t *testing.T) {
	worker, head, base, declaration, _, _, _ := relationsStageFixture(t, FullProfile)
	derived := publishRebuildSearchDerived(t, worker, head, base)
	prior := relationsSavedRows(t, stageRead(t, head))
	require.NotEmpty(t, prior)
	require.NoError(t, worker.pending.Set(pendingNodeKey(declaration.ID), []byte{0xff}, pebble.NoSync))
	dir := t.TempDir()
	stage, err := worker.PrepareSearch(t.Context(), dir, derived.Witness, derived.Incarnation, func(*graph.Node) (string, bool) {
		return "not published", true
	})
	require.ErrorIs(t, err, ErrSearchSeal)
	require.ErrorIs(t, err, ErrRebuildView)
	require.ErrorIs(t, err, errInvalidRecord)
	require.Empty(t, stage)
	require.Empty(t, rebuildSearchRows(t, stageRead(t, head), []byte{familySymbol}))
	require.Equal(t, prior, relationsSavedRows(t, stageRead(t, head)))
}

func publishRebuildSearchDerived(t *testing.T, worker *BuildStore, head *Head, base FileGraphBase) StageTables {
	t.Helper()
	ctx := t.Context()
	relations, err := worker.PrepareRelations(ctx, t.TempDir(), base)
	require.NoError(t, err)
	digest, err := head.PublishStage(ctx, relations)
	require.NoError(t, err)
	w := relations.Witness
	w.Stage = StageDerived // This fixture has no derived rows.
	derived := StageTables{Dir: t.TempDir(), Incarnation: relations.Incarnation, Base: digest, Witness: w}
	_, err = head.PublishStage(ctx, derived)
	require.NoError(t, err)
	return derived
}

func rebuildSearchRows(t *testing.T, s *Snapshot, prefix []byte) []row {
	t.Helper()
	var rows []row
	require.NoError(t, scan(t.Context(), s, prefix, func(key, value []byte) (bool, error) {
		rows = append(rows, row{bytes.Clone(key), bytes.Clone(value)})
		return true, nil
	}))
	return rows
}
