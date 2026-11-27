package resolver

import (
	"cmp"
	"fmt"
	"iter"
	"path"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/graph/store_pebble"
	"github.com/zzet/gortex/internal/graph/store_sqlite"
)

const rebuildImportStub = "unresolved::import::"

func TestRebuildFileImportsNativeRows(t *testing.T) {
	const caller = "src/boot.luau"
	files := []struct{ id, lang string }{
		{caller, "luau"}, {"src/init.luau", "luau"}, {"weird.lua", "Lua"},
		{"net/main.luau", "luau"}, {"vendor/net/main.lua", "lua"}, {"vendor/base/util.lua", "lua"},
		{"x/base/amb.lua", "lua"}, {"y/base/amb.lua", "lua"},
		{"src/Pkg.luau", "luau"}, {"vendor/Pkg.lua", "lua"}, {"vendor/Solo.lua", "lua"},
		{"src/Both.lua", "lua"}, {"src/Both.luau", "luau"}, {"src/local/init.luau", "luau"},
		{"scripts/player.gd", "text"}, {"other/scripts/player.gd", "text"}, {"game/ui/hud.gd", "text"},
		{"game/scenes/menu.tscn", "text"}, {"tiles.png", "image"},
	}
	var nodes []*graph.Node
	for _, f := range files {
		nodes = append(nodes, rebuildImportFile(f.id, f.lang))
	}
	// Godot's named-language gate spans kinds. The source and target of a
	// res:// edge need not themselves have a Godot language.
	nodes = append(nodes, &graph.Node{ID: caller + "::loader", Name: "loader", Kind: graph.KindFunction, Language: "gdscript", FilePath: caller})
	tests := []struct {
		from, spec, want string
		kind             graph.EdgeKind
		roblox           bool
	}{
		{caller, "net.main", "net/main.luau", graph.EdgeImports, false},
		{caller, "base.util", "vendor/base/util.lua", graph.EdgeImports, false},
		{caller, "base.amb", "", graph.EdgeImports, false},
		{caller, "Pkg", "", graph.EdgeImports, true},
		{caller, "./Pkg", "src/Pkg.luau", graph.EdgeImports, false},
		{caller, "Solo", "vendor/Solo.lua", graph.EdgeImports, true},
		{caller, "./Solo", "", graph.EdgeImports, false},
		{caller, "./Both", "", graph.EdgeImports, false},
		{"src/init.luau", "@self/local", "src/local/init.luau", graph.EdgeImports, false},
		{caller, "@alias/Pkg", "", graph.EdgeImports, false},
		{"weird.lua", "net.main", "", graph.EdgeImports, false},
		{caller + "::loader", "net.main", "", graph.EdgeImports, false},
		{caller, "net.absent", "", graph.EdgeImports, false},
		{caller, "res://ui/hud.gd", "game/ui/hud.gd", graph.EdgeReferences, false},
		{caller, "res:///scenes/menu.tscn/", "game/scenes/menu.tscn", graph.EdgeImports, false},
		{caller, "res://tiles.png", "tiles.png", graph.EdgeReferences, false},
		{caller, "res://scripts/player.gd", "", graph.EdgeReferences, false},
		{caller, "res://assets/missing.png", "", graph.EdgeImports, false},
		{caller, "res://ui/hud.gd", "", graph.EdgeCalls, false},
	}
	var edges, want []*graph.Edge
	for i, tc := range tests {
		e := rebuildImportEdge(tc.from, tc.spec, tc.kind, i+1)
		e.FilePath = caller // caller-language checks use ID, not recording path
		if tc.roblox {
			e.Meta = map[string]any{"roblox_path": "script.Parent." + tc.spec}
		}
		resolved := *e
		if tc.want != "" {
			resolved.To, resolved.Origin = tc.want, graph.OriginASTResolved
		}
		edges = append(edges, e)
		want = append(want, &resolved)
	}
	// A pre-existing final identity must retain its own payload, not the mover's.
	stationary := *want[0]
	stationary.Origin = edges[0].Origin
	stationary.Meta = map[string]any{"payload": "stationary"}
	edges = append(edges, &stationary)
	want[0] = &stationary
	slices.SortFunc(want, compareRebuildImportEdges)
	for _, reverse := range []bool{false, true} {
		t.Run(fmt.Sprint(reverse), func(t *testing.T) {
			if reverse {
				slices.Reverse(nodes)
				slices.Reverse(edges)
			}
			native := rebuildImportsNative(t, nodes, edges, nil, nil)
			got, stats := rebuildImportsSaved(t, nodes, edges, 1, nil)
			require.Equal(t, want, native)
			require.Equal(t, native, got)
			require.Equal(t, 5, stats.LuaMoves)
			require.Equal(t, 3, stats.GodotMoves)
			require.Equal(t, len(files), stats.Files)
			// At one byte every move spills as a complete one-row run.
			require.Equal(t, stats.LuaMoves+stats.GodotMoves, stats.Flushes)
			require.Positive(t, stats.SpillBytes)

			got, stats = rebuildImportsSaved(t, nodes, edges, 1<<20, nil)
			require.Equal(t, native, got)
			require.Equal(t, 5, stats.LuaMoves)
			require.Equal(t, 3, stats.GodotMoves)
			require.Equal(t, len(files), stats.Files)
			require.Zero(t, stats.Flushes)
			require.Zero(t, stats.SpillBytes)
		})
	}
}

