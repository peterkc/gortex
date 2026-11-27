package resolver

import (
	"context"
	"errors"
	"fmt"
	"io"
	"maps"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/graph/store_pebble"
	"github.com/zzet/gortex/internal/graph/store_sqlite"
)

func TestRebuildRelativeImportsNativeRows(t *testing.T) {
	const pyrel = "unresolved::pyrel::"
	const py, dart, php, cpp = "py/main.py", "lib/app/main.dart", "php/main.php", "cpp/main.cc"
	nodes := []*graph.Node{
		rebuildImportFile(py, "python"), rebuildImportFile(dart, "dart"),
		rebuildImportFile(php, "php"), rebuildImportFile(cpp, "cpp"),
		rebuildImportFile("lua/app.lua", "lua"), rebuildImportFile("project/main.tscn", "godot_resource"),
	}
	for _, id := range []string{
		"py/mod.py", "py/mod/__init__.py", "py/pkg/__init__.py", "root.py",
		"lib/near.dart", "dart:core", "php/helpers/a.php", "helpers/a.php", "helpers/root.php",
		"x/vendor/one.php", "x/vendor/amb.php", "y/vendor/amb.php", "php/res:/picked.php",
		"cpp/local.h", "cpp/vector", "lua/util.lua", "project/scenes/next.tscn",
	} {
		nodes = append(nodes, rebuildImportFile(id, "text"))
	}
	tests := []struct {
		from, to, want, include string
	}{
		{py, pyrel + "py/mod", "py/mod.py", ""},
		{py, "external::py/pkg", "py/pkg/__init__.py", ""},
		{py, pyrel + "py/missing", "external::py/missing", ""},
		{py, pyrel + "root", "external::root", ""},
		{py, "external::py/../root", "", ""},
		{dart, pyrel + "py/mod", "external::py/mod", ""},
		{"not-a-file.py", pyrel + "py/mod", "", ""},
		{dart, "external::../near.dart", "lib/near.dart", ""},
		{dart, "external::../../../near.dart", "", ""},
		{dart, "external::dart:core", "", ""},
		{dart, "external::package:other/a.dart", "", ""},
		{php, rebuildImportStub + "/helpers/a.php", "php/helpers/a.php", ""},
		{php, rebuildImportStub + "./helpers/root", "helpers/root.php", ""},
		{php, rebuildImportStub + "vendor/one.php", "x/vendor/one.php", ""},
		{php, rebuildImportStub + "vendor/amb.php", "", ""},
		{php, rebuildImportStub + "Vendor/SomeNamespace", "", ""},
		// This is also a valid Godot candidate before relative apply.
		{php, rebuildImportStub + "res://picked.php", "php/res:/picked.php", ""},
		{cpp, rebuildImportStub + "local.h", "cpp/local.h", "quoted"},
		{cpp, rebuildImportStub + "vector", "", "system"},
		{cpp, rebuildImportStub + "missing.h", "", "not-an-include"},
		{"lua/app.lua", rebuildImportStub + "lua.util", "lua/util.lua", ""},
		{"project/main.tscn", rebuildImportStub + "res://scenes/next.tscn", "project/scenes/next.tscn", ""},
	}
	var edges, want []*graph.Edge
	for i, tc := range tests {
		e := &graph.Edge{From: tc.from, To: tc.to, FilePath: "recorded/elsewhere", Kind: graph.EdgeImports,
			Line: i + 1, Origin: graph.OriginASTInferred, Confidence: 0.5, ConfidenceLabel: "medium", Meta: map[string]any{"payload": tc.to}}
		if tc.include != "" {
			e.Meta["include_kind"] = tc.include
		}
		moved := *e
		if tc.want != "" {
			moved.To = tc.want
			if !strings.HasPrefix(tc.want, "external::") {
				moved.Origin = graph.OriginASTResolved
			}
		}
		edges = append(edges, e)
		want = append(want, &moved)
	}
	slices.SortFunc(want, compareRebuildImportEdges)
	native := rebuildImportsNative(t, nodes, edges, nil, nil)
	got, stats := rebuildImportsSaved(t, nodes, edges, 1, nil)
	require.Equal(t, want, native)
	require.Equal(t, native, got)
	require.Equal(t, len(nodes), stats.Files)
	require.Equal(t, 11, stats.RelativeMoves)
	require.Equal(t, 1, stats.LuaMoves)
	require.Equal(t, 1, stats.GodotMoves, "PHP must move its res:// edge before Godot collects")
	require.Positive(t, stats.SpillBytes)
}

