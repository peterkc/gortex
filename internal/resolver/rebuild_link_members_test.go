package resolver

import (
	"context"
	"maps"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/graph/store_sqlite"
)

func TestRebuildMemberNativeRows(t *testing.T) {
	const goFile, cFile, csFile, tsFile = "src/caller.go", "src/caller.c", "src/caller.cs", "src/caller.ts"
	const field, method, fn = graph.KindField, graph.KindMethod, graph.KindFunction
	const reads, writes, refs, calls = graph.EdgeReads, graph.EdgeWrites, graph.EdgeReferences, graph.EdgeCalls
	node := func(id, name, file, lang, receiver string, kind graph.NodeKind) *graph.Node {
		return &graph.Node{ID: id, Name: name, QualName: id, FilePath: file, Language: lang,
			RepoPrefix: "repo", Kind: kind, Meta: map[string]any{"receiver": receiver}}
	}
	goCaller := node("caller-go", "caller", goFile, "go", "Owner", method)
	goCaller.Meta["scope_pkg"] = "project/pkg"
	ownerField := node("z-owner-stamped", "Owned", "other/partial.go", "go", "Owner", field)
	ownerField.Meta["scope_pkg"] = "project/pkg"
	foreign := node("a-foreign", "Slot", goFile, "go", "T", field)
	foreign.RepoPrefix = "other"
	cStatic := node("z-local-static", "Scoped", cFile, "c", "", fn)
	cStatic.Meta[MetaScopeStatic] = true
	csWrong := node("a-wrong-arity", "Run", csFile, "csharp", "T", method)
	csWrong.Meta["param_count"], csWrong.Meta["method_type_params"] = 1, "T"
	csWrongType := node("b-wrong-type-arity", "Run", csFile, "csharp", "T", method)
	csWrongType.Meta["param_count"], csWrongType.Meta["method_type_params"] = 2, "T,U"
	csRight := node("z-right-arity", "Run", csFile, "csharp", "T", method)
	csRight.Meta["param_count"], csRight.Meta["method_type_params"] = 2, "U"
	nodes := []*graph.Node{
		goCaller, node("caller-c", "caller", cFile, "c", "", fn),
		node("caller-cs", "caller", csFile, "csharp", "", fn),
		node("caller-ts", "caller", tsFile, "typescript", "", fn),
		foreign, node("a-other-language", "Slot", goFile, "python", "T", field),
		node("a-far-slot", "Slot", "other/slot.go", "go", "T", field),
		node("a-local-slot-method", "Slot", goFile, "go", "T", method),
		node("y-local-slot", "Slot", "src/data.go", "go", "T", field),
		node("z-local-slot", "Slot", goFile, "go", "T", field),
		node("a-last-resort", "Far", goFile, "go", "Other", field),
		node("z-far-field", "Far", "other/data.go", "go", "T", field),
		node("a-wrong-package", "Owned", goFile, "go", "Owner", field), ownerField,
		node("a-local-any", "Local", goFile, "go", "Other", field),
		node("a-unrelated-field", "Handle", "other/data.go", "go", "Other", field),
		node("a-unrelated-method", "Handle", goFile, "go", "Other", method),
		node("z-exact-method", "Handle", "other/data.go", "go", "T", method),
		node("a-remote-exact", "Choose", "other/data.go", "go", "T", method),
		node("b-local-exact", "Choose", "src/data.go", "go", "T", method),
		node("z-local-exact", "Choose", goFile, "go", "T", method),
		node("a-ambiguous", "Ambiguous", goFile, "go", "First", method),
		node("z-ambiguous", "Ambiguous", goFile, "go", "Second", method),
		node("a-c-method", "Scoped", cFile, "c", "T", method), cStatic,
		csWrong, csWrongType, csRight,
	}
	cases := []struct {
		from, file, name, receiver string
		kind                       graph.EdgeKind
		meta                       map[string]any
		to                         string
		wantKind                   graph.EdgeKind
		confidence                 float64
		origin                     string
	}{
		{"caller-go", goFile, "Slot", "T", reads, nil, "y-local-slot", reads, 0.95, graph.OriginASTInferred},
		{"caller-go", goFile, "Slot", "T", writes, nil, "y-local-slot", writes, 0.95, graph.OriginASTInferred},
		{"caller-go", goFile, "Far", "T", writes, nil, "z-far-field", writes, 0.85, graph.OriginASTInferred},
		{"caller-go", goFile, "Owned", "", reads, nil, "z-owner-stamped", reads, 0.85, graph.OriginASTInferred},
		{"caller-go", goFile, "Local", "T", reads, nil, "a-local-any", reads, 0.6, graph.OriginASTInferred},
		{"caller-go", goFile, "Handle", "T", reads, nil, "z-exact-method", refs, 0.85, graph.OriginASTResolved},
		{"caller-go", goFile, "Handle", "T", writes, nil, "z-exact-method", writes, 0.85, graph.OriginASTResolved},
		{"caller-go", goFile, "Choose", "T", calls, nil, "b-local-exact", calls, 0.95, graph.OriginASTResolved},
		{"caller-go", goFile, "Ambiguous", "", reads, nil, "unresolved::*.Ambiguous", reads, 0.42, graph.OriginASTInferred},
		{"caller-go", goFile, "Missing", "T", writes, nil, "unresolved::*.Missing", writes, 0.42, graph.OriginASTInferred},
		{"caller-go", goFile, "", "T", reads, nil, "unresolved::*.", reads, 0.42, graph.OriginASTInferred},
		{"caller-c", cFile, "Scoped", "T", calls, nil, "z-local-static", calls, 0.92, graph.OriginASTResolved},
		{"caller-cs", csFile, "Run", "T", calls, map[string]any{"arg_count": 2, "type_arg_count": 1}, "z-right-arity", calls, 0.95, graph.OriginASTResolved},
		{"caller-ts", tsFile, "push", "", reads, nil, "repo::builtin::ts::array::push", refs, 0.42, graph.OriginASTInferred},
	}
	edges := make([]*graph.Edge, 0, len(cases))
	for i, tc := range cases {
		e := &graph.Edge{From: tc.from, FilePath: tc.file, Line: i + 1, To: "unresolved::*." + tc.name,
			Kind: tc.kind, Confidence: 0.42, Origin: graph.OriginASTInferred, CrossRepo: true,
			Meta: map[string]any{"receiver_type": tc.receiver, "payload": map[string]any{"kept": true}}}
		maps.Copy(e.Meta, tc.meta)
		setEdgeTerminal(e, terminalReasonNoDefinition)
		edges = append(edges, e)
	}

	// The same complete SQLite rows must survive either native insertion
	// order and either worker order. Opposite fixture order stresses the
	// canonical tie order instead of just checking that a winner exists.
	for i, order := range []string{"forward", "reverse"} {
		t.Run(order, func(t *testing.T) {
			ctx, build, opts := frontierFixture(t, i+1, 0)
			opts.SpillBytes = 1
			ordered := slices.Clone(nodes)
			if order == "reverse" {
				slices.Reverse(ordered)
			}
			build.AddBatch(ordered, edges)
			slices.Reverse(ordered)
			want := rebuildMemberNativeSaved(t, ordered, edges, len(cases)-3)
			require.NoError(t, runRebuildMain(ctx, build, opts, false, rebuildMainImportInputs{}))
			got := rebuildFnPointerSaved(t, build)
			require.Equal(t, want, got)
			require.Len(t, got, len(cases))
			for _, e := range got {
				tc := cases[e.Line-1]
				require.Equal(t, tc.to, e.To, "line %d", e.Line)
				require.Equal(t, tc.wantKind, e.Kind, "line %d", e.Line)
				require.Equal(t, tc.confidence, e.Confidence, "line %d", e.Line)
				require.Equal(t, tc.origin, e.Origin, "line %d", e.Line)
				require.Equal(t, graph.IsUnresolvedTarget(e.To), edgeTerminalFlag(e), "line %d", e.Line)
			}
			checkFrontierScratch(t, opts.ScratchParent)
		})
	}
}