func TestRebuildFileImportsUnnamedGodotLanguage(t *testing.T) {
	scene := rebuildImportFile("main.tscn", "godot_resource")
	scene.Name = ""
	nodes := []*graph.Node{scene, rebuildImportFile("asset.png", "image")}
	edges := []*graph.Edge{rebuildImportEdge(scene.ID, "res://asset.png", graph.EdgeReferences, 1)}
	native := rebuildImportsNative(t, nodes, edges, nil, nil)
	got, stats := rebuildImportsSaved(t, nodes, edges, 1<<20, nil)
	require.Equal(t, edges, native)
	require.Equal(t, native, got)
	require.Zero(t, stats.GodotMoves)
}

// Force only the kind-scan order; keep the pinned reducer and SQLite reindex
// unchanged. The actual query has no ORDER BY. A and B share the complete
// (from, kind, to, file, line) identity after A's move, but B is also moving.
func TestRebuildFileImportsCollisionOrder(t *testing.T) {
	caller := strings.Repeat("w", 220) + "/boot.lua"
	const mid = rebuildImportStub + "zleaf.lua"
	nodes := []*graph.Node{rebuildImportFile(caller, "lua")}
	for _, id := range []string{mid, "zleaf/lua.lua", "v.lua", "zz.lua"} {
		nodes = append(nodes, rebuildImportFile(id, "lua"))
	}
	a := rebuildImportEdge(caller, rebuildImportStub+"zleaf", graph.EdgeImports, 1)
	a.Meta = map[string]any{"payload": "from-a"}
	b := rebuildImportEdge(caller, "zleaf.lua", graph.EdgeImports, 1)
	b.Meta = map[string]any{"payload": "from-b"}
	forward := []*graph.Edge{a}
	for i := range 4998 {
		forward = append(forward, rebuildImportEdge(caller, "v", graph.EdgeImports, i+2))
	}
	forward = append(forward, rebuildImportEdge(caller, "zz", graph.EdgeImports, 5000), b)
	reverse := slices.Clone(forward)
	slices.Reverse(reverse)
	positions := func(rows []*graph.Edge) map[graph.EdgeIdentity]int {
		m := make(map[graph.EdgeIdentity]int, len(rows))
		for i, e := range rows {
			m[graph.EdgeIdentityFor(e)] = i
		}
		return m
	}
	lost := rebuildImportsNative(t, nodes, forward, positions(forward), nil)
	kept := rebuildImportsNative(t, nodes, reverse, positions(reverse), nil)
	wantKey := func(to string) graph.EdgeIdentity {
		e := *b
		e.To = to
		return graph.EdgeIdentityFor(&e)
	}
	lookup := func(rows []*graph.Edge, id graph.EdgeIdentity) *graph.Edge {
		for _, e := range rows {
			if graph.EdgeIdentityFor(e) == id {
				return e
			}
		}
		return nil
	}
	require.Len(t, lost, 5000)
	require.Len(t, kept, 5001)
	require.Nil(t, lookup(lost, wantKey(mid)))
	keptA := lookup(kept, wantKey(mid))
	require.NotNil(t, keptA)
	require.Equal(t, a.Meta, keptA.Meta)
	withoutA := slices.DeleteFunc(slices.Clone(kept), func(e *graph.Edge) bool {
		return graph.EdgeIdentityFor(e) == wantKey(mid)
	})
	require.Equal(t, withoutA, lost, "the forced orders differ only by A's saved row")
	for _, rows := range [][]*graph.Edge{lost, kept} {
		keptB := lookup(rows, wantKey("zleaf/lua.lua"))
		require.NotNil(t, keptB)
		require.Equal(t, b.Meta, keptB.Meta)
	}
	t.Logf("forced A-first SQLite: %d rows, A absent; forced B-first: %d rows, A=%+v; B saved in both", len(lost), len(kept), keptA)

	// Complete old-key order keeps A and B in one native window (B is move
	// 5,000). A 256-row writer would incorrectly consider B stationary.
	complete := slices.Clone(forward)
	slices.SortFunc(complete, compareRebuildImportEdges)
	require.Same(t, b, complete[4999])
	want := rebuildImportsNative(t, nodes, reverse, positions(complete), nil)
	require.Equal(t, kept, want)
	for _, rows := range [][]*graph.Edge{forward, reverse} {
		got, stats := rebuildImportsSaved(t, nodes, rows, 1<<20, nil)
		require.Equal(t, want, got)
		require.Equal(t, 5001, stats.LuaMoves)
		require.Positive(t, stats.Flushes)
		require.Positive(t, stats.SpillBytes)
	}
}