// The base SQLite import scan has no ORDER BY. Both source shapes resolve to
// one fresh identity, so forcing the scan order changes the saved payload.
func TestRebuildRelativeImportsCollisionOrder(t *testing.T) {
	caller := strings.Repeat(strings.Repeat("d", 170)+"/", 5) + "main.py"
	nodes := []*graph.Node{rebuildImportFile(caller, "python"), rebuildImportFile("pkg/mod.py", "python")}
	a := &graph.Edge{From: caller, FilePath: caller, Kind: graph.EdgeImports, Line: 1, To: "external::pkg/mod", Meta: map[string]any{"payload": "external"}}
	b := *a
	b.To, b.Meta = "unresolved::pyrel::pkg/mod", map[string]any{"payload": "pyrel"}
	edges := []*graph.Edge{&b, a}
	// Relative staging, not the two-file index, must exceed the 1 MiB threshold.
	for i := range 680 {
		e := *a
		e.Line = i + 2
		edges = append(edges, &e)
	}
	order := make(map[graph.EdgeIdentity]int, len(edges))
	complete := slices.Clone(edges)
	slices.SortFunc(complete, compareRebuildImportEdges)
	for i, e := range complete {
		order[graph.EdgeIdentityFor(e)] = i
	}
	aKey, bKey := graph.EdgeIdentityFor(a), graph.EdgeIdentityFor(&b)
	canonical := rebuildImportsNative(t, nodes, edges, order, nil)
	order[aKey], order[bKey] = order[bKey], order[aKey]
	reversed := rebuildImportsNative(t, nodes, edges, order, nil)
	require.Len(t, canonical, 681)
	require.Len(t, reversed, 681)
	winner := *a
	winner.To, winner.Origin = "pkg/mod.py", graph.OriginASTResolved
	require.Equal(t, &winner, canonical[0])
	winner.Meta = b.Meta
	require.Equal(t, &winner, reversed[0])
	require.Equal(t, canonical[1:], reversed[1:])
	t.Logf("forced SQLite winners: complete old key %v; pyrel first %v", canonical[0].Meta, reversed[0].Meta)
	for _, reverseArrival := range []bool{false, true} {
		if reverseArrival {
			slices.Reverse(edges)
		}
		got, stats := rebuildImportsSaved(t, nodes, edges, 1<<20, nil)
		require.Equal(t, canonical, got)
		require.Equal(t, len(edges), stats.RelativeMoves)
		require.Positive(t, stats.Flushes)
		require.Positive(t, stats.SpillBytes)
	}
}

func TestRebuildRelativeImportsRefusesMissingCppOrder(t *testing.T) {
	py := &graph.Edge{From: "a/main.py", FilePath: "a/main.py", Kind: graph.EdgeImports, To: "unresolved::pyrel::a/mod"}
	cpp := rebuildImportEdge("z/main.cc", "nested/header.h", graph.EdgeImports, 1)
	cpp.Meta = map[string]any{"include_kind": "quoted"}
	nodes := []*graph.Node{rebuildImportFile(py.From, "python"), rebuildImportFile("a/mod.py", "python"),
		rebuildImportFile(cpp.From, "cpp"), rebuildImportFile("include/nested/header.h", "cpp")}
	edges := []*graph.Edge{py, cpp}
	g, err := store_sqlite.Open(filepath.Join(t.TempDir(), "native.sqlite"))
	require.NoError(t, err)
	defer func() { require.NoError(t, g.Close()) }()
	require.NoError(t, g.AddBatchChecked(nodes, edges))
	// Native has no frozen-input failure mode. Its saved rows before this
	// pass are the oracle for a refusal, not its result after a partial run.
	original := slices.Collect(g.EdgesByKind(graph.EdgeImports))
	slices.SortFunc(original, compareRebuildImportEdges)
	malformed := errors.New("malformed include path record")
	for _, tc := range []struct {
		name  string
		paths RebuildCppIncludePaths
		want  error
	}{
		{"absent capture", nil, ErrRebuildCppIncludeInput},
		{"malformed", func(context.Context, string, func(string) (bool, error)) (bool, error) {
			return false, malformed
		}, malformed},
		{"read error after hit", func(_ context.Context, _ string, visit func(string) (bool, error)) (bool, error) {
			stop, err := visit("include")
			if err != nil {
				return false, err
			}
			if !stop {
				return false, errors.New("expected ordered hit before read error")
			}
			return true, io.ErrUnexpectedEOF
		}, io.ErrUnexpectedEOF},
	} {
		t.Run(tc.name, func(t *testing.T) {
			build, err := store_pebble.OpenBuild(filepath.Join(t.TempDir(), "build"), store_pebble.FullProfile)
			require.NoError(t, err)
			defer func() { require.NoError(t, build.Close()) }()
			build.AddBatch(nodes, edges)
			// The Python move precedes the C lookup in complete-key order.
			stats, err := RunRebuildFileImports(t.Context(), build, 1, tc.paths)
			require.ErrorIs(t, err, store_pebble.ErrRebuildFileImports)
			require.ErrorIs(t, err, tc.want)
			require.Zero(t, stats)
			var saved []*graph.Edge
			require.NoError(t, build.RebuildView().VisitEdges(t.Context(), func(e *graph.Edge) error {
				saved = append(saved, e)
				return nil
			}))
			require.Equal(t, original, saved)
		})
	}
}

