package store_pebble

import (
	"bytes"
	"testing"

	"github.com/cockroachdb/pebble/v2"
	"github.com/stretchr/testify/require"
	"github.com/zzet/gortex/internal/graph"
)

func TestRebuildCustodyGenericReindex(t *testing.T) {
	for _, tc := range []struct {
		name      string
		kind      graph.EdgeKind
		missing   bool
		untracked bool
	}{
		{name: "missing-old", kind: graph.EdgeCalls, missing: true, untracked: true},
		{name: "membership-move-without-raw", kind: graph.EdgeMemberOf, untracked: true},
		{name: "non-reader-move-without-raw", kind: graph.EdgeDefines},
	} {
		t.Run(tc.name, func(t *testing.T) {
			b := newBuild(t, FullProfile)
			old := graph.Edge{
				From:     "caller",
				To:       "old",
				Kind:     tc.kind,
				FilePath: "file.ts",
				Line:     9,
			}
			live := old
			if !tc.missing {
				b.AddEdge(&old)
				live.To = "new"
			}
			b.ReindexEdges([]graph.EdgeReindex{{Edge: &live, OldTo: old.To}})
			key := pendingEdgeKey(&live)
			saved, err := b.pendingEdge(key)
			require.NoError(t, err)
			require.Equal(t, &live, saved)
			got, err := readRebuildEdgeCustody(b.pending, key)
			require.NoError(t, err)
			var want rebuildEdgeCustody
			if tc.untracked {
				want.proof = rebuildProofUntrackedMove
			}
			require.Equal(t, want, got)
		})
	}
}

func TestRebuildCustodyGenericMoveDropsStageProof(t *testing.T) {
	b, old := seedClosureMutationCustody(t)
	live := old
	live.To = "other-registrar"
	b.ReindexEdges([]graph.EdgeReindex{{Edge: &live, OldTo: old.To}})

	key := pendingEdgeKey(&live)
	saved, err := b.pendingEdge(key)
	require.NoError(t, err)
	require.Equal(t, &live, saved)
	got, err := readRebuildEdgeCustody(b.pending, key)
	require.NoError(t, err)
	require.Equal(t, rebuildEdgeCustody{proof: rebuildProofUntrackedMove}, got)
	oldKey := pendingEdgeKey(&old)
	saved, err = b.pendingEdge(oldKey)
	require.NoError(t, err)
	require.Nil(t, saved)
	got, err = readRebuildEdgeCustody(b.pending, oldKey)
	require.NoError(t, err)
	require.Zero(t, got)
}

func TestRebuildCustodySameKeyWriters(t *testing.T) {
	b, live := seedClosureMutationCustody(t)
	key := pendingEdgeKey(&live)
	recorder := rebuildLinkRecorderKey(key)
	original, err := pendingGet(b.pending, recorder, func(raw []byte) ([]byte, error) {
		return bytes.Clone(raw), nil
	})
	require.NoError(t, err)
	require.NotEmpty(t, original)

	wantCustody := rebuildEdgeCustody{proof: rebuildProofClosureCollection}
	check := func(writer string) {
		t.Helper()
		saved, err := b.pendingEdge(key)
		require.NoError(t, err, writer)
		require.Equal(t, &live, saved, writer)
		got, err := readRebuildEdgeCustody(b.pending, key)
		require.NoError(t, err, writer)
		require.Equal(t, wantCustody, got, writer)
	}

	changed := live
	require.True(t, b.SetEdgeProvenance(&changed, graph.OriginASTInferred))
	live.Origin = graph.OriginASTInferred
	check("SetEdgeProvenance")

	live.Confidence = 0.75
	b.PersistEdgeAttributes(&live)
	check("PersistEdgeAttributes")

	live.Via = "refreshed via"
	b.ReindexEdges([]graph.EdgeReindex{{Edge: &live, OldTo: live.To}})
	check("same-key ReindexEdges")

	// The guard clears a stationary winner, not the competing revert payload.
	competitor := live
	competitor.To, competitor.Via = "resolved", "competing row"
	b.AddEdge(&competitor)
	revert := competitor
	revert.To = live.To
	moves := []graph.EdgeReindex{{Edge: &revert, OldTo: competitor.To}}
	_, err = b.ApplyRebuildGuardPage(t.Context(), moves, nil)
	require.NoError(t, err)
	live.Origin = ""
	check("guard destination Origin clear")
	removed, err := b.pendingEdge(pendingEdgeKey(&competitor))
	require.NoError(t, err)
	require.Nil(t, removed)

	// Reinstall the real stage's pre-write stamp over the now-different row.
	require.NoError(t, b.pending.Set(recorder, original, pebble.NoSync))
	got, err := readRebuildEdgeCustody(b.pending, key)
	var unknown *RebuildUnknownProvenanceError
	require.ErrorAs(t, err, &unknown)
	require.Equal(t, graph.EdgeIdentityFor(&live), unknown.First)
	require.EqualValues(t, 1, unknown.Count)
	require.Zero(t, got)

	before := rebuildFrameworkCustodyRows(t, b)
	changed = live
	require.False(t, b.SetEdgeProvenance(&changed, graph.OriginASTInferred))
	require.ErrorIs(t, b.first, ErrRebuildUnknownProvenance)
	require.Equal(t, live, changed)
	require.Equal(t, before, rebuildFrameworkCustodyRows(t, b))
}

