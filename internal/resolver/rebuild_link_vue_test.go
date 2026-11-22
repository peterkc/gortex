package resolver

import (
	"cmp"
	"maps"
	"path/filepath"
	"slices"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/graph/store_sqlite"
)

func rebuildVueTestRef(file, name, raw string, line int) *graph.Edge {
	e := typeTestEdge(file+"::Source", file, name, graph.EdgeReferences, "")
	e.Line = line
	e.Meta["template"], e.Meta["template_name"] = true, raw
	e.Alias, e.Context, e.Via = "kept alias", "kept context", "kept via"
	return e
}

func rebuildVueTestImport(file, to, local string, line int) *graph.Edge {
	return &graph.Edge{From: file, FilePath: file, To: to, Kind: graph.EdgeImports,
		Line: line, Meta: map[string]any{"vue_default_import": local}}
}

func rebuildVueTestSaved(edge *graph.Edge, id string, bound bool) *graph.Edge {
	e := *edge
	e.To = id
	if bound {
		e.Origin, e.Confidence = graph.OriginASTResolved, 0.95
		e.Meta = maps.Clone(edge.Meta)
		if e.Meta == nil {
			e.Meta = map[string]any{}
		}
		e.Meta["resolution"] = "import_binding"
	}
	return &e
}

func rebuildVueNativeRows(t *testing.T, nodes []*graph.Node, edges []*graph.Edge, alias PathAliasResolver) []*graph.Edge {
	t.Helper()
	g, err := store_sqlite.Open(filepath.Join(t.TempDir(), "native.sqlite"))
	require.NoError(t, err)
	defer func() { require.NoError(t, g.Close()) }()
	require.NoError(t, g.AddBatchChecked(nodes, nil))
	// One insertion per transaction forces the pinned SQLite rowid order.
	for _, e := range edges {
		require.NoError(t, g.AddBatchChecked(nil, []*graph.Edge{e}))
	}
	return rebuildVueResolveRows(t, g, alias)
}

// The SQLite edge columns do not carry Context, Via or Alias. State the
// exact expected persisted difference without projecting any actual rows.
func rebuildVueSQLiteExpected(edges []*graph.Edge) []*graph.Edge {
	out := make([]*graph.Edge, 0, len(edges))
	for _, edge := range edges {
		e := *edge
		e.Context, e.Via, e.Alias = "", "", ""
		out = append(out, &e)
	}
	return out
}

func rebuildVueResolveRows(t *testing.T, g graph.Store, alias PathAliasResolver) []*graph.Edge {
	t.Helper()
	var pending []*graph.Edge
	for _, e := range g.AllEdges() {
		if e.Kind != graph.EdgeImports {
			// The in-memory graph lends live row pointers.
			copy := *e
			copy.Meta = maps.Clone(e.Meta)
			pending = append(pending, &copy)
		}
	}
	r := New(g)
	r.SetPathAliasResolver(alias)
	require.NoError(t, r.warmLookupCache(pending))
	defer r.clearLookupCache()
	var stats ResolveStats
	var moves []graph.EdgeReindex
	for _, e := range pending {
		if old, changed := r.resolveEdge(e, &stats); changed {
			moves = append(moves, graph.EdgeReindex{Edge: e, OldTo: old, OldKind: e.Kind})
		}
	}
	g.ReindexEdges(moves)
	return rebuildVueSortedRows(g.AllEdges())
}

func rebuildVueSortedRows(edges []*graph.Edge) []*graph.Edge {
	edges = slices.Clone(edges)
	rebuildFnPointerSortRows(edges)
	return edges
}

// Keep the fixture at the two selectors' reducer boundary: ordinary imports
// are saved evidence, not a second resolver path under test.
func rebuildVueWorkingRows(t *testing.T, nodes []*graph.Node, edges []*graph.Edge) ([]*graph.Edge, error) {
	t.Helper()
	ctx, b, opts := frontierFixture(t, 1, 0)
	b.AddBatch(nodes, edges)
	err := runRebuildLinkFrontier(ctx, b, opts, nil, func(f *rebuildLinkFrontier, page []*graph.Edge) error {
		var moves []graph.EdgeReindex
		for _, old := range page {
			if old.Kind == graph.EdgeImports {
				continue
			}
			e := *old
			if err := resolveRebuildVue(ctx, f, &e, graph.UnresolvedName(old.To)); err != nil {
				return err
			}
			if e.To != old.To {
				require.NotContains(t, old.Meta, "resolution", "tentative move mutated the old row")
				moves = append(moves, graph.EdgeReindex{Edge: &e, OldTo: old.To, OldKind: old.Kind})
			}
		}
		return b.ApplyLinkReindexes(ctx, moves)
	})
	checkFrontierScratch(t, opts.ScratchParent)
	return rebuildFnPointerSaved(t, b), err
}