// Drive the relative pass directly: these are its input rows, not predictions
// about which C includes the preceding main resolver leaves pending.
func TestRebuildCppIncludesNativeRows(t *testing.T) {
	const tu, empty, header = "work/main.cc", "work/empty.c", "work/header.h"
	nodes := []*graph.Node{rebuildImportFile(tu, "cpp"), rebuildImportFile(empty, "c"), rebuildImportFile(header, "objc")}
	for _, id := range []string{
		"z/dup.h", "a/dup.h", "work/local.h", "z/local.h", "work/vector", "z/vector",
		"z/v.1/item.h", "z/v.1/item.hpp", "z/v.1/item.hh", "a/v.1/item", "z/raw", "z/raw.h",
		"a/only.hpp", "a/only.hh", "z/later.hh", "z/later.hxx", "z/last.hxx", "z/already.ext.h", "z/upper.H",
		"tree/api/one.h", "tree/xapi/one.h", "tree/API/one.h", "tree/api/two.h", "other/api/two.h",
	} {
		nodes = append(nodes, rebuildImportFile(id, "text"))
	}
	tus := map[string][]string{tu: {"z", "a"}, empty: nil}
	for _, via := range []string{"compile_db", "heuristic"} {
		t.Run(via, func(t *testing.T) {
			// Both missing and empty TU lists use the sorted union (a, z).
			// The heuristic list, like main.cc's own -I list, starts with z.
			unionFirst := "a"
			if via == "heuristic" {
				unionFirst = "z"
			}
			configure := func(r *Resolver) {
				if via == "compile_db" {
					r.SetCppIncludeDirs(tus)
				} else {
					r.SetCppFallbackIncludeDirs([]string{"z", "a"})
				}
			}
			paths := func(_ context.Context, file string, visit func(string) (bool, error)) (bool, error) {
				dirs := []string{"z", "a"}
				if via == "compile_db" && file != tu {
					dirs = []string{"a", "z"}
				}
				for _, dir := range dirs {
					if stop, err := visit(dir); stop || err != nil {
						return via == "compile_db", err
					}
				}
				return via == "compile_db", nil
			}
			var edges, want []*graph.Edge
			for i, tc := range []struct{ from, kind, spec, target, dir string }{
				{tu, "quoted", "dup.h", "z/dup.h", "z"},
				{empty, "quoted", "dup.h", unionFirst + "/dup.h", unionFirst},
				{header, "system", "dup.h", unionFirst + "/dup.h", unionFirst},
				{tu, "quoted", "local.h", "work/local.h", ""},
				{tu, "system", "vector", "", ""},
				{tu, "system", "v.1/item", "z/v.1/item.h", "z"},
				{tu, "quoted", "raw", "z/raw", "z"},
				{tu, "quoted", "only", "a/only.hpp", "a"},
				{tu, "quoted", "later", "z/later.hh", "z"},
				{tu, "quoted", "last", "z/last.hxx", "z"},
				{tu, "quoted", "already.ext", "", ""},
				{tu, "quoted", "upper.h", "", ""},
				{tu, "quoted", "api/one.h", "tree/api/one.h", ""},
				{tu, "quoted", "api/two.h", "", ""},
				{tu, "quoted", "one.h", "", ""},
			} {
				e := rebuildImportEdge(tc.from, tc.spec, graph.EdgeImports, i+1)
				e.FilePath, e.Origin, e.Confidence, e.ConfidenceLabel = "recorded/elsewhere", graph.OriginASTInferred, 0.5, "medium"
				e.Meta = map[string]any{"include_kind": tc.kind, "payload": tc.spec}
				moved := *e
				if tc.target != "" {
					moved.To, moved.Origin = tc.target, graph.OriginASTResolved
				}
				if tc.dir != "" {
					moved.Meta = maps.Clone(e.Meta)
					moved.Meta["include_dir"], moved.Meta["resolved_via"] = tc.dir, via
				}
				edges, want = append(edges, e), append(want, &moved)
			}
			slices.SortFunc(want, compareRebuildImportEdges)
			native := rebuildImportsNative(t, nodes, edges, nil, configure)
			got, _ := rebuildImportsSaved(t, nodes, edges, 1, paths)
			require.Equal(t, want, native)
			require.Equal(t, native, got)
		})
	}
}

