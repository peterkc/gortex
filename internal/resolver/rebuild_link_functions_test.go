package resolver

import (
	"maps"
	"path/filepath"
	"slices"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/graph/store_sqlite"
)

func TestRebuildFunctionNativeRows(t *testing.T) {
	ctx, build, opts := frontierFixture(t, 2, 0)
	opts.SpillBytes = 1 // exercise the sorted stream, not insertion order
	const goFile, cFile, tsFile, csFile = "src/caller.go", "src/caller.c", "src/caller.ts", "src/caller.cs"
	const cppFile, javaFile, phpFile = "src/caller.cpp", "src/Caller.java", "src/caller.php"
	const calls, reads, refs = graph.EdgeCalls, graph.EdgeReads, graph.EdgeReferences
	const resolved, inferred, text = graph.OriginASTResolved, graph.OriginASTInferred, graph.OriginTextMatched
	node := func(id, name, repo, lang, file string, kind graph.NodeKind, meta map[string]any) *graph.Node {
		return &graph.Node{ID: id, Name: name, QualName: id, RepoPrefix: repo, Language: lang, FilePath: file, Kind: kind, Meta: meta}
	}
	const fn, method, variable = graph.KindFunction, graph.KindMethod, graph.KindVariable
	nodes := []*graph.Node{
		node("go", "caller", "repo", "go", goFile, fn, nil),
		node("c", "caller", "repo", "c", cFile, fn, nil),
		node("ts", "caller", "repo", "typescript", tsFile, fn, nil),
		node("cs", "caller", "repo", "csharp", csFile, fn, nil),
		node("cpp", "caller", "repo", "cpp", cppFile, fn, nil),
		node("java", "caller", "repo", "java", javaFile, method, map[string]any{MetaScopeClass: "Caller"}),
		node("php", "caller", "repo", "php", phpFile, fn, map[string]any{MetaScopeNamespace: "Local"}),
		node("a-remote", "File", "other", "go", goFile, fn, nil),
		node("a-other-language", "File", "repo", "javascript", goFile, fn, nil),
		node("a-directory", "File", "repo", "go", "src/file.go", fn, nil),
		node("z-file", "File", "repo", "go", goFile, fn, nil),
		node("a-ordinary", "Ambiguous", "repo", "go", goFile, fn, nil),
		node("z-neutral", "Ambiguous", "repo", "", goFile, method, nil),
		node("a-neighbor", "Neighbor", "repo", "go", "src/neighbor.go", method, nil),
		node("z-remote", "Neighbor", "repo", "go", "other/neighbor.go", fn, nil),
		node("repo::builtin::go::type::Far", "Far", "repo", "go", goFile, fn, nil),
		node("external-call::go:Far", "Far", "repo", "go", goFile, fn, nil),
		node("a-barrel", "Far", "repo", "go", goFile, fn, map[string]any{"reexport": true}),
		node("z-far", "Far", "repo", "go", "other/far.go", method, nil),
		node("a-generic-miss", "Generic", "repo", "go", goFile, fn, nil),
		node("z-generic", "Generic", "repo", "go", "other/generic.go", fn, map[string]any{"type_params": "T"}),
		node("a-present-repo", "EmptyRepo", "repo", "rust", "src/other.rs", fn, nil),
		node("z-empty-repo", "EmptyRepo", "", "python", "far/foo.py", fn, nil),
		node("a-c-external", "Callee", "repo", "c", cFile, fn, nil),
		node("z-c-static", "Callee", "repo", "c", cFile, fn, map[string]any{MetaScopeStatic: true}),
		node("a-cpp-local", "RunCpp", "repo", "cpp", cppFile, fn, nil),
		node("z-cpp-adl", "RunCpp", "repo", "cpp", "other/use.cpp", fn, map[string]any{MetaScopeNamespace: "N"}),
		node("a-java-local", "RunJava", "repo", "java", javaFile, method, map[string]any{MetaScopeClass: "Nested"}),
		node("z-java-enclosing", "RunJava", "repo", "java", javaFile, method, map[string]any{MetaScopeClass: "Caller"}),
		node("a-php-local", "RunPhp", "repo", "php", phpFile, fn, nil),
		node("z-php-namespace", "RunPhp", "repo", "php", "other/also.php", fn, map[string]any{MetaScopeNamespace: "Local"}),
		node("src/caller.ts::Value", "Value", "repo", "typescript", tsFile, variable, nil),
		node("a-ts-nested", "Value", "repo", "typescript", "far/value.ts", variable, nil),
		node("a-cs-wrong-arity", "Overload", "repo", "csharp", csFile, method, map[string]any{"method_type_params": "T", "param_count": 1}),
		node("b-cs-wrong-type", "Overload", "repo", "csharp", csFile, method, map[string]any{"method_type_params": "T,U", "param_count": 2}),
		node("z-cs-overload", "Overload", "repo", "csharp", csFile, method, map[string]any{"method_type_params": "U", "param_count": 2}),
	}
	cases := []struct {
		from, file, name string
		kind             graph.EdgeKind
		origin           string
		meta             map[string]any
		to               string
		wantKind         graph.EdgeKind
		wantOrigin       string
		confidence       float64
	}{
		{"go", goFile, "File", calls, "", nil, "z-file", calls, resolved, 0.9},
		{"go", goFile, "File", calls, inferred, nil, "z-file", calls, inferred, 0.42},
		{"go", goFile, "Ambiguous", calls, "", nil, "z-neutral", calls, "", 0.42},
		{"go", goFile, "Neighbor", calls, "", nil, "a-neighbor", calls, resolved, 0.9},
		{"go", goFile, "Far", reads, "", map[string]any{"resolution": "extension_method"}, "z-far", refs, text, 0.42},
		{"go", goFile, "File", reads, "", nil, "z-file", refs, "", 0.42},
		{"go", goFile, "Generic", calls, "", map[string]any{graph.MetaGenericInstantiation: true}, "z-generic", calls, text, 0.42},
		{"go", goFile, "EmptyRepo", calls, "", nil, "unresolved::EmptyRepo", calls, "", 0.42},
		{"repo::missing", goFile, "EmptyRepo", calls, "", nil, "z-empty-repo", calls, text, 0.42},
		{"c", cFile, "Callee", calls, "", nil, "z-c-static", calls, resolved, 0.92},
		{"cpp", cppFile, "RunCpp", calls, "", map[string]any{MetaScopeArgTypes: "N::Arg"}, "z-cpp-adl", calls, resolved, 0.92},
		{"java", javaFile, "RunJava", calls, "", nil, "z-java-enclosing", calls, resolved, 0.92},
		{"php", phpFile, "RunPhp", calls, "", nil, "z-php-namespace", calls, resolved, 0.92},
		{"ts", tsFile, "Value", reads, "", nil, "src/caller.ts::Value", reads, inferred, 0.7},
		{"cs", csFile, "Overload", calls, "", map[string]any{"type_arg_count": 1, "arg_count": 2, "resolution": "using_static"}, "z-cs-overload", calls, resolved, 0.9},
		{"cs", csFile, "Overload", calls, "", map[string]any{"type_arg_count": 3, "arg_count": 4}, "a-cs-wrong-arity", calls, "", 0.42},
		{"cs", csFile, "Overload", calls, "", map[string]any{"local_shadow": true, "resolution": "using_static"}, "unresolved::Overload", calls, "", 0.42},
	}
	edges := make([]*graph.Edge, 0, len(cases))
	for i, tc := range cases {
		e := &graph.Edge{From: tc.from, To: "unresolved::" + tc.name, Kind: tc.kind, FilePath: tc.file, Line: i + 1,
			Origin: tc.origin, Confidence: 0.42, CrossRepo: true,
			Meta: map[string]any{"payload": map[string]any{"kept": true}}}
		maps.Copy(e.Meta, tc.meta)
		setEdgeTerminal(e, terminalReasonNoDefinition)
		edges = append(edges, e)
	}
	// Native oracle: same facts, but opposite input order and pinned native
	// resolveEdge before the guard and tail. Persist native's actual moves.
	build.AddBatch(nodes, edges)
	slices.Reverse(nodes)
	sql, err := store_sqlite.Open(filepath.Join(t.TempDir(), "native.sqlite"))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, sql.Close()) })
	require.NoError(t, sql.AddBatchChecked(nodes, edges))
	native := New(sql)
	native.bulkMode = true
	pending := sql.AllEdges()
	slices.SortFunc(pending, compareRebuildImportEdges)
	require.NoError(t, native.warmLookupCache(pending))
	defer native.clearLookupCache()
	oracle := make(map[int]graph.Edge, len(pending))
	var moves []graph.EdgeReindex
	for _, original := range pending {
		e := cloneEdgeForResolve(original)
		oldTo, changed := native.resolveEdge(e, &ResolveStats{})
		oracle[e.Line] = *e
		if changed {
			// Do not clear the terminal on oracle's copy of the tentative
			// Meta: the caller of resolveRebuildFunction owns that step.
			saved := *e
			saved.Meta = maps.Clone(e.Meta)
			clearEdgeTerminal(&saved)
			moves = append(moves, graph.EdgeReindex{Edge: &saved, OldTo: oldTo, OldKind: original.Kind})
		}
		releaseResolverClone(e)
	}
	require.Len(t, moves, len(cases)-2)
	require.True(t, sql.ReindexEdgesApplied(moves))
	want := sql.AllEdges()
	slices.SortFunc(want, compareRebuildImportEdges)

	require.NoError(t, runRebuildLinkFrontier(ctx, build, opts, nil, func(f *rebuildLinkFrontier, page []*graph.Edge) error {
		var moves []graph.EdgeReindex
		for _, original := range page {
			before, e := *original, *original
			before.Meta = maps.Clone(original.Meta)
			if err := resolveRebuildFunction(ctx, f, &e, graph.UnresolvedName(e.To)); err != nil {
				return err
			}
			require.Equal(t, before, *original, "original line %d", original.Line)
			require.Equal(t, oracle[e.Line], e, "tentative line %d", e.Line)
			if e.To != original.To {
				e.Meta = maps.Clone(e.Meta)
				clearEdgeTerminal(&e)
				moves = append(moves, graph.EdgeReindex{Edge: &e, OldTo: original.To, OldKind: original.Kind})
			}
		}
		return build.ApplyLinkReindexes(ctx, moves)
	}))
	got := rebuildFnPointerSaved(t, build)
	require.Equal(t, want, got)
	require.Len(t, got, len(cases))
	for _, e := range got {
		tc := cases[e.Line-1]
		require.Equal(t, tc.to, e.To, "line %d", e.Line)
		require.Equal(t, tc.wantKind, e.Kind, "line %d", e.Line)
		require.Equal(t, tc.wantOrigin, e.Origin, "line %d", e.Line)
		require.Equal(t, tc.confidence, e.Confidence, "line %d", e.Line)
		require.Equal(t, e.To == "unresolved::"+tc.name, edgeTerminalFlag(e), "line %d", e.Line)
	}
	checkFrontierScratch(t, opts.ScratchParent)
}