func TestRebuildVueTemplateNativeRows(t *testing.T) {
	const file = "ui/Shop.vue"
	nodes := []*graph.Node{typeTestNode(file+"::Source", "Source", graph.KindType, "vue", file)}
	for _, name := range []string{"Panel", "Direct", "Missing", "Conflict", "Tail", "Ambiguous", "Local", "BareMiss", "NoSymbol", "Empty", "Symbols"} {
		nodes = append(nodes, typeTestNode("ui/local.ts::"+name, name, graph.KindType, "typescript", "ui/local.ts"))
	}
	for _, path := range []string{"ui/shop/Widget.vue", "ui/shop/Direct.vue", "ui/shop/NoSymbol.vue", "ui/emit.ts"} {
		nodes = append(nodes, typeTestNode(path, filepath.Base(path), graph.KindFile, "", path))
	}
	for _, d := range []struct{ file, name string }{
		{"ui/shop/Widget.vue", "Widget"}, {"ui/shop/Widget.vue", "default"},
		{"ui/shop/Direct.vue", "Direct"}, {"ui/shop/Direct.vue", "default"}, {"ui/emit.ts", "Emit"},
		{"ui/shop/Direct.vue", "Symbols"}, {"ui/shop/Direct.vue", "RawSymbols"},
		{"ui/shop/Tail.vue", "Tail"}, {"ui/other/Tail.vue", "Tail"},
		{"ui/shop/Ambiguous.vue", "Ambiguous"}, {"archive/shop/Ambiguous.vue", "Ambiguous"},
	} {
		n := typeTestNode(d.file+"::"+d.name, d.name, graph.KindFunction, "vue", d.file)
		n.Meta = map[string]any{"component": true}
		nodes = append(nodes, n)
	}
	imports := []*graph.Edge{
		rebuildVueTestImport(file, "unresolved::import::./shop/Widget.vue", "MyPanel", 1),
		rebuildVueTestImport(file, "ui/shop/Direct.vue", "Direct", 2),
		rebuildVueTestImport(file, "external::./missing/Missing.vue", "Missing", 3),
		rebuildVueTestImport(file, "unresolved::import::./shop/Widget.vue", "Conflict", 4),
		rebuildVueTestImport(file, "unresolved::import::./shop/Direct.vue", "Conflict", 4),
		rebuildVueTestImport(file, "external::./shop/Tail", "Tail", 5),
		rebuildVueTestImport(file, "external::./shop/Ambiguous", "Ambiguous", 6),
		rebuildVueTestImport(file, "unresolved::import::./shop/BareMiss", "BareMiss", 7),
		rebuildVueTestImport(file, "unresolved::import::./emit.js", "Emit", 8),
		rebuildVueTestImport(file, "ui/shop/NoSymbol.vue", "NoSymbol", 9),
		rebuildVueTestImport(file, "unresolved::import::", "Empty", 10),
		rebuildVueTestImport(file, "unresolved::import::./shop/Direct.vue", "Symbols", 11),
		rebuildVueTestImport(file, "external::./shop/Direct.vue", "RawSymbols", 12),
	}
	edges, want := slices.Clone(imports), slices.Clone(imports)
	for i, tc := range []struct {
		name, raw, id string
		bound         bool
	}{
		{"Panel", "MyPanel", "ui/shop/Widget.vue::default", true},
		{"Direct", "Direct", "ui/shop/Direct.vue::Direct", true},
		{"Missing", "Missing", "unresolved::Missing", false},
		{"Conflict", "Conflict", "unresolved::Conflict", false},
		{"Tail", "Tail", "ui/shop/Tail.vue::Tail", true},
		{"Ambiguous", "Ambiguous", "unresolved::Ambiguous", false},
		{"Local", "Local", "ui/local.ts::Local", false},
		{"BareMiss", "BareMiss", "ui/local.ts::BareMiss", false},
		{"Emit", "Emit", "ui/emit.ts::Emit", true},
		{"NoSymbol", "NoSymbol", "unresolved::NoSymbol", false},
		{"Empty", "Empty", "unresolved::Empty", false},
		{"Symbols", "RawSymbols", "unresolved::Symbols", false},
		{"Unknown", "Unknown", "unresolved::Unknown", false},
	} {
		e := rebuildVueTestRef(file, tc.name, tc.raw, i+20)
		edges, want = append(edges, e), append(want, rebuildVueTestSaved(e, tc.id, tc.bound))
	}
	want = rebuildVueSortedRows(want)
	require.Equal(t, rebuildVueSQLiteExpected(want), rebuildVueNativeRows(t, nodes, edges, nil))
	got, err := rebuildVueWorkingRows(t, nodes, edges)
	require.NoError(t, err)
	require.Equal(t, want, got)
}

