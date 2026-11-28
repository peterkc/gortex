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
			putTestGoBuiltinScope(t, worker, "old", file.Path, "old")
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
	putTestGoBuiltinScope(t, worker, owner.RepoPrefix, file.Path, "old")
	putTestGoBuiltinScope(t, worker, owner.RepoPrefix, "other.go", "old len")
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
			worker, head, base, owner, raw, file, _ := relationsStageFixtureFile(t, FullProfile, revision, "old."+tc.short, tc.language)
			worker.AddNode(&graph.Node{ID: file.Path, FilePath: file.Path, Name: file.Path, Kind: graph.KindFile, Language: tc.language, RepoPrefix: owner.RepoPrefix})
			extractor := func(p string) string {
				if strings.HasSuffix(p, "."+tc.short) {
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
	worker, _, _, owner, raw, file, _ := relationsStageFixtureFile(t, FullProfile, revision, "old.js", "javascript")
	worker.AddNode(&graph.Node{ID: file.Path, FilePath: file.Path, Name: file.Path, Kind: graph.KindFile, Language: "javascript", RepoPrefix: owner.RepoPrefix})
	worker.AddNode(&graph.Node{ID: "other.js", FilePath: "other.js", Name: "other.js", Kind: graph.KindFile, Language: "javascript", RepoPrefix: "other"})
	js := func(p string) string {
		if strings.HasSuffix(p, ".js") {
			return "javascript"
		}
		return ""
	}
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
	require.ErrorContains(t, worker.validateRebuildPlatformEdge(worker.pending, &invalidRole, revision, js), "role, scope or status differs")
	invalidRole.Meta = call.Meta
	require.ErrorContains(t, worker.validateRebuildPlatformEdge(worker.pending, &invalidRole, revision, js), "link_status outside a builtin Call")
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

func putTestGoBuiltinScope(t *testing.T, worker *BuildStore, repo, file, scope string) {
	t.Helper()
	node := &graph.Node{ID: file, FilePath: file, Name: file, Kind: graph.KindFile, Language: "go", RepoPrefix: repo}
	if scope != "" {
		node.Meta = map[string]any{RebuildGoBuiltinScopeReceiptMeta: scope}
	}
	require.NoError(t, worker.recordRebuildGoBuiltinScope(repo, ParseFacts{
		EffectiveLanguage: "go", File: graph.FileMetaRow{FilePath: file}, Nodes: []*graph.Node{node},
	}))
}

func TestRebuildGoBuiltinPackageScope(t *testing.T) {
	for _, tc := range []struct {
		name, pkg, other string
		want             graph.LinkStatus
	}{
		{"local var in same package", "example", "example", graph.LinkStatusPlatform},
		{"grouped iota in calling test package", "example_test", "example_test len", graph.LinkStatusProbablePlatform},
		{"grouped iota in different test package", "example", "example_test len", graph.LinkStatusPlatform},
		{"unchecked sibling", "example", "", graph.LinkStatusProbablePlatform},
		{"unchecked caller", "", "example", graph.LinkStatusProbablePlatform},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := t.Context()
			worker, _, _, owner, raw, file, _ := relationsStageFixtureRevision(t, FullProfile, graph.PlatformCatalogRevision)
			worker.AddNode(&graph.Node{ID: file.Path, FilePath: file.Path, Name: file.Path, Kind: graph.KindFile, Language: "go", RepoPrefix: owner.RepoPrefix})
			putTestGoBuiltinScope(t, worker, owner.RepoPrefix, file.Path, tc.pkg)
			const other = "other_test.go"
			node := &graph.Node{ID: other, FilePath: other, Name: other, Kind: graph.KindFile, Language: "go", RepoPrefix: owner.RepoPrefix}
			if tc.other != "" {
				node.Meta = map[string]any{RebuildGoBuiltinScopeReceiptMeta: tc.other}
			}
			kind := graph.KindVariable
			if strings.HasSuffix(tc.other, " len") {
				kind = graph.KindEnumMember
			}
			named := &graph.Node{ID: other + "::len", FilePath: other, Name: "len", Kind: kind, Language: "go", RepoPrefix: owner.RepoPrefix}
			require.NoError(t, worker.AddParsedFile(owner.RepoPrefix, ParseFacts{
				EffectiveLanguage: "go", File: graph.FileMetaRow{FilePath: other, NodeCount: 2}, Nodes: []*graph.Node{node, named},
			}))
			stored, err := worker.pendingNode(other)
			require.NoError(t, err)
			require.NotNil(t, stored)
			require.NotContains(t, stored.Meta, RebuildGoBuiltinScopeReceiptMeta)
			call := *raw
			call.To = "old::builtin::go::len"
			call.SetBuiltinReceiverHint(graph.BuiltinReceiverHint{Language: "go", Form: "unbound", Name: "len"})
			worker.AddEdge(&call)
			extractor := func(string) string { return "go" }
			require.NoError(t, worker.SettleRebuildPlatformCalls(ctx, extractor))
			saved, err := worker.pendingEdge(pendingEdgeKey(&call))
			require.NoError(t, err)
			status, ok := saved.LinkStatus()
			require.True(t, ok)
			require.Equal(t, tc.want, status)
			require.NoError(t, worker.ValidateRebuildCallDelta(ctx, saved, graph.PlatformCatalogRevision, extractor))
		})
	}
}