func rebuildMemberNativeSaved(t *testing.T, nodes []*graph.Node, edges []*graph.Edge, moveCount int) []*graph.Edge {
	t.Helper()
	sql, err := store_sqlite.Open(filepath.Join(t.TempDir(), "native.sqlite"))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, sql.Close()) })
	require.NoError(t, sql.AddBatchChecked(nodes, edges))
	native := New(sql)
	native.bulkMode = true
	pending := sql.AllEdges()
	rebuildFnPointerSortRows(pending)
	require.NoError(t, native.warmLookupCache(pending))
	defer native.clearLookupCache()
	require.True(t, native.buildReachabilityIndexForPending(pending, native.nodeByID))
	defer native.clearReachabilityIndex()
	var moves []graph.EdgeReindex
	for _, original := range pending {
		e := *original
		e.Meta = maps.Clone(e.Meta)
		if oldTo, changed := native.resolveEdge(&e, &ResolveStats{}); changed {
			clearEdgeTerminal(&e)
			moves = append(moves, graph.EdgeReindex{Edge: &e, OldTo: oldTo, OldKind: original.Kind})
		}
	}
	require.Len(t, moves, moveCount)
	require.True(t, sql.ReindexEdgesApplied(moves))
	saved := sql.AllEdges()
	rebuildFnPointerSortRows(saved)
	return saved
}