func rebuildVueTestRender(file, target string, line int) *graph.Edge {
	e := typeTestEdge(file+"::Source", file, target, graph.EdgeRendersChild, "")
	e.Line = line
	return e
}

func TestRebuildVueRenderedNativeRows(t *testing.T) {
	const file = "ui/Render.tsx"
	nodes := []*graph.Node{typeTestNode(file+"::Source", "Source", graph.KindFunction, "typescript", file)}
	for _, d := range []struct{ file, name string }{
		{"ui/z.tsx", "Avatar"}, {"ui/z.ts", "Avatar"}, {"ui/a.tsx", "Avatar"},
		{"ui/local.tsx", "Ava"}, {"ui/local.tsx", "Miss"}, {"ui/local.tsx", "NoImport"},
		{"ui/a.tsx", "Miss"}, {"ui/barrel/index.jsx", "FromIndex"},
		{"ui/a.tsx", "Settled"}, {"ui/local.tsx", "Settled"},
	} {
		kind := graph.KindFunction
		if d.file == "ui/local.tsx" && (d.name == "Miss" || d.name == "Settled") {
			kind = graph.KindType
		}
		nodes = append(nodes, typeTestNode(d.file+"::"+d.name, d.name, kind, "typescript", d.file))
	}
	imports := []*graph.Edge{
		{From: file, FilePath: file, To: "unresolved::import::./a::Avatar", Kind: graph.EdgeImports, Alias: "Ava", Line: 4},
		{From: file, FilePath: file, To: "unresolved::import::./z::Avatar", Kind: graph.EdgeImports, Alias: "Ava", Line: 1},
		{From: file, FilePath: file, To: "unresolved::import::./missing::Miss", Kind: graph.EdgeImports, Line: 2},
		{From: file, FilePath: file, To: "unresolved::import::./a::Miss", Kind: graph.EdgeImports, Line: 5},
		{From: file, FilePath: file, To: "unresolved::import::./barrel::FromIndex", Kind: graph.EdgeImports, Line: 3},
		{From: file, FilePath: file, To: "ui/a.tsx", Kind: graph.EdgeImports, Alias: "Settled", Line: 6},
	}
	edges, want := slices.Clone(imports), slices.Clone(imports)
	for i, tc := range []struct {
		name, id string
		bound    bool
	}{
		{"Ava", "ui/z.tsx::Avatar", true},
		{"Miss", "ui/local.tsx::Miss", false},
		{"NoImport", "ui/local.tsx::NoImport", false},
		{"FromIndex", "ui/barrel/index.jsx::FromIndex", true},
		{"Settled", "ui/local.tsx::Settled", false},
		{"Unknown", "unresolved::Unknown", false},
	} {
		e := rebuildVueTestRender(file, tc.name, i+10)
		if tc.name == "FromIndex" {
			e.Meta = nil
		}
		edges, want = append(edges, e), append(want, rebuildVueTestSaved(e, tc.id, tc.bound))
	}
	want = rebuildVueSortedRows(want)
	t.Run("SQLite persistence loses aliases before resolution", func(t *testing.T) {
		// Native SQLite loses the import aliases before resolution. Ava then
		// takes the local fallback instead of either aliased Avatar import.
		sqliteWant := rebuildVueSQLiteExpected(want)
		for i, edge := range sqliteWant {
			if edge.Kind == graph.EdgeRendersChild && edge.Line == 10 {
				sqliteWant[i] = rebuildVueTestSaved(edges[len(imports)], "ui/local.tsx::Ava", false)
			}
		}
		require.Equal(t, rebuildVueSortedRows(sqliteWant), rebuildVueNativeRows(t, nodes, edges, nil))
	})

	// Also compare the native resolver on the full input payload that Pebble
	// can store. In-memory adjacency is insertion ordered; insert by the
	// SQLite adjacency order (line, insertion ID) for this fixture only.
	full := graph.New()
	ordered := slices.Clone(edges)
	slices.SortStableFunc(ordered, func(a, b *graph.Edge) int { return cmp.Compare(a.Line, b.Line) })
	full.AddBatch(nodes, ordered)
	require.Equal(t, want, rebuildVueResolveRows(t, full, nil))
	got, err := rebuildVueWorkingRows(t, nodes, edges)
	require.NoError(t, err)
	require.Equal(t, want, got)
}