func rebuildImportsNative(t *testing.T, nodes []*graph.Node, edges []*graph.Edge, order map[graph.EdgeIdentity]int, configure func(*Resolver)) []*graph.Edge {
	t.Helper()
	g, err := store_sqlite.Open(filepath.Join(t.TempDir(), "native.sqlite"))
	require.NoError(t, err)
	defer func() { require.NoError(t, g.Close()) }()
	require.NoError(t, g.AddBatchChecked(nodes, edges))
	var input graph.Store = g
	if order != nil {
		input = &rebuildImportOrderedSQLite{Store: g, order: order}
	}
	r := New(input)
	if configure != nil {
		configure(r)
	}
	r.resolveRelativeImports()
	r.resolveLuaRequires()
	r.resolveGodotResPaths()
	var rows []*graph.Edge
	for _, kind := range []graph.EdgeKind{graph.EdgeImports, graph.EdgeReferences, graph.EdgeCalls} {
		for e := range g.EdgesByKind(kind) {
			rows = append(rows, e)
		}
	}
	slices.SortFunc(rows, compareRebuildImportEdges)
	return rows
}

type rebuildImportOrderedSQLite struct {
	*store_sqlite.Store
	order map[graph.EdgeIdentity]int
}

func (g *rebuildImportOrderedSQLite) EdgesByKind(kind graph.EdgeKind) iter.Seq[*graph.Edge] {
	return func(yield func(*graph.Edge) bool) {
		rows := slices.Collect(g.Store.EdgesByKind(kind))
		slices.SortFunc(rows, func(a, b *graph.Edge) int {
			return cmp.Compare(g.order[graph.EdgeIdentityFor(a)], g.order[graph.EdgeIdentityFor(b)])
		})
		for _, e := range rows {
			if !yield(e) {
				return
			}
		}
	}
}

func rebuildImportsSaved(t *testing.T, nodes []*graph.Node, edges []*graph.Edge, spillAt int, cpp RebuildCppIncludePaths) ([]*graph.Edge, store_pebble.RebuildFileImportStats) {
	t.Helper()
	b, err := store_pebble.OpenBuild(filepath.Join(t.TempDir(), "build"), store_pebble.FullProfile)
	require.NoError(t, err)
	defer func() { require.NoError(t, b.Close()) }()
	b.AddBatch(nodes, edges)
	stats, err := RunRebuildFileImports(t.Context(), b, spillAt, cpp)
	require.NoError(t, err)
	var rows []*graph.Edge
	require.NoError(t, b.RebuildView().VisitEdges(t.Context(), func(e *graph.Edge) error {
		rows = append(rows, e)
		return nil
	}))
	return rows, stats
}

func rebuildImportFile(id, lang string) *graph.Node {
	return &graph.Node{ID: id, Name: path.Base(id), FilePath: id, Kind: graph.KindFile, Language: lang}
}

func rebuildImportEdge(from, spec string, kind graph.EdgeKind, line int) *graph.Edge {
	return &graph.Edge{From: from, To: rebuildImportStub + spec, FilePath: from, Kind: kind, Line: line}
}

func compareRebuildImportEdges(a, b *graph.Edge) int {
	return cmp.Or(cmp.Compare(a.From, b.From), cmp.Compare(a.Kind, b.Kind), cmp.Compare(a.To, b.To), cmp.Compare(a.FilePath, b.FilePath), cmp.Compare(a.Line, b.Line))
}