// Mirror the page owner's tentative copies and terminal handling without
// changing or depending on the shared dispatcher.
func rebuildMemberTestMoves(ctx context.Context, f *rebuildLinkFrontier, page []*graph.Edge) ([]graph.EdgeReindex, error) {
	var moves []graph.EdgeReindex
	for _, old := range page {
		e := *old
		name := strings.TrimPrefix(graph.UnresolvedName(e.To), "*.")
		if err := resolveRebuildMember(ctx, f, &e, name); err != nil {
			return nil, err
		}
		if e.To != old.To {
			e.Meta = maps.Clone(e.Meta)
			clearEdgeTerminal(&e)
			moves = append(moves, graph.EdgeReindex{Edge: &e, OldTo: old.To, OldKind: old.Kind})
		}
	}
	return moves, nil
}

func TestRebuildMemberRefusesMissingEvidence(t *testing.T) {
	for _, tc := range []struct {
		reason, lang, caller, file string
		meta                       map[string]any
	}{
		{"direct-import reachability", "go", "src/caller.go", "far/def.go", nil},
		{"first-use useClass", "go", "src/caller.go", "src/def.go", nil},
		{"C# extension visibility", "csharp", "src/caller.cs", "src/def.cs", map[string]any{"this_param_type": "I", "extension": true}},
	} {
		t.Run(tc.reason, func(t *testing.T) {
			ctx, build, opts := frontierFixture(t, 1, 0)
			opts.SpillBytes = 1
			badMeta := map[string]any{"receiver_type": "NotOwner", "payload": []any{"unchanged"}}
			if tc.lang == "csharp" {
				delete(badMeta, "receiver_type")
			}
			build.AddBatch([]*graph.Node{
				{ID: "a-go", Name: "caller", RepoPrefix: "repo", Language: "go", FilePath: "first.go", Kind: graph.KindFunction},
				{ID: "ok-method", Name: "Safe", RepoPrefix: "repo", Language: "go", FilePath: "first.go", Kind: graph.KindMethod,
					Meta: map[string]any{"receiver": "T"}},
				{ID: "z-other", Name: "caller", RepoPrefix: "repo", Language: tc.lang, FilePath: tc.caller, Kind: graph.KindFunction},
				{ID: "bad-method", Name: "NeedsEvidence", RepoPrefix: "repo", Language: tc.lang, FilePath: tc.file,
					Kind: graph.KindMethod, Meta: tc.meta},
				{ID: "local-distractor", Name: "NeedsEvidence", RepoPrefix: "repo", Language: tc.lang, FilePath: tc.caller, Kind: graph.KindField},
			}, []*graph.Edge{
				{From: "a-go", To: "unresolved::*.Safe", Kind: graph.EdgeReads, FilePath: "first.go", Meta: map[string]any{"receiver_type": "T"}},
				{From: "z-other", To: "unresolved::*.NeedsEvidence", Kind: graph.EdgeCalls, FilePath: tc.caller, Meta: badMeta},
			})
			want := rebuildFnPointerSaved(t, build)
			require.Len(t, want, 2)
			err := runRebuildLinkFrontier(ctx, build, opts, func(f *rebuildLinkFrontier, page []*graph.Edge) error {
				require.Len(t, page, 2)
				moves, err := rebuildMemberTestMoves(ctx, f, page[:1])
				require.NoError(t, err)
				require.Len(t, moves, 1)
				require.Equal(t, graph.EdgeReferences, moves[0].Edge.Kind)
				require.Equal(t, "ok-method", moves[0].Edge.To)
				before, e := *page[1], *page[1]
				before.Meta = maps.Clone(before.Meta)
				err = resolveRebuildMember(ctx, f, &e, "NeedsEvidence")
				require.Equal(t, before, e)
				require.Equal(t, before, *page[1])
				return err
			})
			require.ErrorIs(t, err, errRebuildMainRule)
			require.ErrorContains(t, err, tc.reason)
			require.Equal(t, want, rebuildFnPointerSaved(t, build))
			checkFrontierScratch(t, opts.ScratchParent)
		})
	}
}
