package resolver

import (
	"fmt"
	"maps"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/graph/store_pebble"
	"github.com/zzet/gortex/internal/graph/store_sqlite"
	"github.com/zzet/gortex/internal/parser/languages"
)

func TestRebuildFunctionNativeRows(t *testing.T) {
	ctx, build, opts := frontierFixture(t, 0)
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
			ctx, build, opts := frontierFixture(t, 0)
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

// The main pass sees the extractor's prefixed read, not the declaration's
// bare name. A compatible full-name hit is deliberately unsupported.
func TestRebuildFunctionValueRefFullNameMiss(t *testing.T) {
	const file, name = "src/config.go", "MAX_RETRIES"
	const target = "valueref::" + name
	facts, err := languages.NewGoExtractor().Extract(file, []byte(`package config
const MAX_RETRIES = 3
func Limit() int { return MAX_RETRIES }
`))
	require.NoError(t, err)
	defer facts.ReleaseTree()
	var selected []*graph.Edge
	for _, e := range facts.Edges {
		if e.To == "unresolved::"+target && e.Kind == graph.EdgeReads {
			selected = append(selected, e)
		}
	}
	require.Len(t, selected, 1)
	read := selected[0]
	require.Equal(t, graph.OriginSpeculative, read.Origin)
	require.Equal(t, valueRefCandidateVia, read.Meta["via"])
	require.Equal(t, name, read.Meta["name"])
	setEdgeTerminal(read, terminalReasonNoDefinition)
	before := cloneFrameworkEdge(read)
	var foundDeclaration bool
	for _, n := range facts.Nodes {
		n.RepoPrefix = "repo"
		foundDeclaration = foundDeclaration || (n.Name == name && n.Kind == graph.KindConstant)
	}
	require.True(t, foundDeclaration, "bare declaration must exist")
	for _, tc := range []struct {
		label string
		hit   bool
	}{{"miss", false}, {"compatible_hit", true}} {
		t.Run(tc.label, func(t *testing.T) {
			hit := tc.hit
			nodes := slices.Clone(facts.Nodes)
			// These exact-name decoys are excluded by the native request scope.
			nodes = append(nodes,
				&graph.Node{ID: "other-repo", Name: target, RepoPrefix: "other", Language: "go", FilePath: file, Kind: graph.KindFunction},
				&graph.Node{ID: "other-language", Name: target, RepoPrefix: "repo", Language: "python", FilePath: file, Kind: graph.KindFunction},
			)
			if hit {
				// A language-neutral definition is also native-compatible.
				nodes = append(nodes, &graph.Node{ID: "compatible", Name: target, RepoPrefix: "repo", FilePath: file, Kind: graph.KindFunction})
			}
			g := graph.New()
			g.AddBatch(nodes, selected)
			native := New(g)
			native.bulkMode = true
			require.NoError(t, native.warmLookupCache(selected))
			defer native.clearLookupCache()
			e := cloneFrameworkEdge(read)
			_, changed := native.resolveEdge(e, &ResolveStats{})
			require.Equal(t, hit, changed)
			if hit {
				require.Equal(t, "compatible", e.To)
			} else {
				require.Equal(t, before, e)
			}

			ctx, build, opts := frontierFixture(t, 0)
			opts.SpillBytes = 1
			build.AddBatch(nodes, selected)
			err := runRebuildLinkFrontier(ctx, build, opts, nil, func(f *rebuildLinkFrontier, page []*graph.Edge) error {
				require.Equal(t, []*graph.Edge{before}, page)
				moves, err := reduceRebuildMainPage(ctx, f, page)
				require.Empty(t, moves)
				require.Equal(t, []*graph.Edge{before}, page)
				return err
			})
			if hit {
				require.ErrorIs(t, err, errRebuildMainRule)
				require.ErrorContains(t, err, "non-bare value-ref candidate")
			} else {
				require.NoError(t, err)
			}
			require.Equal(t, []*graph.Edge{before}, rebuildFnPointerSaved(t, build))
			require.Equal(t, before, read)
			checkFrontierScratch(t, opts.ScratchParent)
		})
	}
}

func TestRebuildFunctionValueRefRefusesOtherNonBareRows(t *testing.T) {
	const name = "MAX_RETRIES"
	const target = "valueref::" + name
	for _, tc := range []struct {
		label, target, name, origin, via string
		kind                             graph.EdgeKind
	}{
		{"call", target, name, graph.OriginSpeculative, valueRefCandidateVia, graph.EdgeCalls},
		{"origin", target, name, graph.OriginASTResolved, valueRefCandidateVia, graph.EdgeReads},
		{"via", target, name, graph.OriginSpeculative, "other", graph.EdgeReads},
		{"name", target, "OTHER", graph.OriginSpeculative, valueRefCandidateVia, graph.EdgeReads},
		{"empty", "valueref::", "", graph.OriginSpeculative, valueRefCandidateVia, graph.EdgeReads},
		{"qualified_suffix", "valueref::pkg::MAX_RETRIES", "pkg::MAX_RETRIES", graph.OriginSpeculative, valueRefCandidateVia, graph.EdgeReads},
		{"other_family", "other::" + name, name, graph.OriginSpeculative, valueRefCandidateVia, graph.EdgeReads},
	} {
		t.Run(tc.label, func(t *testing.T) {
			e := &graph.Edge{From: "caller", To: "unresolved::" + tc.target, Kind: tc.kind, Origin: tc.origin,
				FilePath: "caller.go", Line: 7, Confidence: 0.42, Meta: map[string]any{"via": tc.via, "name": tc.name}}
			before := cloneFrameworkEdge(e)
			// Refusal occurs before any view or candidate access.
			require.ErrorIs(t, resolveRebuildFunction(t.Context(), nil, e, tc.target), errRebuildMainRule)
			require.Equal(t, before, e)
		})
	}
}

// Actual Go extraction supplies the marker and channel expression provenance.
// The same full-name native lookup must preserve each miss, including payload,
// and a compatible hit deliberately remains outside the private rule.
func TestRebuildFunctionGoFullNameMiss(t *testing.T) {
	const file = "src/executor.go"
	facts, err := languages.NewGoExtractor().Extract(file, []byte(`package sample
 type Dispatcher struct { Name string; C chan int }
 func (p *Dispatcher) Done() chan int { return p.C }
 func Run(p *Dispatcher) {
     _ = Dispatcher{Name: "activity"}
     <-p.Done()
     <-p.C
     p.C <- 1
 }
`))
	require.NoError(t, err)
	defer facts.ReleaseTree()
	for _, n := range facts.Nodes {
		n.RepoPrefix = "repo"
	}
	for _, tc := range []struct {
		label, target, suffix, family string
		kind                          graph.EdgeKind
	}{
		{"marker", "temporal-executor::Dispatcher::Name", "Name", "temporal executor-field", graph.EdgeCalls},
		{"receive_call", "p.Done()", "Done", "channel expression", graph.EdgeRecvs},
		{"receive_field", "p.C", "C", "channel expression", graph.EdgeRecvs},
		{"send_field", "p.C", "C", "channel expression", graph.EdgeSends},
	} {
		t.Run(tc.label, func(t *testing.T) {
			var selected []*graph.Edge
			for _, e := range facts.Edges {
				if e.Kind == tc.kind && e.To == "unresolved::"+tc.target {
					selected = append(selected, cloneFrameworkEdge(e))
				}
			}
			require.Len(t, selected, 1)
			row := selected[0]
			if tc.kind == graph.EdgeCalls {
				require.Equal(t, "temporal.executor-field", row.Meta["via"])
				require.Equal(t, "Dispatcher", row.Meta["executor_type"])
				require.Equal(t, "Name", row.Meta["executor_field"])
				require.Equal(t, "activity", row.Meta["executor_value"])
			} else {
				require.Equal(t, graph.OriginASTInferred, row.Origin)
			}
			setEdgeTerminal(row, terminalReasonNoDefinition)
			before := cloneFrameworkEdge(row)
			for _, hit := range []bool{false, true} {
				t.Run(fmt.Sprintf("hit=%v", hit), func(t *testing.T) {
					nodes := append(slices.Clone(facts.Nodes),
						&graph.Node{ID: "suffix", Name: tc.suffix, RepoPrefix: "repo", Language: "go", FilePath: file, Kind: graph.KindFunction},
						&graph.Node{ID: "other-repo", Name: tc.target, RepoPrefix: "other", Language: "go", FilePath: file, Kind: graph.KindFunction},
						&graph.Node{ID: "other-language", Name: tc.target, RepoPrefix: "repo", Language: "python", FilePath: file, Kind: graph.KindFunction},
					)
					if hit {
						nodes = append(nodes, &graph.Node{ID: "compatible", Name: tc.target, RepoPrefix: "repo", FilePath: file, Kind: graph.KindFunction})
					}
					g := graph.New()
					g.AddBatch(nodes, selected)
					native := New(g)
					native.bulkMode = true
					require.NoError(t, native.warmLookupCache(selected))
					defer native.clearLookupCache()
					e := cloneFrameworkEdge(row)
					_, changed := native.resolveEdge(e, &ResolveStats{})
					require.Equal(t, hit, changed)
					if hit {
						require.Equal(t, "compatible", e.To)
					} else {
						require.Equal(t, before, e)
					}

					ctx, build, opts := frontierFixture(t, 0)
					opts.SpillBytes = 1
					build.AddBatch(nodes, selected)
					err := runRebuildLinkFrontier(ctx, build, opts, nil, func(f *rebuildLinkFrontier, page []*graph.Edge) error {
						require.Equal(t, []*graph.Edge{before}, page)
						moves, err := reduceRebuildMainPage(ctx, f, page)
						require.Empty(t, moves)
						require.Equal(t, []*graph.Edge{before}, page)
						require.Equal(t, store_pebble.LinkRequestProgress{CandidateCalls: 1, CandidatePositions: 1}, f.link.RequestProgress())
						return err
					})
					if hit {
						require.ErrorIs(t, err, errRebuildMainRule)
						require.ErrorContains(t, err, "non-bare "+tc.family+" candidate")
					} else {
						require.NoError(t, err)
					}
					require.Equal(t, []*graph.Edge{before}, rebuildFnPointerSaved(t, build))
					require.Equal(t, before, row)
					checkFrontierScratch(t, opts.ScratchParent)
				})
			}
		})
	}
}

func TestRebuildFunctionGoFullNameRefusesOtherRows(t *testing.T) {
	marker := &graph.Edge{From: "caller", To: "unresolved::temporal-executor::Dispatcher::Name", FilePath: "caller.go", Kind: graph.EdgeCalls,
		Meta: map[string]any{"via": "temporal.executor-field", "executor_type": "Dispatcher", "executor_field": "Name", "executor_value": "activity"}}
	channel := &graph.Edge{From: "caller", To: "unresolved::p.Done()", FilePath: "caller.go", Kind: graph.EdgeRecvs, Origin: graph.OriginASTInferred}
	for _, tc := range []struct {
		label  string
		base   *graph.Edge
		mutate func(*graph.Edge)
	}{
		{"marker_kind", marker, func(e *graph.Edge) { e.Kind = graph.EdgeReads }},
		{"marker_via", marker, func(e *graph.Edge) { delete(e.Meta, "via") }},
		{"marker_type", marker, func(e *graph.Edge) { e.Meta["executor_type"] = "Other" }},
		{"marker_value", marker, func(e *graph.Edge) { delete(e.Meta, "executor_value") }},
		{"marker_extra_component", marker, func(e *graph.Edge) { e.To += "::Extra" }},
		{"channel_kind", channel, func(e *graph.Edge) { e.Kind = graph.EdgeCalls }},
		{"channel_origin", channel, func(e *graph.Edge) { e.Origin = graph.OriginSpeculative }},
		{"channel_path", channel, func(e *graph.Edge) { e.FilePath = "caller.ts" }},
		{"channel_marker", channel, func(e *graph.Edge) { e.To = "unresolved::other::p.Done()" }},
		{"channel_stale_using", channel, func(e *graph.Edge) { e.Meta = map[string]any{"resolution": "using_static"} }},
	} {
		t.Run(tc.label, func(t *testing.T) {
			e := cloneFrameworkEdge(tc.base)
			tc.mutate(e)
			before := cloneFrameworkEdge(e)
			require.ErrorIs(t, resolveRebuildFunction(t.Context(), nil, e, graph.UnresolvedName(e.To)), errRebuildMainRule)
			require.Equal(t, before, e)
		})
	}
}

// Temporal dispatch targets, including a goroutine's separate Spawn, use
// native main's exact full-name miss, not a framework-prefix exemption.
func TestRebuildFunctionTemporalDispatchFullNameMiss(t *testing.T) {
	const file = "src/workflow.go"
	facts, err := languages.NewGoExtractor().Extract(file, []byte(`package sample
import "go.temporal.io/sdk/workflow"
func Run(ctx workflow.Context) {
    go workflow.ExecuteActivity(ctx, "Charge", 1)
    go workflow.ExecuteChildWorkflow(ctx, "ChildWorkflow", 2)
}
`))
	require.NoError(t, err)
	defer facts.ReleaseTree()
	const activityTarget = "unresolved::temporal::activity::Charge"
	const workflowTarget = "unresolved::temporal::workflow::ChildWorkflow"
	var goCall, activitySpawn, workflowSpawn *graph.Edge
	for _, e := range facts.Edges {
		if e.To == workflowTarget && e.Kind == graph.EdgeSpawns {
			require.Nil(t, workflowSpawn)
			workflowSpawn = e
		}
		if e.To != activityTarget {
			continue
		}
		switch e.Kind {
		case graph.EdgeCalls:
			require.Nil(t, goCall)
			goCall = e
		case graph.EdgeSpawns:
			require.Nil(t, activitySpawn)
			activitySpawn = e
		}
	}
	require.NotNil(t, goCall)
	require.NotNil(t, activitySpawn)
	require.NotNil(t, workflowSpawn)

	java := languages.NewJavaExtractor()
	java.SetTemporalInvokers([]string{"Invoker"}, nil)
	javaFacts := rebuildRNSourceFacts(t, java, "src/Tasks.java", `public class Tasks {
    private final Invoker invoker;

    void run(WorkflowClient client) {
        invoker.invokeAsync("charge.task", options, input);
        client.newUntypedWorkflowStub("charge.task");
    }
}
`)
	var javaStub, javaStart *graph.Edge
	for _, e := range javaFacts.Edges {
		if e.Kind != graph.EdgeCalls || e.To != "unresolved::temporal::workflow::charge.task" {
			continue
		}
		switch e.Meta["via"] {
		case "temporal.stub":
			require.Nil(t, javaStub)
			javaStub = e
		case "temporal.start":
			require.Nil(t, javaStart)
			javaStart = e
		}
	}
	require.NotNil(t, javaStub)
	require.NotNil(t, javaStart)
	for _, tc := range []struct {
		name string
		edge *graph.Edge
		lang string
	}{
		{"Go call", goCall, "go"},
		{"Go activity spawn", activitySpawn, "go"},
		{"Go workflow spawn", workflowSpawn, "go"},
		{"Java stub", javaStub, "java"},
		{"Java start", javaStart, "java"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			row := tc.edge
			name := graph.UnresolvedName(row.To)
			suffix := name[strings.LastIndex(name, "::")+2:]
			for _, hit := range []bool{false, true} {
				t.Run(fmt.Sprintf("hit=%v", hit), func(t *testing.T) {
					caller := &graph.Node{ID: row.From, Name: "Run", FilePath: row.FilePath, Language: tc.lang, Kind: graph.KindFunction, RepoPrefix: "repo"}
					nodes := []*graph.Node{caller,
						{ID: "suffix", Name: suffix, Language: tc.lang, FilePath: row.FilePath, RepoPrefix: "repo", Kind: graph.KindFunction},
						{ID: "other-repo", Name: name, Language: tc.lang, FilePath: row.FilePath, RepoPrefix: "other", Kind: graph.KindFunction},
					}
					if hit {
						nodes = append(nodes, &graph.Node{ID: "hit", Name: name, Language: tc.lang, FilePath: row.FilePath, RepoPrefix: "repo", Kind: graph.KindFunction})
					}
					g := graph.New()
					g.AddBatch(nodes, []*graph.Edge{row})
					native := New(g)
					native.bulkMode = true
					require.NoError(t, native.warmLookupCache([]*graph.Edge{row}))
					defer native.clearLookupCache()
					want := cloneFrameworkEdge(row)
					_, changed := native.resolveEdge(want, &ResolveStats{})
					require.Equal(t, hit, changed)
					if hit {
						require.Equal(t, "hit", want.To)
					} else {
						require.Equal(t, row, want)
					}
					ctx, build, opts := frontierFixture(t, 0)
					opts.SpillBytes = 1
					build.AddBatch(nodes, []*graph.Edge{row})
					before := rebuildFnPointerSaved(t, build)
					err := runRebuildLinkFrontier(ctx, build, opts, nil, func(f *rebuildLinkFrontier, page []*graph.Edge) error {
						moves, err := reduceRebuildMainPage(ctx, f, page)
						require.Empty(t, moves)
						return err
					})
					if hit {
						require.ErrorIs(t, err, errRebuildMainRule)
						require.ErrorContains(t, err, "non-bare Temporal")
					} else {
						require.NoError(t, err)
					}
					require.Equal(t, before, rebuildFnPointerSaved(t, build))
				})
			}
		})
	}
}