func TestRebuildFunctionRefusesMissingRule(t *testing.T) {
	for _, tc := range []struct {
		lang, file, targetFile, id, reason string
		kind                               graph.NodeKind
	}{
		{"typescript", "caller.ts", "imported.ts", "imported.ts::Run", "JS/TS import closure", graph.KindFunction},
		{"csharp", "caller.cs", "using.cs", "csRun", "C# first-use visibility", graph.KindMethod},
	} {
		t.Run(tc.lang, func(t *testing.T) {
			ctx, build, opts := frontierFixture(t, 1, 0)
			build.AddBatch([]*graph.Node{
				{ID: "a-go", Name: "caller", RepoPrefix: "repo", Language: "go", FilePath: "caller.go", Kind: graph.KindFunction},
				{ID: "goRun", Name: "Run", RepoPrefix: "repo", Language: "go", FilePath: "caller.go", Kind: graph.KindFunction},
				{ID: "z-other", Name: "caller", RepoPrefix: "repo", Language: tc.lang, FilePath: tc.file, Kind: graph.KindFunction},
				{ID: tc.id, Name: "Run", RepoPrefix: "repo", Language: tc.lang, FilePath: tc.targetFile, Kind: tc.kind},
			}, []*graph.Edge{
				{From: "a-go", To: "unresolved::Run", Kind: graph.EdgeCalls, FilePath: "caller.go"},
				{From: "z-other", To: "unresolved::Run", Kind: graph.EdgeCalls, FilePath: tc.file,
					Meta: map[string]any{"resolution": "using_static"}},
			})
			want := rebuildFnPointerSaved(t, build)
			err := runRebuildLinkFrontier(ctx, build, opts, nil, func(f *rebuildLinkFrontier, page []*graph.Edge) error {
				require.Len(t, page, 2)
				first, later := *page[0], *page[1]
				require.NoError(t, resolveRebuildFunction(ctx, f, &first, "Run"))
				require.Equal(t, "goRun", first.To)
				before := later
				before.Meta = maps.Clone(later.Meta)
				err := resolveRebuildFunction(ctx, f, &later, "Run")
				require.Equal(t, before, later)
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
