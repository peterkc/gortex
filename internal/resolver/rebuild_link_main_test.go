package resolver

import (
	"path/filepath"
	"slices"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/graph/store_sqlite"
)

func TestRebuildMainTokenAndSource(t *testing.T) {
	t.Setenv("GORTEX_RESOLVE_FROM_RECONCILE", "1")
	ctx, build, opts := frontierFixture(t, 2, 0)
	node := func(id, name, repo, lang, file string) *graph.Node {
		return &graph.Node{ID: id, Name: name, QualName: id, RepoPrefix: repo, Language: lang, FilePath: file, Kind: graph.KindVariable}
	}
	nodes := []*graph.Node{
		node("repo/caller", "caller", "repo", "typescript", "src/caller.ts"),
		node("repo/a", "TOKEN", "repo", "typescript", "other/token.ts"),
		node("repo/b", "TOKEN", "repo", "typescript", "src/token.ts"),
		node("repo/c", "TOKEN", "repo", "go", "src/token.go"),
		node("other/d", "TOKEN", "other", "typescript", "src/token.ts"),
		node("root", "ROOT", "", "javascript", "far/token.js"),
		node("repo/root", "ROOT", "repo", "javascript", "far/token.js"),
		{ID: "repo/t", Name: "Typed", RepoPrefix: "repo", Language: "typescript", Kind: graph.KindType, FilePath: "src/type.ts"},
	}
	token := &graph.Edge{From: "repo/caller", To: "unresolved::TOKEN", Kind: graph.EdgeProvides, FilePath: "src/caller.ts", Line: 8, Meta: map[string]any{"payload": "keep"}}
	setEdgeTerminal(token, terminalReasonNoDefinition)
	qualified := &graph.Edge{From: "repo/unresolved::TOKEN", To: "repo/caller", Kind: graph.EdgeArgOf, FilePath: token.FilePath, Line: token.Line}
	bare := *qualified
	bare.From = token.To
	missing := &graph.Edge{From: "repo/missing", To: "unresolved::ROOT", Kind: graph.EdgeConsumes, FilePath: "src/caller.ts"}
	deferred := &graph.Edge{From: token.From, To: "unresolved::pyrel::pkg.file", Kind: graph.EdgeImports}
	exempt := &graph.Edge{From: token.From, To: token.To, Kind: graph.EdgeTests}
	typeEdge := &graph.Edge{From: token.From, To: "unresolved::*.Typed", Kind: graph.EdgeTypedAs, FilePath: token.FilePath, Line: 11}
	edges := []*graph.Edge{token, &bare, qualified, missing, deferred, exempt, typeEdge}
	build.AddBatch(nodes, edges)
	sql, err := store_sqlite.Open(filepath.Join(t.TempDir(), "native.sqlite"))
	require.NoError(t, err)
	defer func() { require.NoError(t, sql.Close()) }()
	require.NoError(t, sql.AddBatchChecked(nodes, edges))
	native := New(sql)
	native.bulkMode = true
	pending := []*graph.Edge{token, missing, deferred, exempt, typeEdge}
	slices.SortFunc(pending, compareRebuildImportEdges)
	require.NoError(t, native.warmLookupCache(pending))
	defer native.clearLookupCache()
	var moves []graph.EdgeReindex
	for _, edge := range pending {
		clone := cloneEdgeForResolve(edge)
		if oldTo, changed := native.resolveEdge(clone, &ResolveStats{}); changed {
			clearEdgeTerminal(clone)
			moved := *clone
			moves = append(moves, graph.EdgeReindex{Edge: &moved, OldTo: oldTo, OldKind: edge.Kind})
		}
		releaseResolverClone(clone)
	}
	require.Len(t, moves, 3)
	require.True(t, sql.ReindexEdgesApplied(moves))
	native.placeholderSrcIdx.deferRepoints(sql, moves)
	require.Equal(t, 2, native.placeholderSrcIdx.applyDeferred(sql))
	want := sql.AllEdges()
	slices.SortFunc(want, compareRebuildImportEdges)

	require.NoError(t, runRebuildMain(ctx, build, opts, true, rebuildMainImportInputs{}))
	got := rebuildFnPointerSaved(t, build)
	require.Equal(t, want, got)
	for _, e := range got {
		switch e.Kind {
		case graph.EdgeProvides:
			require.Equal(t, "repo/b", e.To)
			require.Equal(t, 0.9, e.Confidence)
			require.False(t, edgeTerminalFlag(e))
		case graph.EdgeConsumes:
			require.Equal(t, "root", e.To, "missing caller reads exact empty repo")
			require.Equal(t, 0.7, e.Confidence)
		case graph.EdgeArgOf:
			require.Equal(t, "repo/b", e.From)
		case graph.EdgeTypedAs:
			require.Equal(t, "repo/t", e.To, "type rules precede wildcard member rules")
		}
	}
	checkFrontierScratch(t, opts.ScratchParent)
}