func TestRebuildFunctionFrameworkMarkersRefuseOtherRows(t *testing.T) {
	activity := &graph.Edge{From: "caller", To: "unresolved::temporal::activity::Charge", FilePath: "caller.go", Kind: graph.EdgeCalls,
		Meta: map[string]any{"via": "temporal.stub", "temporal_kind": "activity", "temporal_name": "Charge"}}
	start := &graph.Edge{From: "caller", To: "unresolved::temporal::workflow::Charge", FilePath: "caller.java", Kind: graph.EdgeCalls,
		Meta: map[string]any{"via": "temporal.start", "temporal_kind": "workflow", "temporal_name": "Charge"}}
	spawn := &graph.Edge{From: "caller", To: activity.To, FilePath: activity.FilePath, Kind: graph.EdgeSpawns,
		Origin: graph.OriginASTResolved, Meta: map[string]any{"mode": "goroutine"}}
	rn := &graph.Edge{From: "caller", To: "unresolved::rn::Calendar::open", FilePath: "caller.ts", Kind: graph.EdgeCalls,
		Meta: map[string]any{"via": rnNativeVia, "rn_module": "Calendar", "rn_method": "open"}}
	for _, tc := range []struct {
		name   string
		edge   *graph.Edge
		mutate func(*graph.Edge)
	}{
		{"activity stub not a call", activity, func(e *graph.Edge) { e.Kind = graph.EdgeReads }},
		{"activity stub outside Go and Java", activity, func(e *graph.Edge) { e.FilePath = "caller.ts" }},
		{"activity stub in Java", activity, func(e *graph.Edge) { e.FilePath = "caller.java" }},
		{"activity stub wrong target", activity, func(e *graph.Edge) { e.To += "::other" }},
		{"activity stub missing via", activity, func(e *graph.Edge) { delete(e.Meta, "via") }},
		{"activity stub wrong temporal kind", activity, func(e *graph.Edge) { e.Meta["temporal_kind"] = "register_activity" }},
		{"workflow start in Go", start, func(e *graph.Edge) { e.FilePath = "caller.go" }},
		{"spawn wrong mode", spawn, func(e *graph.Edge) { e.Meta["mode"] = "thread" }},
		{"spawn wrong origin", spawn, func(e *graph.Edge) { e.Origin = graph.OriginASTInferred }},
		{"spawn outside Go", spawn, func(e *graph.Edge) { e.FilePath = "caller.java" }},
		{"spawn wrong prefix", spawn, func(e *graph.Edge) { e.To = "unresolved::temporal::other::Charge" }},
		{"workflow spawn empty name", spawn, func(e *graph.Edge) { e.To = "unresolved::temporal::workflow::" }},
		{"RN marker not a call", rn, func(e *graph.Edge) { e.Kind = graph.EdgeReads }},
		{"RN marker outside JS and TS", rn, func(e *graph.Edge) { e.FilePath = "caller.go" }},
		{"RN marker empty module", rn, func(e *graph.Edge) {
			e.Meta["rn_module"] = ""
			e.To = "unresolved::rn::::open" // Keep the placeholder consistent with the empty module.
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := cloneFrameworkEdge(tc.edge)
			tc.mutate(e)
			before := cloneFrameworkEdge(e)
			require.ErrorIs(t, resolveRebuildFunction(t.Context(), nil, e, graph.UnresolvedName(e.To)), errRebuildMainRule)
			require.Equal(t, before, e)
		})
	}
}
