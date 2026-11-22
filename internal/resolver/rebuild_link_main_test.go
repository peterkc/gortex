package resolver

import (
	"errors"
	"maps"
	"path/filepath"
	"slices"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/graph/store_pebble"
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

	require.NoError(t, RunRebuildMain(ctx, build, opts, RebuildMainInputs{SelectCallers: func(page []*graph.Edge) ([]*graph.Edge, error) { return page, nil }}))
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
		typeTestNode(jsxFile+"::Source", "Source", graph.KindFunction, "typescript", jsxFile),
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
	require.NoError(t, runRebuildMain(ctx, build, opts, false, RebuildMainInputs{}))
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
	require.ErrorIs(t, runRebuildMain(ctx, build, opts, true, RebuildMainInputs{}), errRebuildMainRule)
	require.Equal(t, want, rebuildFnPointerSaved(t, build))
	checkFrontierScratch(t, opts.ScratchParent)
}

func TestRunRebuildMainSelectsCallers(t *testing.T) {
	ctx, build, opts := frontierFixture(t, 2, 0)
	const caller, deferredCaller, candidate = "repo/src/main.ts", "repo/legacy/caller.py", "repo/legacy/provider.py"
	selected := rebuildImportTestEdge(caller, caller, "package::Item", 7)
	deferred := rebuildImportTestEdge(deferredCaller, deferredCaller, "rewritten::Item", 7)
	build.AddBatch([]*graph.Node{
		rebuildImportTestNode(caller, "repo", "repo", "", "typescript", graph.KindFile),
		rebuildImportTestNode(deferredCaller, "repo", "repo", "", "python", graph.KindFile),
		rebuildImportTestNode(candidate, "repo", "repo", "rewritten::Item", "python", graph.KindPackage),
	}, []*graph.Edge{selected, deferred})

	require.NoError(t, RunRebuildMain(ctx, build, opts, RebuildMainInputs{
		SelectCallers: func(page []*graph.Edge) ([]*graph.Edge, error) {
			require.Equal(t, []*graph.Edge{deferred, selected}, page)
			return page[1:], nil
		},
		PathAlias: func(file, _ string) (string, error) {
			require.Equal(t, caller, file)
			return "", nil
		},
		RewriteNpm: func(file, spec string) (string, error) {
			require.Equal(t, caller, file)
			require.Equal(t, "package::Item", spec)
			return "rewritten::Item", nil
		},
	}))
	// The link still finds a deferred-language node in the frozen candidate
	// partitions. The selected old key disappears; the deferred row is exact.
	want := []*graph.Edge{deferred, rebuildImportTestMove(selected, candidate, false)}
	require.Equal(t, want, rebuildFnPointerSaved(t, build))
	checkFrontierScratch(t, opts.ScratchParent)
}

func TestRunRebuildMainSkipsCandidateStartup(t *testing.T) {
	ctx, build, opts := frontierFixture(t, 1, resolvePendingPageRows+1)
	build.AddNode(&graph.Node{ID: "caller", Language: "python", FilePath: "caller.py"})
	opts.Binary = filepath.Join(opts.ScratchParent, "must-not-start")
	want := rebuildFnPointerSaved(t, build)
	require.Len(t, want, resolvePendingPageRows+1)
	require.ErrorContains(t, RunRebuildMain(ctx, build, opts, RebuildMainInputs{}), "missing caller selector")

	seen := 0
	require.NoError(t, RunRebuildMain(ctx, build, opts, RebuildMainInputs{
		SelectCallers: func(page []*graph.Edge) ([]*graph.Edge, error) {
			end := min(seen+resolvePendingPageRows, len(want))
			require.Equal(t, want[seen:end], page)
			seen = end
			return nil, nil
		},
	}))
	require.Equal(t, len(want), seen)
	require.Equal(t, want, rebuildFnPointerSaved(t, build))
	checkFrontierScratch(t, opts.ScratchParent)

	rejected := errors.New("receipt selection refused")
	calls := 0
	err := RunRebuildMain(ctx, build, opts, RebuildMainInputs{
		SelectCallers: func(page []*graph.Edge) ([]*graph.Edge, error) {
			calls++
			require.Equal(t, want[:resolvePendingPageRows], page)
			return page, rejected
		},
	})
	require.ErrorIs(t, err, rejected)
	require.NotErrorIs(t, err, store_pebble.ErrRebuildLink)
	require.Equal(t, 1, calls)
	require.Equal(t, want, rebuildFnPointerSaved(t, build))
	checkFrontierScratch(t, opts.ScratchParent)
}

