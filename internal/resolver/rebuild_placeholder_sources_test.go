package resolver

import (
	"fmt"
	"path/filepath"
	"slices"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/graph/store_pebble"
	"github.com/zzet/gortex/internal/graph/store_sqlite"
)

const testBareSource = "unresolved::*.field"

func placeholderEdge(from, to string, kind graph.EdgeKind) *graph.Edge {
	return &graph.Edge{From: from, To: to, Kind: kind, FilePath: "repo/use.go", Line: 7,
		Meta: map[string]any{"payload": from + " -> " + to}}
}

func placeholderRetarget(old *graph.Edge, to string, kind graph.EdgeKind) graph.EdgeReindex {
	e := *old
	e.To, e.Kind = to, kind
	return graph.EdgeReindex{Edge: &e, OldTo: old.To, OldKind: old.Kind}
}

type placeholderFixture struct {
	edges   []*graph.Edge
	pages   [][]graph.EdgeReindex
	after   func(graph.Store) // immediately after the first page's deferred record
	reverse bool              // force the opposite native light-census order
}

func (f placeholderFixture) native(t *testing.T) ([]*graph.Edge, int) {
	t.Helper()
	g, err := store_sqlite.Open(filepath.Join(t.TempDir(), "native.sqlite"))
	require.NoError(t, err)
	defer func() { require.NoError(t, g.Close()) }()
	require.NoError(t, g.AddBatchChecked(nil, f.edges))
	var idx placeholderSourceIndex
	for i, page := range f.pages {
		g.ReindexEdges(page)
		idx.deferRepoints(g, page)
		if i == 0 {
			// Lock only the otherwise unordered light-census scan, not the
			// repoint sequence, retarget traversal, lookup or reindex kernel.
			for _, ids := range idx.bySite {
				slices.SortFunc(ids, rebuildFnPointerCompare)
				if f.reverse {
					slices.Reverse(ids)
				}
			}
			if f.after != nil {
				f.after(g)
			}
		}
	}
	moved := idx.applyDeferred(g)
	rows := g.AllEdges()
	slices.SortFunc(rows, compareRebuildImportEdges)
	return rows, moved
}

func (f placeholderFixture) private(t *testing.T, spillAt int) ([]*graph.Edge, store_pebble.RebuildPlaceholderStats) {
	t.Helper()
	ctx := t.Context()
	b := rebuildFnPointerBuild(t)
	b.AddBatch(nil, f.edges)
	var p *store_pebble.RebuildPlaceholderSources
	for i, page := range f.pages {
		b.ReindexEdges(page)
		if i == 0 {
			var err error
			p, err = b.CapturePlaceholderSources(ctx, spillAt)
			require.NoError(t, err)
			defer func() { require.NoError(t, p.Close()) }()
		}
		require.NoError(t, p.Record(ctx, page))
		if i == 0 && f.after != nil {
			f.after(b)
		}
	}
	stats, err := p.Apply(ctx)
	require.NoError(t, err)
	return rebuildFnPointerSaved(t, b), stats
}

