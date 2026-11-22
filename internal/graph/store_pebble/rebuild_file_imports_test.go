package store_pebble

import (
	"fmt"
	"strings"
	"testing"

	"github.com/cockroachdb/pebble/v2"
	"github.com/stretchr/testify/require"
	"github.com/zzet/gortex/internal/graph"
)

func TestRebuildFileImportsCorruptRead(t *testing.T) {
	ctx := t.Context()
	b := newBuild(t, FullProfile)
	seed := &graph.Node{ID: "caller.lua", Name: "caller.lua", Kind: graph.KindFile, Language: "lua"}
	target := &graph.Node{ID: "module.lua", Name: "module.lua", Kind: graph.KindFile, Language: "lua"}
	old := &graph.Edge{From: seed.ID, To: "unresolved::import::module", Kind: graph.EdgeImports, FilePath: seed.ID, Line: 2}
	b.AddBatch([]*graph.Node{seed, target}, []*graph.Edge{old})
	// Complete-key order visits the resolvable edge first, then hits corruption.
	bad := pendingEdgeKey(&graph.Edge{From: "z.lua", To: old.To, Kind: graph.EdgeImports})
	require.NoError(t, b.pending.Set(bad, []byte{0xff}, pebble.NoSync))
	var visited int
	stats, err := b.RebuildFileImports(ctx, 1, nil, func(idx *RebuildFileImportIndex, e *graph.Edge) (string, error) {
		visited++
		file, err := idx.File(ctx, target.ID)
		return file.ID, err
	}, nil) // no Godot language in this build
	require.ErrorIs(t, err, ErrRebuildFileImports)
	require.ErrorIs(t, err, ErrRebuildView)
	require.ErrorIs(t, err, errInvalidRecord)
	require.Equal(t, 1, visited, "the failure must follow a staged move")
	require.Zero(t, stats)
	require.NoError(t, pendingVisit(ctx, b.pending, fileImportPrefix, func(row) error {
		return fmt.Errorf("file-import scratch survived refusal")
	}))
	require.NoError(t, b.pending.Delete(bad, pebble.NoSync))
	var saved []*graph.Edge
	require.NoError(t, b.RebuildView().VisitEdges(ctx, func(e *graph.Edge) error {
		saved = append(saved, e)
		return nil
	}))
	require.Equal(t, []*graph.Edge{old}, saved)
}

func TestRebuildFileImportsIndexSpill(t *testing.T) {
	b := newBuild(t, FullProfile)
	const files = 1300
	// Force the basename index alone above 1 MiB, with no staged moves.
	wide := strings.Repeat(strings.Repeat("p", 200)+"/", 5)
	for i := range files {
		id := fmt.Sprintf("%s%04d/item.lua", wide, i)
		b.AddNode(&graph.Node{ID: id, Name: "item.lua", Kind: graph.KindFile, Language: "lua"})
	}
	// Any subsequent explicit flush bytes come from scratch, not input rows.
	require.NoError(t, b.pending.Flush())
	stats, err := b.RebuildFileImports(t.Context(), 1<<20, nil, nil, nil) // no edges
	require.NoError(t, err)
	require.Equal(t, files, stats.Files)
	require.Zero(t, stats.LuaMoves+stats.GodotMoves)
	require.Positive(t, stats.Flushes)
	require.Positive(t, stats.SpillBytes)
	require.NoError(t, pendingVisit(t.Context(), b.pending, fileImportPrefix, func(row) error {
		return fmt.Errorf("file-import scratch survived completion")
	}))
}