func TestRebuildMainFrozenPlaceholderPolicy(t *testing.T) {
	t.Setenv("GORTEX_RESOLVE_FROM_RECONCILE", "1")
	ctx, build, opts := frontierFixture(t, 1, 0)
	const source, file = "repo/caller", "repo/caller.ts"
	const target, pending = "repo/Token", "unresolved::TOKEN"
	token := &graph.Edge{From: source, To: pending, Kind: graph.EdgeProvides, FilePath: file, Line: 3}
	flow := &graph.Edge{From: pending, To: source, Kind: graph.EdgeArgOf, FilePath: file, Line: 3}
	build.AddBatch([]*graph.Node{
		{ID: source, Name: "caller", Kind: graph.KindVariable, Language: "typescript", RepoPrefix: "repo", FilePath: file},
		{ID: target, Name: "TOKEN", Kind: graph.KindVariable, Language: "typescript", RepoPrefix: "repo", FilePath: "repo/value.ts"},
	}, []*graph.Edge{token, flow})
	require.NoError(t, RunRebuildMain(ctx, build, opts, RebuildMainInputs{
		SelectCallers:          func(page []*graph.Edge) ([]*graph.Edge, error) { return page, nil },
		SkipPlaceholderSources: true,
	}))
	want := *token
	want.To, want.Confidence = target, 0.9
	require.Equal(t, []*graph.Edge{&want, flow}, rebuildFnPointerSaved(t, build))
	checkFrontierScratch(t, opts.ScratchParent)
}

