package resolver

import (
	"maps"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/graph/store_pebble"
	"github.com/zzet/gortex/internal/graph/store_sqlite"
)

func typeTestNode(id, name string, kind graph.NodeKind, lang, file string) *graph.Node {
	return &graph.Node{ID: id, Name: name, Kind: kind, Language: lang, FilePath: file, RepoPrefix: "repo"}
}

func typeTestEdge(from, file, name string, kind graph.EdgeKind, fqn string) *graph.Edge {
	e := &graph.Edge{From: from, FilePath: file, To: "unresolved::" + name, Kind: kind,
		Confidence: 0.75, ConfidenceLabel: "medium", Origin: graph.OriginASTInferred, Tier: "ast",
		Meta: map[string]any{"payload": name}}
	if fqn != "" {
		e.Meta["target_fqn"] = fqn
	}
	return e
}

// Run the pinned selectors with their real warmed SQLite lookup caches. The
// expected IDs below are independent of both implementations. Compare the
// complete persisted rows, not just the number of resolved edges.
func checkRebuildTypeRows(t *testing.T, nodes []*graph.Node, edges []*graph.Edge, targets []string, refusal string,
	receipts ...store_pebble.ParseFacts,
) {
	t.Helper()
	require.Len(t, targets, len(edges))
	for i, e := range edges {
		e.Line = i + 1
	}
	g, err := store_sqlite.Open(filepath.Join(t.TempDir(), "native.sqlite"))
	require.NoError(t, err)
	defer func() { require.NoError(t, g.Close()) }()
	require.NoError(t, g.AddBatchChecked(nodes, edges))
	pending := g.AllEdges()
	require.Len(t, pending, len(edges))
	r := New(g)
	require.NoError(t, r.warmLookupCache(pending))
	defer r.clearLookupCache()
	var stats ResolveStats
	var moves []graph.EdgeReindex
	for _, e := range pending {
		old := e.To
		if e.Kind == graph.EdgeInstantiates || e.Kind == graph.EdgeReferences {
			r.resolveTypeOrFunc(e, graph.UnresolvedName(old), &stats)
		} else {
			r.resolveTypeRef(e, graph.UnresolvedName(old), &stats)
		}
		if e.To != old {
			moves = append(moves, graph.EdgeReindex{Edge: e, OldTo: old, OldKind: e.Kind})
		}
	}
	g.ReindexEdges(moves)
	want := g.AllEdges()
	require.Len(t, want, len(edges))
	for _, e := range want {
		before := *edges[e.Line-1]
		before.To = targets[e.Line-1]
		require.Equal(t, &before, e)
	}

	ctx, b, opts := frontierFixture(t, 0)
	b.AddBatch(nodes, edges)
	in := RebuildMainInputs{}
	if len(receipts) != 0 {
		in.Receipts = rebuildGRPCReceipts(receipts...)
	}
	err = runRebuildMain(ctx, b, opts, false, in)
	if refusal != "" {
		require.ErrorIs(t, err, store_pebble.ErrRebuildLink)
		require.ErrorContains(t, err, refusal)
		want = edges // the single tentative page was not applied
	} else {
		require.NoError(t, err)
	}
	rebuildFnPointerSortRows(want)
	require.Equal(t, want, rebuildFnPointerSaved(t, b))
	checkFrontierScratch(t, opts.ScratchParent)
}

