package resolver

import (
	"fmt"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/graph/store_pebble"
)

func TestRebuildReexportsNativeEquality(t *testing.T) {
	const (
		caller      = "app/caller/main.ts"
		second      = "app/second/main.ts"
		barrelA     = "barrels/a/index.ts"
		barrelB     = "barrels/b/index.go"
		direct      = "values/direct/value.ts"
		outside     = "values/outside/value.ts"
		ordinaryHop = "values/ordinary-hop/value.ts"
		leaf        = "values/leaf/value.ts"
		fallback    = "app/fallback/file.ts"
	)
	var nodes []*graph.Node
	for _, file := range []string{caller, second, barrelA, barrelB, direct, outside, ordinaryHop, leaf, "bare.ts"} {
		nodes = append(nodes, &graph.Node{ID: file, Kind: graph.KindFile, FilePath: file, RepoPrefix: graph.RepoPrefixOfID(file)})
	}
	nodes = append(nodes, &graph.Node{ID: "app/foreign-owner.ts", Kind: graph.KindFile, FilePath: "app/foreign-owner.ts", RepoPrefix: "barrels"},
		&graph.Node{ID: "barrels/foreign-id.ts", Kind: graph.KindFile, FilePath: "barrels/foreign-id.ts", RepoPrefix: "app"},
		&graph.Node{ID: "app/source.ts::Call", Kind: graph.KindFunction, FilePath: caller, RepoPrefix: "barrels"},
		&graph.Node{ID: "barrels/source.ts::Call", Kind: graph.KindFunction, FilePath: caller, RepoPrefix: "app"},
		&graph.Node{ID: "app/pathless.ts::Call", Kind: graph.KindFunction},
		&graph.Node{ID: "values/unknown-placement.ts", Kind: graph.KindFunction})
	var edges []*graph.Edge
	add := func(from, to, file string, kind graph.EdgeKind) {
		edges = append(edges, &graph.Edge{From: from, To: to, FilePath: file, Kind: kind, Line: len(edges) + 1, Origin: graph.OriginASTResolved})
	}
	add("app/source.ts::Call", barrelA, "incorrect/provenance.ts", graph.EdgeImports)
	add(caller, direct, caller, graph.EdgeImports)
	add(second, barrelB, second, graph.EdgeImports)
	add("app/pathless.ts::Call", direct, fallback, graph.EdgeImports)
	add("barrels/source.ts::Call", outside, caller, graph.EdgeImports)
	add(barrelA, barrelB, "incorrect/provenance.ts", graph.EdgeReExports)
	add(barrelA, barrelB, barrelA, graph.EdgeReExports) // distinct rows, one hop
	add(barrelB, barrelA, barrelB, graph.EdgeReExports)
	add("barrels/b/missing.ts::Export", leaf, barrelB, graph.EdgeReExports)
	add(direct, ordinaryHop, direct, graph.EdgeImports)
	add(caller, "missing/target.ts", caller, graph.EdgeImports)
	add(caller, "values/unknown-placement.ts", caller, graph.EdgeImports)
	for i, id := range []string{"unresolved::name", "external::name", "app::stdlib::name", "dep::name"} {
		nodes = append(nodes, &graph.Node{ID: id, Kind: graph.KindFunction, FilePath: fmt.Sprintf("skipped/%d/symbol.ts", i)})
		add(caller, id, caller, graph.EdgeImports)
		add(barrelA, id, barrelA, graph.EdgeReExports)
	}

	for _, reverse := range []bool{false, true} {
		if reverse {
			slices.Reverse(nodes)
			slices.Reverse(edges)
		}
		for _, scope := range []struct {
			name  string
			repos map[string]struct{}
		}{
			{name: "full"},
			{name: "scoped", repos: map[string]struct{}{"app": {}}},
			{name: "empty", repos: map[string]struct{}{}},
		} {
			t.Run(fmt.Sprintf("%s/reversed=%t", scope.name, reverse), func(t *testing.T) {
				stats := assertRebuildReexportRows(t, nodes, edges, scope.repos, func(c *store_pebble.RebuildImportClosure) {
					for _, query := range []struct {
						file, dir string
						want      bool
					}{
						{caller, "values/direct", scope.name != "empty"},
						{caller, "values/leaf", scope.name != "empty"},
						{second, "barrels/a", scope.name != "empty"},
						{caller, "values/outside", scope.name == "full"},
						{caller, "values/ordinary-hop", false},
						{caller, "missing", false},
						{caller, "skipped/0", false},
						{fallback, "values/direct", scope.name != "empty"},
						{fallback, "app/fallback", false},
						{"bare.ts", ".", scope.name == "full"},
					} {
						got, err := c.Reachable(t.Context(), query.file, query.dir)
						if err != nil || got != query.want {
							t.Errorf("Reachable(%q, %q) = %t, %v; want %t", query.file, query.dir, got, err, query.want)
						}
					}
				})
				if scope.name == "empty" {
					if stats.Rounds != 0 || stats.Directories != 0 {
						t.Fatalf("empty scope processed a caller: %+v", stats)
					}
				} else if stats.Rounds != 3 || stats.Directories == 0 {
					t.Fatalf("expected a persisted closure and a three-hop frontier: %+v", stats)
				}
			})
		}
	}
}