// The guard must use the import resolved by main and undo the unreachable
// call and its source repoint, without changing the reachable call site.
func TestRebuildMainCrossPackageGuard(t *testing.T) {
	t.Setenv("GORTEX_RESOLVE_FROM_RECONCILE", "1")
	ctx, build, opts := frontierFixture(t, 1, 0)
	opts.SpillBytes = 256 // smaller than one guard record
	const file, caller = "app/main.ts", "app/main.ts::Caller"
	const barrel = "entry/index.ts"
	const keep, drop = "lib/impl.ts::owner.Keep", "other/impl.ts::owner.Drop"
	nodes := []*graph.Node{
		typeTestNode(file, "main.ts", graph.KindFile, "typescript", file),
		typeTestNode(caller, "Caller", graph.KindFunction, "typescript", file),
		typeTestNode(barrel, "index.ts", graph.KindFile, "typescript", barrel),
		// Not file::Name export keys: both calls reach the weak name tier.
		typeTestNode(keep, "Keep", graph.KindFunction, "typescript", "lib/impl.ts"),
		typeTestNode(drop, "Drop", graph.KindFunction, "typescript", "other/impl.ts"),
	}
	imp := rebuildImportTestEdge(file, file, "../entry/index", 1)
	reexport := &graph.Edge{From: barrel, To: keep, Kind: graph.EdgeReExports, FilePath: barrel, Line: 1}
	makeCall := func(name string, line int) *graph.Edge {
		return &graph.Edge{From: caller, To: "unresolved::" + name, Kind: graph.EdgeCalls, FilePath: file, Line: line,
			Origin: graph.OriginASTInferred, Confidence: 0.6, ConfidenceLabel: "INFERRED", Meta: map[string]any{"payload": name}}
	}
	keptCall, droppedCall := makeCall("Keep", 2), makeCall("Drop", 3)
	makeFlow := func(e *graph.Edge) *graph.Edge {
		return &graph.Edge{From: e.To, To: caller, Kind: graph.EdgeArgOf, FilePath: file, Line: e.Line}
	}
	keptFlow, droppedFlow := makeFlow(keptCall), makeFlow(droppedCall)
	edges := []*graph.Edge{imp, reexport, keptCall, droppedCall, keptFlow, droppedFlow}
	build.AddBatch(nodes, edges)

	g := graph.New()
	g.AddBatch(nodes, nil)
	for _, e := range edges {
		copy := *e
		copy.Meta = maps.Clone(e.Meta)
		g.AddEdge(&copy)
	}
	r := New(g)
	r.bulkMode = true
	r.buildDirIndexes()
	pending := []*graph.Edge{imp, keptCall, droppedCall}
	slices.SortFunc(pending, compareRebuildImportEdges)
	require.NoError(t, r.warmLookupCache(pending))
	var moves []graph.EdgeReindex
	var jobs []reindexJob
	for _, old := range pending {
		e := *old
		e.Meta = maps.Clone(old.Meta)
		oldTo, changed := r.resolveEdge(&e, &ResolveStats{})
		require.True(t, changed, old.To)
		clearEdgeTerminal(&e)
		moves = append(moves, graph.EdgeReindex{Edge: &e, OldTo: oldTo, OldKind: old.Kind})
		jobs = append(jobs, reindexJob{edge: &e, newTo: e.To, oldTo: oldTo, kind: e.Kind, origin: e.Origin, confidence: e.Confidence})
	}
	r.clearLookupCache()
	g.ReindexEdges(moves)
	r.placeholderSrcIdx.deferRepoints(g, moves)
	require.Equal(t, 2, r.placeholderSrcIdx.applyDeferred(g))
	spool, err := newResolveGuardSpool()
	require.NoError(t, err)
	defer spool.close()
	require.NoError(t, spool.appendJobs([][]reindexJob{jobs}))
	records, done, err := spool.nextPage(resolvePendingPageRows)
	require.NoError(t, err)
	require.True(t, done)
	require.Len(t, records, 2)
	guardJobs := guardJobsFromRecords(g, records)
	require.Len(t, guardJobs, 2)
	closure := r.buildImportClosure()
	r.warmGuardLookupCache(guardJobs)
	require.Equal(t, 1, r.guardCrossPackageCallEdges(guardJobs, closure))
	r.clearLookupCache()
	want := rebuildVueSortedRows(g.AllEdges())

	require.NoError(t, RunRebuildMain(ctx, build, opts, RebuildMainInputs{
		SelectCallers: func(page []*graph.Edge) ([]*graph.Edge, error) { return page, nil },
	}))
	got := rebuildFnPointerSaved(t, build)
	require.Equal(t, want, got)
	require.Len(t, got, len(edges))
	require.Equal(t, barrel, rebuildFnPointerSavedAt(t, got, graph.EdgeImports, file, 1).To)
	require.Equal(t, keep, rebuildFnPointerSavedAt(t, got, graph.EdgeCalls, caller, 2).To)
	reverted := rebuildFnPointerSavedAt(t, got, graph.EdgeCalls, caller, 3)
	require.Equal(t, droppedCall.To, reverted.To)
	require.Equal(t, map[string]any{"payload": "Drop", "guard_reverted": true}, reverted.Meta)
	require.Equal(t, caller, rebuildFnPointerSavedAt(t, got, graph.EdgeArgOf, keep, 2).To)
	require.Equal(t, droppedFlow, rebuildFnPointerSavedAt(t, got, graph.EdgeArgOf, droppedFlow.From, 3))
	checkFrontierScratch(t, opts.ScratchParent)
}