func TestRebuildTypeScopedChoices(t *testing.T) {
	const local = "loc/use.go"
	empty := typeTestNode("z-empty", "Detached", graph.KindType, "rust", "else/defs")
	empty.RepoPrefix = ""
	generic := typeTestNode("z-generic", "Thunk", graph.KindFunction, "go", "else/run.go")
	generic.Meta = map[string]any{"type_params": "T"}
	nodes := []*graph.Node{
		typeTestNode("caller", "Caller", graph.KindFunction, "go", local),
		typeTestNode("a-other-language", "Widget", graph.KindType, "rust", local),
		typeTestNode("b-local-test", "Widget", graph.KindType, "go", "loc/mock_test.go"),
		typeTestNode("z-real", "Widget", graph.KindInterface, "go", "else/real.go"),
		typeTestNode("z-neutral", "Action", graph.KindMethod, "", "else/a.go"),
		typeTestNode("a-go", "Action", graph.KindFunction, "go", "else/b.go"),
		typeTestNode("z-local", "Task", graph.KindMethod, "go", local),
		typeTestNode("a-remote", "Task", graph.KindFunction, "", "else/a.go"),
		typeTestNode("a-wrong-repo", "Detached", graph.KindType, "go", local), empty,
		typeTestNode("a-plain", "Thunk", graph.KindType, "go", local), generic,
		typeTestNode("dep::terminal", "Widget", graph.KindType, "go", local),
	}
	edges := []*graph.Edge{
		typeTestEdge("caller", local, "*.Widget", graph.EdgeExtends, ""),
		typeTestEdge("caller", local, "Action", graph.EdgeReferences, ""),
		typeTestEdge("caller", local, "Task", graph.EdgeInstantiates, ""),
		typeTestEdge("caller", local, "Task", graph.EdgeReturns, ""),
		typeTestEdge("repo/missing", local, "Detached", graph.EdgeTypedAs, ""),
		typeTestEdge("caller", local, "Thunk", graph.EdgeReferences, ""),
	}
	edges[5].Meta[graph.MetaGenericInstantiation] = true
	checkRebuildTypeRows(t, nodes, edges, []string{"z-real", "z-neutral", "z-local", "unresolved::Task", "z-empty", "z-generic"}, "")
}

func testNamespacedType(id, name, lang, namespace string) *graph.Node {
	n := typeTestNode(id, name, graph.KindType, lang, "defs/"+id)
	n.Meta = map[string]any{MetaScopeNamespace: namespace}
	return n
}

func TestRebuildTypeNamespaceChoices(t *testing.T) {
	const php, cs = "loc/use.php", "loc/use.cs"
	inScope := typeTestNode("z-factory", "Factory", graph.KindFunction, "php", "defs/F.php")
	inScope.Meta = map[string]any{MetaScopeNamespace: "Model"}
	csCaller := typeTestNode("cs", "CSCaller", graph.KindFunction, "csharp", cs)
	aDialog := testNamespacedType("a-dialog", "Dialog", "csharp", "UI")
	bDialog := testNamespacedType("b-dialog", "Dialog", "csharp", "UI")
	nodes := []*graph.Node{
		testNamespacedType("a-factory-type", "Factory", "php", "Other"), inScope,
		typeTestNode("php", "PHPCaller", graph.KindFunction, "php", php),
		csCaller,
		testNamespacedType("a-decoy", "User", "php", "Other"),
		testNamespacedType("z-user", "User", "php", "Model"),
		testNamespacedType("alias-decoy", "Nickname", "php", "Model"),
		testNamespacedType("a-button", "Button", "csharp", "Other.UI"),
		testNamespacedType("z-button", "Button", "csharp", "Product.Widgets"),
		bDialog, aDialog,
	}
	edges := []*graph.Edge{
		typeTestEdge("php", php, "User", graph.EdgeReferences, `Model\User`),
		typeTestEdge("php", php, "User", graph.EdgeInstantiates, `Vendor\User`),
		typeTestEdge("php", php, "Nickname", graph.EdgeTypedAs, `Model\Missing`),
		typeTestEdge("php", php, "Nickname", graph.EdgeTypedAs, `Model\User`),
		typeTestEdge("cs", cs, "Button", graph.EdgeImplements, "Widgets.Button"),
		typeTestEdge("cs", cs, "Dialog", graph.EdgeReferences, "Absent.Dialog"),
		typeTestEdge("php", php, "Factory", graph.EdgeReferences, `Model\Factory`),
		typeTestEdge("php", php, "Factory", graph.EdgeExtends, `Model\Factory`),
	}
	// This alias itself warms its canonical short name in a PHP-visible scope.
	edges[3].Meta["receiver_type"] = "User"
	// The two UI.Dialog candidates tie for type selection and form a partial
	// group in the cold tail. Keep the tie and supply its original facts.
	checkRebuildTypeRows(t, nodes, edges, []string{"z-user", "a-decoy", "unresolved::Nickname", "z-user", "z-button", "a-dialog", "z-factory", "unresolved::Factory"}, "",
		rebuildEventFacts(aDialog.FilePath, "csharp", []*graph.Node{aDialog}),
		rebuildEventFacts(bDialog.FilePath, "csharp", []*graph.Node{bDialog}),
		rebuildEventFacts(cs, "csharp", []*graph.Node{csCaller}, edges[5]),
	)
}