func TestRebuildReexportsNativeEqualityAbove1MiB(t *testing.T) {
	const caller = "app/main.ts"
	const barrel = "barrels/index.ts"
	const leaves = 2600
	nodes := []*graph.Node{
		{ID: caller, Kind: graph.KindFile, FilePath: caller, RepoPrefix: "app"},
		{ID: barrel, Kind: graph.KindFile, FilePath: barrel, RepoPrefix: "barrels"},
	}
	edges := []*graph.Edge{{From: caller, To: barrel, Kind: graph.EdgeImports, FilePath: caller}}
	// Encoded re-export and placement keys alone exceed 1 MiB. Distinct
	// directories ensure no-op reduction cannot pass.
	for i := range leaves {
		file := fmt.Sprintf("barrels/%s/%04d/value.ts", strings.Repeat("p", 140), i)
		nodes = append(nodes, &graph.Node{ID: file, Kind: graph.KindFile, FilePath: file, RepoPrefix: "barrels"})
		edges = append(edges, &graph.Edge{From: barrel, To: file, Kind: graph.EdgeReExports, FilePath: barrel, Line: i + 1})
	}
	stats := assertRebuildReexportRows(t, nodes, edges, map[string]struct{}{"app": {}}, nil)
	if stats.ScratchBytes <= 1<<20 || stats.Rounds != 2 || stats.Directories != leaves+2 {
		t.Fatalf("expected more than 1 MiB of scratch writes and a two-round closure with %d rows: %+v", leaves+2, stats)
	}
	t.Logf("saved closure and logical scratch bytes: %+v", stats)
}

func assertRebuildReexportRows(t *testing.T, nodes []*graph.Node, edges []*graph.Edge, repos map[string]struct{}, check func(*store_pebble.RebuildImportClosure)) store_pebble.RebuildReexportStats {
	t.Helper()
	g := graph.New()
	for _, n := range nodes {
		g.AddNode(n)
	}
	for _, e := range edges {
		g.AddEdge(e)
	}
	want := New(g).buildImportClosureFiltered(repos)
	var keys [][2]string
	for file, dirs := range want {
		for dir := range dirs {
			keys = append(keys, [2]string{file, dir})
		}
	}
	slices.SortFunc(keys, func(a, b [2]string) int {
		if order := strings.Compare(a[0], b[0]); order != 0 {
			return order
		}
		return strings.Compare(a[1], b[1])
	})
	b, err := store_pebble.OpenBuild(filepath.Join(t.TempDir(), "build"), store_pebble.FullProfile)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := b.Close(); err != nil {
			t.Error(err)
		}
	})
	b.AddBatch(nodes, edges)
	var got [][2]string
	stats, err := withRebuildImportClosure(t.Context(), b.RebuildView(), repos, func(c *store_pebble.RebuildImportClosure) error {
		if check != nil {
			check(c)
		}
		return c.Visit(t.Context(), func(file, dir string) error {
			got = append(got, [2]string{file, dir})
			return nil
		})
	})
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(got, keys) {
		t.Fatalf("closure rows differ from native: disk has %d, native has %d; first mismatch %s", len(got), len(keys), firstReexportMismatch(got, keys))
	}
	if stats.Directories != len(got) {
		t.Fatalf("count disagrees with saved rows: %+v, got %d", stats, len(got))
	}
	return stats
}

func firstReexportMismatch(got, want [][2]string) string {
	for i := range min(len(got), len(want)) {
		if got[i] != want[i] {
			return fmt.Sprintf("%d: %q versus %q", i, got[i], want[i])
		}
	}
	return "row count only"
}