func TestRebuildGoBuiltinScopeRejectsMalformedReceipt(t *testing.T) {
	for _, tc := range []struct {
		name  string
		scope any
		want  string
	}{
		{"non-string", true, "Go builtin package scope receipt"},
		{"empty scope", "", "Go builtin package scope receipt"},
		{"whitespace scope", " ", "Go builtin package identifier"},
		{"invalid package", "invalid-pkg", "Go builtin package identifier"},
		{"invalid declaration", "p notABuiltin", "Go builtin package declaration name"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			worker, _, _, owner, _, _, _ := relationsStageFixture(t, FullProfile)
			const path = "invalid.go"
			node := &graph.Node{ID: path, FilePath: path, Name: path, Kind: graph.KindFile, Language: "go", RepoPrefix: owner.RepoPrefix,
				Meta: map[string]any{RebuildGoBuiltinScopeReceiptMeta: tc.scope}}
			err := worker.AddParsedFile(owner.RepoPrefix, ParseFacts{
				EffectiveLanguage: "go", File: graph.FileMetaRow{FilePath: path, NodeCount: 1}, Nodes: []*graph.Node{node},
			})
			require.ErrorIs(t, err, errInvalidRecord)
			require.ErrorContains(t, err, tc.want)
			stored, err := worker.pendingNode(path)
			require.NoError(t, err)
			require.Nil(t, stored)
		})
	}
}

func TestRebuildGoBuiltinScopeMissingPackageWithoutBlocker(t *testing.T) {
	worker, _, _, owner, _, _, _ := relationsStageFixture(t, FullProfile)
	const path = "other/caller.go"
	worker.AddNode(&graph.Node{ID: path, FilePath: path, Name: path, Kind: graph.KindFile, Language: "go", RepoPrefix: owner.RepoPrefix})
	clear, err := rebuildGoBuiltinPackageClear(worker.pending, path, owner.RepoPrefix, "len")
	require.NoError(t, err)
	require.False(t, clear, "missing caller package is not evidence of absence")
	putTestGoBuiltinScope(t, worker, owner.RepoPrefix, path, "p")
	clear, err = rebuildGoBuiltinPackageClear(worker.pending, path, owner.RepoPrefix, "len")
	require.NoError(t, err)
	require.True(t, clear)
}

func TestRebuildGoLanguageProbeDoesNotBoundNamedNodeMetadata(t *testing.T) {
	worker, _, _, _, _, _, _ := relationsStageFixture(t, FullProfile)
	worker.AddNode(&graph.Node{ID: "a.go", Name: "a", Kind: graph.KindFunction, Language: "go",
		Meta: map[string]any{"body": strings.Repeat("A", 16<<20)}})
	ok, err := worker.rebuildHasNamedGo(t.Context())
	require.NoError(t, err)
	require.True(t, ok)
}

func TestRebuildReceiptNodeDigestIgnoresOnlyPrivateGoScope(t *testing.T) {
	ctx := t.Context()
	worker, _, _, _, _, _, _ := relationsStageFixture(t, FullProfile)
	// The package incumbent census is by ID prefix, not node kind. A file
	// with this spelling can also be an incumbent candidate.
	const id = "external-call::dep::example.go"
	receipt := &graph.Node{ID: id, FilePath: id, Name: id, Kind: graph.KindFile, Language: "go",
		Meta: map[string]any{RebuildGoBuiltinScopeReceiptMeta: "p len", "other": "keep"}}
	facts := ParseFacts{Nodes: []*graph.Node{receipt}, File: graph.FileMetaRow{FilePath: id, NodeCount: 1}, EffectiveLanguage: "go"}
	require.NoError(t, worker.AddParsedFile("", facts))
	pass := rebuildExternalCallPass{b: worker}
	require.NoError(t, pass.receipts(ctx, func(yield func(ParseFacts, error) bool) { yield(facts, nil) }))
	require.NoError(t, pass.provePackage(id))
	live, err := worker.pendingNode(id)
	require.NoError(t, err)
	require.NotNil(t, live)
	live.Meta["other"] = "different"
	worker.AddNode(live)
	require.ErrorContains(t, pass.provePackage(id), "unproved retained external-call package incumbent")
}