func TestRebuildTypeMissingPageScopes(t *testing.T) {
	const file = "use.php"
	nodes := []*graph.Node{
		typeTestNode("php", "PHPCaller", graph.KindFunction, "php", file),
		typeTestNode("go", "GoCaller", graph.KindFunction, "go", "use.go"),
		testNamespacedType("real", "User", "php", "Model"),
		testNamespacedType("decoy", "Nickname", "php", "Model"),
	}
	for _, mode := range []string{"not-warmed", "warmed-negative"} {
		t.Run(mode, func(t *testing.T) {
			edges := []*graph.Edge{typeTestEdge("php", file, "Nickname", graph.EdgeTypedAs, `Model\User`)}
			targets := []string{"real"}
			if mode == "warmed-negative" {
				edges = append(edges, typeTestEdge("go", "use.go", "User", graph.EdgeReferences, ""))
				targets = []string{"unresolved::Nickname", "unresolved::User"}
			}
			checkRebuildTypeRows(t, nodes, edges, targets, "needs page name scopes")
		})
	}
}

func TestRebuildTypeMissingVisibility(t *testing.T) {
	const file = "src/page/A.cs"
	nodes := []*graph.Node{
		typeTestNode("cs", "Caller", graph.KindFunction, "csharp", file),
		typeTestNode("src/App.csproj", "App.csproj", graph.KindFile, "", "src/App.csproj"),
		testNamespacedType("a-decoy", "User", "csharp", "Other"),
		testNamespacedType("z-user", "User", "csharp", "Model"),
	}
	edges := []*graph.Edge{typeTestEdge("cs", file, "User", graph.EdgeReferences, "")}
	t.Run("bare", func(t *testing.T) {
		checkRebuildTypeRows(t, nodes, edges, []string{"a-decoy"}, "needs C# namespace visibility")
	})
	t.Run("project-global", func(t *testing.T) {
		file := typeTestNode("src/sibling/Usings.cs", "Usings.cs", graph.KindFile, "csharp", "src/sibling/Usings.cs")
		file.Meta = map[string]any{"global_usings": []string{"Model"}}
		checkRebuildTypeRows(t, append(nodes, file), edges, []string{"z-user"}, "needs C# namespace visibility")
	})
}