func TestRebuildPlaceholderSourcesPages(t *testing.T) {
	t.Setenv("GORTEX_RESOLVE_FROM_RECONCILE", "1")
	const qualified = "repo/" + testBareSource
	first := placeholderEdge("repo/first", testBareSource, graph.EdgeReferences)
	second := placeholderEdge("repo/second", testBareSource, graph.EdgeReferences)
	promoted := placeholderEdge(qualified, "before", graph.EdgeReferences)
	one := placeholderRetarget(promoted, "one", graph.EdgeArgOf)
	two := placeholderRetarget(one.Edge, "two", graph.EdgeValueFlow)
	three := placeholderRetarget(two.Edge, "three", graph.EdgeArgOf)
	three.Edge.Meta = map[string]any{"saved": "latest payload"}
	bare := placeholderEdge(testBareSource, "unchanged-callee", graph.EdgeArgOf)
	// Both this census identity and the promotion converge on the same
	// current row. One exact-refetch window must move it only once.
	converged := placeholderEdge(qualified, "two", graph.EdgeValueFlow)
	lost := placeholderEdge(qualified, "gone", graph.EdgeArgOf)
	late := placeholderEdge(qualified, "late", graph.EdgeArgOf)
	otherFile := placeholderEdge(qualified, "other-file", graph.EdgeArgOf)
	otherFile.FilePath = "repo/other.go"
	otherLine := placeholderEdge(qualified, "other-line", graph.EdgeValueFlow)
	otherLine.Line++
	f := placeholderFixture{
		edges: []*graph.Edge{first, second, promoted, bare, converged, lost, otherFile, otherLine},
		pages: [][]graph.EdgeReindex{
			{one, placeholderRetarget(first, "winner", first.Kind)},
			{two, placeholderRetarget(second, "later", second.Kind)},
			{three},
		},
		after: func(g graph.Store) {
			require.True(t, g.RemoveEdge(lost.From, lost.To, lost.Kind))
			g.AddBatch(nil, []*graph.Edge{late})
		},
	}
	want, nativeMoved := f.native(t)
	got, stats := f.private(t, 1<<20)
	require.Len(t, got, 7)
	require.Equal(t, want, got)
	require.Equal(t, 2, nativeMoved)
	require.Equal(t, nativeMoved, stats.Reindexes)
	require.Equal(t, 6, stats.Census, "includes the promotion in the first commit, excludes the late row")
	require.Equal(t, 4, stats.Claimed, "includes the evicted and converged identities")
	require.Equal(t, 3, stats.Links)
	for _, e := range []*graph.Edge{otherFile, otherLine, late} {
		require.Contains(t, got, e)
	}
	for _, e := range []*graph.Edge{three.Edge, bare} {
		moved := *e
		moved.From = "winner"
		require.Contains(t, got, &moved)
	}
}

func TestRebuildPlaceholderSourcesWindowCollision(t *testing.T) {
	t.Setenv("GORTEX_RESOLVE_FROM_RECONCILE", "1")
	// The first reference claims only bare sources. Its destination happens
	// to be the qualified marker: native IsUnresolvedTarget accepts this as
	// a bound target. The second reference then claims the qualified rows.
	const qualified = "repo/" + testBareSource
	first := placeholderEdge("first", testBareSource, graph.EdgeReferences)
	second := placeholderEdge("repo/second", testBareSource, graph.EdgeReferences)
	a := placeholderEdge(testBareSource, "match", graph.EdgeArgOf)
	b := placeholderEdge(qualified, "match", graph.EdgeArgOf)
	z := placeholderEdge(qualified, "z", graph.EdgeArgOf)
	f := placeholderFixture{
		edges: []*graph.Edge{first, second, a, b, z},
		pages: [][]graph.EdgeReindex{
			{placeholderRetarget(first, qualified, first.Kind)},
			{placeholderRetarget(second, "final", second.Kind)},
		},
	}
	for i := range 254 {
		f.edges = append(f.edges, placeholderEdge(testBareSource, fmt.Sprintf("pad%03d", i), graph.EdgeArgOf))
	}
	// A miss still occupies its refetch slot. Compacting successful rows
	// before the window cut would put both qualified moves in window one.
	f.after = func(g graph.Store) { require.True(t, g.RemoveEdge(testBareSource, "pad000", graph.EdgeArgOf)) }
	kept, moves := f.native(t)
	f.reverse = true
	lost, reverseMoves := f.native(t)
	require.Equal(t, 256, moves)
	require.Equal(t, moves, reverseMoves)
	movedA := *a
	movedA.From = qualified
	require.Len(t, kept, 258)
	require.Contains(t, kept, &movedA, "matching qualified sibling moves in window one")
	require.Len(t, lost, 257)
	require.NotContains(t, lost, &movedA, "stationary destination blocks A until window two")
	movedB := *b
	movedB.From = "final"
	require.Contains(t, kept, &movedB)
	require.Contains(t, lost, &movedB)
	slices.Reverse(f.edges)
	for _, spillAt := range []int{1, 1 << 20} {
		t.Run(fmt.Sprint(spillAt), func(t *testing.T) {
			got, stats := f.private(t, spillAt)
			require.Equal(t, kept, got, "complete-key oracle, regardless of insertion order")
			require.Equal(t, 257, stats.Claimed)
			require.Equal(t, 2, stats.Windows)
			require.Equal(t, moves, stats.Reindexes)
			if spillAt == 1 {
				require.Equal(t, stats.Claimed, stats.Flushes)
				require.Positive(t, stats.SpillBytes)
			} else {
				require.Zero(t, stats.Flushes)
				require.Zero(t, stats.SpillBytes)
			}
		})
	}
}