func TestRebuildCustodyGuardReverts(t *testing.T) {
	for _, tc := range []struct {
		name      string
		kind      graph.EdgeKind
		to        string
		missing   bool
		untracked bool
	}{
		{name: "stationary", kind: graph.EdgeCalls, to: "old"},
		{name: "same key without old row", kind: graph.EdgeCalls, to: "old", missing: true, untracked: true},
		{name: "moved-call", kind: graph.EdgeCalls, to: "unresolved::new", untracked: true},
		{name: "non-reader", kind: graph.EdgeDefines, to: "new"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			b := newBuild(t, FullProfile)
			old := graph.Edge{
				From:     "caller",
				To:       "old",
				Kind:     tc.kind,
				FilePath: "file.ts",
				Line:     9,
				Origin:   graph.OriginASTInferred,
			}
			if !tc.missing {
				b.AddEdge(&old)
			}
			live := old
			live.To = tc.to
			moves := []graph.EdgeReindex{{Edge: &live, OldTo: old.To}}
			_, err := b.ApplyRebuildGuardPage(t.Context(), moves, nil)
			require.NoError(t, err)

			key := pendingEdgeKey(&live)
			saved, err := b.pendingEdge(key)
			require.NoError(t, err)
			want := old
			want.To, want.Origin = tc.to, ""
			require.Equal(t, &want, saved)
			got, err := readRebuildEdgeCustody(b.pending, key)
			require.NoError(t, err)
			var wantCustody rebuildEdgeCustody
			if tc.untracked {
				wantCustody.proof = rebuildProofUntrackedMove
			}
			require.Equal(t, wantCustody, got)
		})
	}
}

func TestRebuildCustodyRecordValidation(t *testing.T) {
	b := newBuild(t, FullProfile)
	e := &graph.Edge{From: "caller", To: "callee", Kind: graph.EdgeCalls, FilePath: "file.ts"}
	b.AddEdge(e)
	key := pendingEdgeKey(e)
	recorder := rebuildLinkRecorderKey(key)
	payload, err := encodeEdge(e)
	require.NoError(t, err)
	stamp := rebuildEdgeCustody{proof: rebuildProofClosureCollection}
	valid, err := stamp.encode(payload)
	require.NoError(t, err)

	badVersion := bytes.Clone(valid)
	badVersion[0]++
	zeroCode, limitCode := bytes.Clone(valid), bytes.Clone(valid)
	zeroCode[1], limitCode[1] = 0, byte(rebuildProofLimit)
	for _, tc := range []struct {
		name  string
		value []byte
	}{
		{name: "short", value: valid[:len(valid)-1]},
		{name: "version", value: badVersion},
		{name: "zero-code", value: zeroCode},
		{name: "exclusive-limit", value: limitCode},
		{name: "truncated-raw-key", value: []byte{familyEdge, edgeOut}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			require.NoError(t, b.pending.Set(recorder, tc.value, pebble.NoSync))
			got, err := readRebuildEdgeCustody(b.pending, key)
			require.ErrorIs(t, err, errInvalidRecord)
			require.Zero(t, got)
		})
	}

	_, err = (rebuildEdgeCustody{proof: rebuildProofLimit}).encode(payload)
	require.ErrorIs(t, err, errInvalidRecord)
	stamp.raw = key
	_, err = stamp.encode(payload)
	require.ErrorIs(t, err, errInvalidRecord)

	// Leave a valid stamp but remove its live row without deleting the recorder.
	require.NoError(t, b.pending.Set(recorder, valid, pebble.NoSync))
	require.NoError(t, b.pending.Delete(key, pebble.NoSync))
	got, err := readRebuildEdgeCustody(b.pending, key)
	require.ErrorIs(t, err, ErrRebuildUnknownProvenance)
	require.Zero(t, got)
}

func seedClosureMutationCustody(t *testing.T) (*BuildStore, graph.Edge) {
	t.Helper()
	const file = "bus.swift"
	dispatch := &graph.Node{
		ID: "dispatch", Kind: graph.KindMethod, FilePath: file, Language: "swift",
		Meta: map[string]any{"cc_dispatch_field": "listeners"},
	}
	registrar := &graph.Node{
		ID: "registrar", Kind: graph.KindMethod, FilePath: file, Language: "swift",
		Meta: map[string]any{"cc_append_field": "listeners"},
	}
	makeCall := func(d, r *graph.Node, field string) *graph.Edge {
		return &graph.Edge{
			From: d.ID, To: r.ID, Kind: graph.EdgeCalls, FilePath: d.FilePath,
			Origin: graph.OriginSpeculative,
			Meta:   map[string]any{"channel_field": field},
		}
	}
	b, receipts := seedSyntheticCustody(t, ParseFacts{
		EffectiveLanguage: "swift",
		File:              graph.FileMetaRow{FilePath: file},
		Nodes:             []*graph.Node{dispatch, registrar},
	})
	stats, err := b.RebuildClosureCollection(t.Context(), receipts, 1, 8,
		func(d, r *graph.Node) bool { return d.WorkspaceID == r.WorkspaceID }, makeCall, closureTestApple)
	require.NoError(t, err)
	require.EqualValues(t, 1, stats.Edges)
	e := makeCall(dispatch, registrar, "listeners")
	got, err := readRebuildEdgeCustody(b.pending, pendingEdgeKey(e))
	require.NoError(t, err)
	require.Equal(t, rebuildEdgeCustody{proof: rebuildProofClosureCollection}, got)
	return b, *e
}