// Both transports run the same candidate reducer against the same unchanged
// precommit view. Expected targets/refusals are independently specified.
// The mixed main reducer also proves that Vue fallback stays conditional.
func TestRebuildTypePrimaryBatch(t *testing.T) {
	ctx, b, opts := frontierFixture(t, 0)
	const goFile, phpFile, csFile = "src/use.go", "src/use.php", "src/use.cs"
	const vueFile, fallbackFile, panel = "ui/View.vue", "ui/Fallback.vue", "ui/Panel.vue"
	nodes := []*graph.Node{
		typeTestNode("go", "Caller", graph.KindFunction, "go", goFile),
		typeTestNode("php", "Caller", graph.KindFunction, "php", phpFile),
		typeTestNode("cs", "Caller", graph.KindFunction, "csharp", csFile),
		typeTestNode("go-A", "A", graph.KindType, "go", "defs/a.go"),
		typeTestNode("wrong-A", "A", graph.KindType, "rust", goFile),
		typeTestNode("go-B", "B", graph.KindFunction, "go", goFile),
		typeTestNode("web-B", "B", graph.KindType, "typescript", "ui/b.ts"),
		testNamespacedType("php-User", "User", "php", "Model"),
		testNamespacedType("cs-other", "User", "csharp", "Other"),
		testNamespacedType("cs-model", "User", "csharp", "Model"),
		typeTestNode(vueFile+"::Source", "Source", graph.KindFunction, "vue", vueFile),
		typeTestNode(fallbackFile+"::Source", "Source", graph.KindFunction, "vue", fallbackFile),
		typeTestNode(panel, "Panel.vue", graph.KindFile, "vue", panel),
		typeTestNode(panel+"::A", "A", graph.KindFunction, "vue", panel),
	}
	// Duplicate A positions
	// remain distinct; a type branch never falls back to a function.
	edges := []*graph.Edge{
		typeTestEdge("go", goFile, "*.A", graph.EdgeExtends, ""),
		typeTestEdge("go", goFile, "A", graph.EdgeReferences, ""),
		typeTestEdge("go", goFile, "B", graph.EdgeInstantiates, ""),
		typeTestEdge("go", goFile, "B", graph.EdgeReturns, ""),
		typeTestEdge("go", goFile, "Missing", graph.EdgeTypedAs, ""),
		typeTestEdge("php", phpFile, "Nickname", graph.EdgeTypedAs, `Model\User`),
		typeTestEdge("cs", csFile, "User", graph.EdgeReferences, ""),
		typeTestEdge("cs", csFile, "User", graph.EdgeImplements, "Model.User"),
		rebuildVueTestRef(vueFile, "A", "A", 9),
		rebuildVueTestRef(fallbackFile, "B", "B", 10),
		typeTestEdge("go", goFile, "grpc::Skip", graph.EdgeReferences, ""),
		typeTestEdge("go", goFile, "pyrel::Skip", graph.EdgeTypedAs, ""),
		typeTestEdge("go", goFile, "A", graph.EdgeTests, ""),
	}
	for i, e := range edges {
		e.Line = i + 1
	}
	b.AddBatch(nodes, append(edges, rebuildVueTestImport(vueFile, panel, "A", 1)))
	before := rebuildFnPointerSaved(t, b)
	type expectation struct {
		target        string
		allowFunction bool
		refusal       string
	}
	want := map[int]expectation{
		1: {target: "go-A"}, 2: {target: "go-A", allowFunction: true},
		3: {target: "go-B", allowFunction: true}, 4: {target: "unresolved::B"},
		5: {target: "unresolved::Missing"},
		6: {target: "unresolved::Nickname", refusal: "needs page name scopes"},
		7: {target: "unresolved::User", allowFunction: true, refusal: "needs C# namespace visibility"},
		8: {target: "cs-model"},
	}
	type outcome struct {
		old     graph.EdgeIdentity
		row     graph.Edge
		refusal string
	}
	observe := func(batched bool) ([]outcome, store_pebble.LinkRequestProgress) {
		var rows []outcome
		var counts store_pebble.LinkRequestProgress
		require.NoError(t, runRebuildLinkFrontier(ctx, b, opts, nil, func(f *rebuildLinkFrontier, page []*graph.Edge) error {
			require.Len(t, page, len(edges))
			var queries []rebuildTypeQuery
			if batched {
				var err error
				queries, err = resolveRebuildTypePage(ctx, f, page)
				if err != nil {
					return err
				}
			}
			for i, e := range page {
				expected, primary := want[e.Line]
				if !primary {
					if batched {
						require.Empty(t, queries[i].request.Key)
					}
					continue
				}
				copy := *e
				var err error
				if batched {
					require.NotEmpty(t, queries[i].request.Key)
					err = queries[i].finish(&copy)
				} else {
					err = resolveRebuildType(ctx, f, &copy, graph.UnresolvedName(e.To), expected.allowFunction)
				}
				var refused string
				if expected.refusal != "" {
					require.ErrorIs(t, err, store_pebble.ErrRebuildLink)
					require.ErrorContains(t, err, expected.refusal)
					refused = err.Error()
				} else {
					require.NoError(t, err)
				}
				w := *e
				w.To = expected.target
				require.Equal(t, w, copy)
				rows = append(rows, outcome{graph.EdgeIdentityFor(e), copy, refused})
			}
			counts = f.link.RequestProgress()
			return nil
		}))
		checkFrontierScratch(t, opts.ScratchParent)
		require.Len(t, rows, len(want))
		return rows, counts
	}
	serial, unbatched := observe(false)
	batch, batched := observe(true)
	require.Equal(t, serial, batch, "complete old keys, payloads and refusal sequence")
	require.Equal(t, store_pebble.LinkRequestProgress{CandidateCalls: 8, CandidatePositions: 8}, unbatched)
	require.Equal(t, store_pebble.LinkRequestProgress{CandidateCalls: 1, CandidatePositions: 8}, batched)

	// The actual mixed main reducer must return the first original
	// semantic refusal. Its tentative successful rows are not applied.
	var first string
	for _, row := range serial {
		if row.refusal != "" {
			first = row.refusal
			break
		}
	}
	require.NotEmpty(t, first)
	err := runRebuildLinkFrontier(ctx, b, opts, nil, func(f *rebuildLinkFrontier, page []*graph.Edge) error {
		moves, err := reduceRebuildMainPage(ctx, f, page)
		require.Empty(t, moves)
		require.EqualError(t, err, first)
		return err
	})
	require.ErrorIs(t, err, store_pebble.ErrRebuildLink)
	require.Equal(t, before, rebuildFnPointerSaved(t, b))
	checkFrontierScratch(t, opts.ScratchParent)

	// With just the two deliberately refused sites removed, assert
	// actual mixed-page move order and payload. Vue hit issues no type
	// query. The Vue import miss still issues one conditional query.
	require.NoError(t, runRebuildLinkFrontier(ctx, b, opts, func(page []*graph.Edge) ([]*graph.Edge, error) {
		var kept []*graph.Edge
		for _, e := range page {
			if e.Line != 6 && e.Line != 7 {
				kept = append(kept, e)
			}
		}
		return kept, nil
	}, func(f *rebuildLinkFrontier, page []*graph.Edge) error {
		var moves []graph.EdgeReindex
		for _, e := range page {
			copy := *e
			switch e.Line {
			case 1, 2, 3, 8:
				copy.To = want[e.Line].target
			case 9:
				copy = *rebuildVueTestSaved(e, panel+"::A", true)
			case 10:
				copy.To = "web-B"
			default:
				continue
			}
			copy.Meta = maps.Clone(copy.Meta)
			clearEdgeTerminal(&copy)
			moves = append(moves, graph.EdgeReindex{Edge: &copy, OldTo: e.To, OldKind: e.Kind})
		}
		got, err := reduceRebuildMainPage(ctx, f, page)
		require.NoError(t, err)
		require.Len(t, moves, 6)
		require.Equal(t, moves, got)
		require.Equal(t, store_pebble.LinkRequestProgress{CandidateCalls: 2, CandidatePositions: 7}, f.link.RequestProgress())
		return nil
	}))
	require.Equal(t, before, rebuildFnPointerSaved(t, b))
	checkFrontierScratch(t, opts.ScratchParent)
}

