package store_pebble

import (
	"strings"
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

func TestRebuildPlatformWebRelationsWithoutSentinels(t *testing.T) {
	for _, tc := range []struct{ language, short, other string }{
		{"javascript", "js", "typescript"},
		{"typescript", "ts", "javascript"},
	} {
		t.Run(tc.language, func(t *testing.T) {
			ctx := t.Context()
			const revision = graph.PlatformCatalogRevision
			worker, head, base, owner, raw, file, _ := relationsStageFixtureRevision(t, FullProfile, revision)
			worker.AddNode(&graph.Node{ID: file.Path, FilePath: file.Path, Name: file.Path, Kind: graph.KindFile, Language: tc.language, RepoPrefix: owner.RepoPrefix})
			extractor := func(p string) string {
				if p == file.Path {
					return tc.language
				}
				return ""
			}
			exact := &graph.Edge{From: owner.ID, To: graph.StubID(owner.RepoPrefix, graph.StubKindBuiltin, tc.short, "array", "slice"),
				Kind: graph.EdgeCalls, FilePath: file.Path, Line: raw.Line + 1,
				Meta: map[string]any{"keep": "other data", graph.MetaLinkStatus: string(graph.LinkStatusPlatform)}}
			exact.SetBuiltinReceiverHint(graph.BuiltinReceiverHint{Language: tc.language, Form: "instance", Family: "array"})
			probable := &graph.Edge{From: owner.ID, To: graph.StubID(owner.RepoPrefix, graph.StubKindBuiltin, tc.short, "array", "push"),
				Kind: graph.EdgeCalls, FilePath: file.Path, Line: raw.Line + 2,
				Meta: map[string]any{graph.MetaLinkStatus: string(graph.LinkStatusProbablePlatform)}}
			probable.SetBuiltinReceiverHint(graph.BuiltinReceiverHint{Language: tc.other, Form: "shadowed"})
			read := &graph.Edge{From: owner.ID, To: probable.To, Kind: graph.EdgeReferences, FilePath: file.Path, Line: raw.Line + 3}
			worker.AddEdge(exact)
			worker.AddEdge(probable)
			worker.AddEdge(read)
			for _, call := range []*graph.Edge{exact, probable} {
				require.NoError(t, worker.ValidateRebuildCallDelta(ctx, call, revision, extractor))
			}
			require.NoError(t, worker.SettleRebuildPlatformCalls(ctx, extractor))
			stage, err := worker.PrepareRelations(ctx, t.TempDir(), base, extractor)
			require.NoError(t, err)
			_, err = head.PublishStage(ctx, stage)
			require.NoError(t, err)
			snap := stageRead(t, head)
			require.Nil(t, snap.GetNode(exact.To))
			require.Nil(t, snap.GetNode(probable.To))
			require.Equal(t, []*graph.Edge{exact}, snap.GetInEdges(exact.To))
			require.ElementsMatch(t, []*graph.Edge{probable, read}, snap.GetInEdges(probable.To))
		})
	}
}

func TestRebuildPlatformWebCallRefusals(t *testing.T) {
	ctx := t.Context()
	const revision = graph.PlatformCatalogRevision
	worker, _, _, owner, raw, file, _ := relationsStageFixtureRevision(t, FullProfile, revision)
	worker.AddNode(&graph.Node{ID: file.Path, FilePath: file.Path, Name: file.Path, Kind: graph.KindFile, Language: "javascript", RepoPrefix: owner.RepoPrefix})
	worker.AddNode(&graph.Node{ID: "other.js", FilePath: "other.js", Name: "other.js", Kind: graph.KindFile, Language: "javascript", RepoPrefix: "other"})
	js := func(string) string { return "javascript" }
	call := *raw
	call.To = "old::builtin::js::array/string::slice"
	call.Meta = map[string]any{graph.MetaLinkStatus: string(graph.LinkStatusProbablePlatform)}
	require.NoError(t, worker.ValidateRebuildCallDelta(ctx, &call, revision, js))
	require.ErrorContains(t, worker.ValidateRebuildCallDelta(ctx, &call, revision, nil), "no recording extractor")
	require.ErrorContains(t, worker.ValidateRebuildCallDelta(ctx, &call, revision, func(string) string { return "vue" }), "language is not admitted")
	require.ErrorContains(t, worker.ValidateRebuildCallDelta(ctx, &call, revision+1, js), "unknown builtin target name")
	require.NoError(t, worker.ValidateRebuildCallDelta(ctx, raw, revision, js), "ordinary unresolved Call")

	for _, tc := range []struct {
		name string
		edit func(*graph.Edge)
		want string
	}{
		{"different extractor", func(e *graph.Edge) { e.To = "old::builtin::ts::array/string::slice" }, "language differs"},
		{"malformed target", func(e *graph.Edge) { e.To = "old::builtin::js::slice" }, "malformed builtin target"},
		{"unknown name", func(e *graph.Edge) { e.To = "old::builtin::js::array::gortexUnknown" }, "unknown builtin target name"},
		{"wrong target scope", func(e *graph.Edge) { e.To = "other::builtin::js::array/string::slice" }, "scope or status differs"},
		{"wrong recording scope", func(e *graph.Edge) { e.FilePath = "other.js" }, "recording scope differs"},
		{"missing source", func(e *graph.Edge) { e.From = "missing" }, "no ordinary source owner"},
		{"missing file", func(e *graph.Edge) { e.FilePath = "missing.js" }, "recording scope differs"},
		{"wrong status", func(e *graph.Edge) { e.Meta[graph.MetaLinkStatus] = string(graph.LinkStatusPlatform) }, "scope or status differs"},
		{"missing status", func(e *graph.Edge) { delete(e.Meta, graph.MetaLinkStatus) }, "scope or status differs"},
		{"proxy unresolved target", func(e *graph.Edge) { e.To = graph.ProxyNodeID("remote", "x::unresolved::f"); e.Meta = nil }, "proxy retained Call endpoint"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			edge := call
			edge.Meta = map[string]any{graph.MetaLinkStatus: string(graph.LinkStatusProbablePlatform)}
			tc.edit(&edge)
			require.ErrorContains(t, worker.ValidateRebuildCallDelta(ctx, &edge, revision, js), tc.want)
		})
	}
	for _, form := range []string{"import", "shadowed", "user"} {
		t.Run(form, func(t *testing.T) {
			edge := call
			edge.Meta = map[string]any{graph.MetaLinkStatus: string(graph.LinkStatusProbablePlatform)}
			edge.SetBuiltinReceiverHint(graph.BuiltinReceiverHint{Language: "javascript", Form: form})
			require.ErrorContains(t, worker.ValidateRebuildCallDelta(ctx, &edge, revision, js), "contrary receiver evidence")
		})
	}
	invalidRole := call
	invalidRole.Kind = graph.EdgeTypedAs
	invalidRole.Meta = nil
	worker.AddEdge(&invalidRole)
	require.NoError(t, worker.SettleRebuildPlatformCalls(ctx, js), "non-Calls do not enter the status sweep")
	require.ErrorContains(t, worker.validateRebuildPlatformEdge(ctx, worker.pending, &invalidRole, revision, js), "role, scope or status differs")
	invalidRole.Meta = call.Meta
	require.ErrorContains(t, worker.validateRebuildPlatformEdge(ctx, worker.pending, &invalidRole, revision, js), "link_status outside a builtin Call")
}