// Given a mixed main page with settled import evidence, bare reads promote
// their kind and Vue sites bind through imports. A Rust-qualified call stays
// untouched for its scope pass, even when the bare name would match.
func TestRebuildMainBareAndVueDispatch(t *testing.T) {
	ctx, build, opts := frontierFixture(t, 2, 0)
	const goFile, vueFile, jsxFile = "src/caller.go", "ui/view.vue", "ui/view.tsx"
	const goID, panelFile, panelID, jsxID = "src/caller.go::Run", "ui/Panel.vue", "ui/Panel.vue::Panel", "ui/Child.tsx::Child"
	nodes := []*graph.Node{
		typeTestNode("go", "Source", graph.KindFunction, "go", goFile),
		typeTestNode(goID, "Run", graph.KindFunction, "go", goFile),
		typeTestNode(panelFile, "Panel.vue", graph.KindFile, "vue", panelFile),
		typeTestNode(panelID, "Panel", graph.KindFunction, "vue", panelFile),
		typeTestNode(jsxID, "Child", graph.KindFunction, "typescript", "ui/Child.tsx"),
	}
	read := &graph.Edge{From: "go", To: "unresolved::Run", Kind: graph.EdgeReads, FilePath: goFile, Line: 1}
	promoted := *read
	promoted.To, promoted.Kind = goID, graph.EdgeReferences
	rust := &graph.Edge{From: "go", To: "unresolved::Run", Kind: graph.EdgeCalls, FilePath: goFile, Line: 2,
		Meta: map[string]any{"rust_path": "Type::Run"}}
	imp := rebuildVueTestImport(vueFile, panelFile, "Panel", 1)
	ref := rebuildVueTestRef(vueFile, "Panel", "Panel", 2)
	render := rebuildVueTestRender(jsxFile, "Child", 3)
	build.AddBatch(nodes, []*graph.Edge{read, rust, imp, ref, render})
	require.NoError(t, runRebuildMain(ctx, build, opts, false, rebuildMainImportInputs{}))
	want := rebuildVueSortedRows([]*graph.Edge{&promoted, rust, imp, rebuildVueTestSaved(ref, panelID, true), rebuildVueTestSaved(render, jsxID, false)})
	require.Equal(t, want, rebuildFnPointerSaved(t, build))
	checkFrontierScratch(t, opts.ScratchParent)
}

func TestRebuildMainRefusesIncompletePage(t *testing.T) {
	ctx, build, opts := frontierFixture(t, 1, 0)
	token := &graph.Edge{From: "a", Kind: graph.EdgeProvides, To: "unresolved::Run"}
	unsupported := &graph.Edge{From: "z", Kind: graph.EdgeCalls, To: "unresolved::*.Run", FilePath: "z.go",
		Meta: map[string]any{"receiver_type": "MissingOwner"}}
	build.AddNode(&graph.Node{ID: "empty-repo", Name: "Run", QualName: "empty-repo", Kind: graph.KindMethod})
	build.AddBatch(nil, []*graph.Edge{unsupported, token})
	want := rebuildFnPointerSaved(t, build)
	require.ErrorIs(t, runRebuildMain(ctx, build, opts, true, rebuildMainImportInputs{}), errRebuildMainRule)
	require.Equal(t, want, rebuildFnPointerSaved(t, build))
	checkFrontierScratch(t, opts.ScratchParent)
}