func TestRebuildTypePrimaryBatchWireBound(t *testing.T) {
	ctx, b, opts := frontierFixture(t, 0)
	// Each request fits the existing wire cap. Together they do not. No
	// raised bound, retry, partial update, or singleton fallback is allowed.
	a := typeTestEdge("caller", "use.go", strings.Repeat("a", 1100<<10), graph.EdgeReferences, "")
	z := typeTestEdge("caller", "use.go", strings.Repeat("z", 1100<<10), graph.EdgeReferences, "")
	b.AddBatch(nil, []*graph.Edge{a, z})
	before := rebuildFnPointerSaved(t, b)
	err := runRebuildLinkFrontier(ctx, b, opts, nil, func(f *rebuildLinkFrontier, page []*graph.Edge) error {
		require.Len(t, page, 2)
		moves, err := reduceRebuildMainPage(ctx, f, page)
		require.Empty(t, moves)
		require.ErrorIs(t, err, store_pebble.ErrRebuildLink)
		require.ErrorContains(t, err, "wire byte bound")
		require.Equal(t, store_pebble.LinkRequestProgress{CandidateCalls: 1}, f.link.RequestProgress())
		return err
	})
	require.ErrorIs(t, err, store_pebble.ErrRebuildLink)
	require.Equal(t, before, rebuildFnPointerSaved(t, b))
	checkFrontierScratch(t, opts.ScratchParent)
}