func TestRebuildPlatformCallRejectsSizeBeforeDecode(t *testing.T) {
	worker, _, _, _, raw, _, _ := relationsStageFixture(t, FullProfile)
	// Corrupt and oversized. The explicit byte bound must win over decoding.
	require.NoError(t, worker.pending.Set(pendingEdgeKey(raw), make([]byte, 16<<20), pebble.NoSync))
	err := worker.SettleRebuildPlatformCalls(t.Context(), nil)
	require.ErrorIs(t, err, ErrRebuildLinkInputSize)
	require.ErrorContains(t, err, "platform Call")
}

func TestRebuildGoLanguageProbeDoesNotBoundUnrelatedNode(t *testing.T) {
	worker, _, _, _, _, _, _ := relationsStageFixture(t, FullProfile)
	// Lexically earlier than the fixture's Go declarations.
	worker.AddNode(&graph.Node{ID: "a.js", Name: "a", Kind: graph.KindFunction, Language: "javascript",
		Meta: map[string]any{"body": strings.Repeat("A", 16<<20)}})
	ok, err := worker.rebuildHasNamedGo(t.Context())
	require.NoError(t, err)
	require.True(t, ok)
}

func TestRebuildBuiltinNodeCannotBePublished(t *testing.T) {
	worker, _, base, _, _, _, _ := relationsStageFixture(t, FullProfile)
	worker.AddNode(&graph.Node{ID: "old::builtin::go::len", Name: "len", Kind: graph.KindBuiltin, Language: "go"})
	_, err := worker.PrepareRelations(t.Context(), t.TempDir(), base, nil)
	require.ErrorIs(t, err, ErrRelationsSeal)
	require.ErrorContains(t, err, "builtin node cannot be published")
}
