package store_pebble

import (
	"testing"

	"github.com/cockroachdb/pebble/v2"
	"github.com/stretchr/testify/require"
	"github.com/zzet/gortex/internal/graph"
)

func TestRebuildPlatformGoRelationsWithoutSentinels(t *testing.T) {
	for _, profile := range []Profile{DefaultProfile, FullProfile} {
		t.Run(string(profile), func(t *testing.T) {
			ctx := t.Context()
			const revision = graph.PlatformCatalogRevision
			const lenID = "old::builtin::go::len"
			worker, head, base, owner, _, file, _ := relationsStageFixtureRevision(t, profile, revision)
			worker.AddNode(&graph.Node{ID: file.Path, FilePath: file.Path, Name: file.Path, Kind: graph.KindFile, Language: "go", RepoPrefix: "old"})
			call := &graph.Edge{From: owner.ID, To: "unresolved::len", Kind: graph.EdgeCalls, FilePath: file.Path, Line: 4,
				Meta: map[string]any{"keep": "other data"}}
			call.SetBuiltinReceiverHint(graph.BuiltinReceiverHint{Language: "go", Form: "unbound", Name: "len"})
			shadowed := *call
			shadowed.Line = 5
			shadowed.Meta = nil
			shadowed.SetBuiltinReceiverHint(graph.BuiltinReceiverHint{Language: "go", Form: "shadowed", Name: "len"})
			arg := &graph.Edge{From: "unresolved::len", To: owner.ID, Kind: graph.EdgeArgOf, FilePath: file.Path, Line: call.Line}
			worker.AddEdge(call)
			worker.AddEdge(&shadowed)
			worker.AddEdge(arg)
			require.NoError(t, worker.RebuildGoBuiltins(ctx, true, 1, func(e *graph.Edge, _ func(string) (*graph.Node, error)) (string, error) {
				if e.To == "unresolved::len" && e.Kind == graph.EdgeCalls {
					return lenID, nil
				}
				return "", nil
			}))
			extractor := func(string) string { return "go" }
			require.NoError(t, worker.SettleRebuildPlatformCalls(ctx, extractor))
			bound := *call
			bound.To = lenID
			c, err := readRebuildEdgeCustody(worker.pending, pendingEdgeKey(&bound))
			require.NoError(t, err)
			require.Equal(t, pendingEdgeKey(call), c.raw)
			tables, err := worker.PrepareRelations(ctx, t.TempDir(), base, extractor)
			require.NoError(t, err)
			_, err = head.PublishStage(ctx, tables)
			require.NoError(t, err)
			snap := stageRead(t, head)
			require.Nil(t, snap.GetNode(lenID))
			uses := snap.GetInEdges(lenID)
			require.Len(t, uses, 2)
			for _, edge := range uses {
				status, ok := edge.LinkStatus()
				require.True(t, ok)
				if edge.Line == call.Line {
					require.Equal(t, graph.LinkStatusPlatform, status)
					require.Equal(t, "other data", edge.Meta["keep"])
				} else {
					require.Equal(t, graph.LinkStatusProbablePlatform, status)
				}
			}
			if profile.keepsEdge(graph.EdgeArgOf) {
				from := *arg
				from.From = lenID
				require.Equal(t, []*graph.Edge{&from}, snap.GetOutEdges(lenID))
				ours, _, err := snap.GetInEdgesByNodeIDsFromRepos([]string{owner.ID}, []string{"old"})
				require.NoError(t, err)
				require.Equal(t, []*graph.Edge{&from}, ours[owner.ID])
				foreign, _, err := snap.GetInEdgesByNodeIDsFromRepos([]string{owner.ID}, []string{"foreign"})
				require.NoError(t, err)
				require.Empty(t, foreign[owner.ID])
				require.EqualValues(t, 4, snap.RepoStats()["old"].TotalEdges)
				require.Zero(t, snap.count("repo edges", ""))
			}
		})
	}
}

func TestRebuildPlatformCallValidationAndRefresh(t *testing.T) {
	ctx := t.Context()
	const revision = graph.PlatformCatalogRevision
	worker, head, base, owner, raw, file, _ := relationsStageFixtureRevision(t, FullProfile, revision)
	worker.AddNode(&graph.Node{ID: file.Path, FilePath: file.Path, Kind: graph.KindFile, Language: "go", RepoPrefix: owner.RepoPrefix})
	extractor := func(string) string { return "go" }
	ordinary := *raw
	ordinary.To = owner.ID
	require.NoError(t, worker.ValidateRebuildCallDelta(ctx, &ordinary, revision, extractor))
	ordinary.Meta = map[string]any{graph.MetaLinkStatus: string(graph.LinkStatusResolved)}
	require.ErrorContains(t, worker.ValidateRebuildCallDelta(ctx, &ordinary, revision, extractor), "link_status outside")
	ordinary.Meta = nil
	ordinary.To = "missing::ordinary"
	require.ErrorContains(t, worker.ValidateRebuildCallDelta(ctx, &ordinary, revision, extractor), "invalid retained Call endpoint")

	call := *raw
	call.To = "old::builtin::go::len"
	call.SetBuiltinReceiverHint(graph.BuiltinReceiverHint{Language: "go", Form: "unbound", Name: "len"})
	worker.AddNode(&graph.Node{ID: "other.go::len", FilePath: "other.go", Name: "len", Kind: graph.KindFunction, Language: "go", RepoPrefix: owner.RepoPrefix})
	require.NoError(t, worker.withWrites(func() error {
		return worker.putEdgeWithCustody(&call, rebuildEdgeCustody{proof: rebuildProofObserver})
	}))
	require.NoError(t, worker.SettleRebuildPlatformCalls(ctx, extractor))
	key := pendingEdgeKey(&call)
	saved, err := worker.pendingEdge(key)
	require.NoError(t, err)
	status, ok := saved.LinkStatus()
	require.True(t, ok)
	require.Equal(t, graph.LinkStatusProbablePlatform, status, "package declaration blocks exact status")
	c, err := readRebuildEdgeCustody(worker.pending, key)
	require.NoError(t, err)
	require.Equal(t, rebuildProofObserver, c.proof)
	for _, to := range []string{"foreign::builtin::go::len", "old::builtin::go::doesNotExist", "old::builtin::go::const::nil"} {
		bad := *saved
		bad.To = to
		require.Error(t, worker.ValidateRebuildCallDelta(ctx, &bad, revision, extractor), to)
	}

	// Without the sanctioned refresh, changing only the bytes breaks custody.
	old, err := encodeEdge(&call)
	require.NoError(t, err)
	require.NoError(t, worker.pending.Set(key, old, pebble.NoSync))
	_, err = readRebuildEdgeCustody(worker.pending, key)
	require.ErrorIs(t, err, ErrRebuildUnknownProvenance)
	updated, err := encodeEdge(saved)
	require.NoError(t, err)
	require.NoError(t, worker.pending.Set(key, updated, pebble.NoSync))
	badSource := &graph.Edge{From: call.To, To: owner.ID, Kind: graph.EdgeValueFlow, FilePath: file.Path, Line: 1}
	worker.AddEdge(badSource)
	_, err = worker.PrepareRelations(ctx, t.TempDir(), base, extractor)
	require.ErrorIs(t, err, ErrRelationsSeal)
	require.ErrorContains(t, err, "unproved builtin source repoint")
	snap := stageRead(t, head)
	w, err := snap.Publication()
	require.NoError(t, err)
	require.Equal(t, StageFileGraph, w.Stage)
}