func TestRebuildVueRenderedCanonicalTie(t *testing.T) {
	const file = "ui/Render.tsx"
	a, z := "ui/a.tsx::Button", "ui/z.tsx::Button"
	nodes := []*graph.Node{
		typeTestNode(file+"::Source", "Source", graph.KindFunction, "typescript", file),
		typeTestNode(a, "Button", graph.KindFunction, "typescript", "ui/a.tsx"),
		typeTestNode(z, "Button", graph.KindFunction, "typescript", "ui/z.tsx"),
	}
	ref := rebuildVueTestRender(file, "Button", 10)
	first := &graph.Edge{From: file, FilePath: file, To: "unresolved::import::./a::Button", Kind: graph.EdgeImports, Line: 1}
	last := &graph.Edge{From: file, FilePath: file, To: "unresolved::import::./z::Button", Kind: graph.EdgeImports, Line: 1}
	forward := []*graph.Edge{first, last, ref}
	reverse := []*graph.Edge{last, first, ref}
	canonical := rebuildVueSortedRows([]*graph.Edge{first, last, rebuildVueTestSaved(ref, a, true)})
	require.Equal(t, canonical, rebuildVueNativeRows(t, nodes, forward, nil))
	require.Equal(t, rebuildVueSortedRows([]*graph.Edge{first, last, rebuildVueTestSaved(ref, z, true)}),
		rebuildVueNativeRows(t, nodes, reverse, nil))
	for _, order := range [][]*graph.Edge{forward, reverse} {
		got, err := rebuildVueWorkingRows(t, nodes, order)
		require.NoError(t, err)
		require.Equal(t, canonical, got)
	}
}

func TestRebuildVueTemplateMissingAliasEvidence(t *testing.T) {
	const file = "ui/Shop.vue"
	const targetFile, targetID = "actual/Panel.vue", "actual/Panel.vue::Panel"
	nodes := []*graph.Node{
		typeTestNode(file+"::Source", "Source", graph.KindType, "vue", file),
		typeTestNode(targetFile, "Panel.vue", graph.KindFile, "vue", targetFile),
		typeTestNode(targetID, "Panel", graph.KindFunction, "vue", targetFile),
		typeTestNode("ui/decoy.ts::Panel", "Panel", graph.KindType, "typescript", "ui/decoy.ts"),
		typeTestNode("ui/local.ts::Local", "Local", graph.KindType, "typescript", "ui/local.ts"),
	}
	for _, spec := range []string{"@/Panel.vue", "~/Panel.vue", "acme-ui"} {
		t.Run(spec, func(t *testing.T) {
			imp := rebuildVueTestImport(file, "external::"+spec, "Panel", 1)
			ref := rebuildVueTestRef(file, "Panel", "Panel", 2)
			local := rebuildVueTestRef(file, "Local", "Local", 3)
			localResolved := rebuildVueTestSaved(local, "ui/local.ts::Local", false)
			edges := []*graph.Edge{imp, ref, local}
			wantRaw := rebuildVueSortedRows(edges)
			require.Equal(t, rebuildVueSQLiteExpected(rebuildVueSortedRows([]*graph.Edge{imp, ref, localResolved})),
				rebuildVueNativeRows(t, nodes, edges, nil))
			withAlias := rebuildVueNativeRows(t, nodes, edges, func(caller, path string) string {
				require.Equal(t, file, caller)
				require.Equal(t, spec, path)
				return targetFile
			})
			require.Equal(t, rebuildVueSQLiteExpected(rebuildVueSortedRows([]*graph.Edge{imp, rebuildVueTestSaved(ref, targetID, true), localResolved})), withAlias)
			got, err := rebuildVueWorkingRows(t, nodes, edges)
			require.ErrorIs(t, err, errRebuildMainRule)
			require.ErrorContains(t, err, "needs frozen path-alias evidence")
			require.Equal(t, wantRaw, got)
		})
	}
}