func TestRebuildCppIncludesCollisionOrder(t *testing.T) {
	const caller = "src/main.c"
	nodes := []*graph.Node{rebuildImportFile(caller, "c"), rebuildImportFile("inc/x.h", "text")}
	a := rebuildImportEdge(caller, "x", graph.EdgeImports, 7)
	b := rebuildImportEdge(caller, "x.h", graph.EdgeImports, 7)
	a.Meta = map[string]any{"include_kind": "quoted", "payload": "extension"}
	b.Meta = map[string]any{"include_kind": "system", "payload": "exact"}
	edges := []*graph.Edge{b, a}
	configure := func(r *Resolver) { r.SetCppIncludeDirs(map[string][]string{caller: {"inc"}}) }
	ak, bk := graph.EdgeIdentityFor(a), graph.EdgeIdentityFor(b)
	first := rebuildImportsNative(t, nodes, edges, map[graph.EdgeIdentity]int{ak: 0, bk: 1}, configure)
	last := rebuildImportsNative(t, nodes, edges, map[graph.EdgeIdentity]int{ak: 1, bk: 0}, configure)
	require.Len(t, first, 1)
	require.Len(t, last, 1)
	require.Equal(t, "extension", first[0].Meta["payload"])
	require.Equal(t, "exact", last[0].Meta["payload"])
	paths := func(_ context.Context, _ string, visit func(string) (bool, error)) (bool, error) {
		_, err := visit("inc")
		return true, err
	}
	for range 2 {
		got, _ := rebuildImportsSaved(t, nodes, edges, 1, paths)
		require.Equal(t, first, got, "complete old-key order on both arrivals")
		slices.Reverse(edges)
	}
}

func TestRebuildRelativeImportDowngradeSeals(t *testing.T) {
	for _, incumbent := range []bool{false, true} {
		t.Run(fmt.Sprint(incumbent), func(t *testing.T) {
			ctx, b, _ := frontierFixture(t, 0)
			const file = "py/main.py"
			node := rebuildImportFile(file, "python")
			raw := &graph.Edge{From: file, To: "unresolved::pyrel::py/missing", Kind: graph.EdgeImports, FilePath: file, Line: 1, Origin: graph.OriginASTInferred}
			want := *raw
			want.To = "external::py/missing"
			edges := []*graph.Edge{raw}
			if incumbent {
				edges = append(edges, &want)
			}
			require.Equal(t, []*graph.Edge{&want}, rebuildImportsNative(t, []*graph.Node{node}, edges, nil, nil))
			require.NoError(t, b.AddParsedFile("", store_pebble.ParseFacts{
				EffectiveLanguage: "python",
				File:              graph.FileMetaRow{FilePath: file, NodeCount: 1},
				Nodes:             []*graph.Node{node},
				Edges:             edges,
			}))
			manifest, err := store_pebble.PrepareManifest(ctx, t.TempDir(), []store_pebble.ManifestFile{{Path: file, Kind: 'f', Language: "python"}}, store_pebble.Witness{
				Profile: store_pebble.FullProfile, Stage: store_pebble.StageManifest, Input: store_pebble.HeadDigest{1}, Recipe: store_pebble.HeadDigest{2},
			}, "relative-import")
			require.NoError(t, err)
			fileGraph, err := b.PrepareFileGraph(ctx, t.TempDir(), manifest.Witness, manifest.Incarnation)
			require.NoError(t, err)
			base, err := b.RetainFileGraph(ctx, fileGraph)
			require.NoError(t, err)
			stats, err := RunRebuildFileImports(ctx, b, 1, nil)
			require.NoError(t, err)
			require.Equal(t, 1, stats.RelativeMoves)
			require.Equal(t, []*graph.Edge{&want}, rebuildFnPointerSaved(t, b))
			stage, err := b.PrepareRelations(ctx, t.TempDir(), base, nil)
			if incumbent {
				require.ErrorAs(t, err, new(*store_pebble.RebuildUnknownProvenanceError))
				require.Empty(t, stage)
				return
			}
			require.NoError(t, err)
			require.Equal(t, store_pebble.StageRelations, stage.Witness.Stage)
		})
	}
}
