package store_pebble

import (
	"fmt"
	"os"
	"path/filepath"
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
	assertFileImportRunsRemoved(t, b)
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

func TestRebuildFileImportsIndexDoesNotSpill(t *testing.T) {
	b := newBuild(t, FullProfile)
	const files = 1300
	// The basename index exceeds 1 MiB but has no move rows to spill.
	wide := strings.Repeat(strings.Repeat("p", 200)+"/", 5)
	for i := range files {
		id := fmt.Sprintf("%s%04d/item.lua", wide, i)
		b.AddNode(&graph.Node{ID: id, Name: "item.lua", Kind: graph.KindFile, Language: "lua"})
	}
	b.AddEdge(&graph.Edge{From: wide + "0000/item.lua", To: "unresolved::import::miss", Kind: graph.EdgeImports})
	ctx := t.Context()
	reads := 0
	stats, err := b.RebuildFileImports(ctx, 1<<20, nil, func(idx *RebuildFileImportIndex, e *graph.Edge) (string, error) {
		file, err := idx.File(ctx, e.From)
		require.NoError(t, err)
		require.Equal(t, graph.FileLanguageNode{ID: e.From, Language: "lua"}, file)
		count := 0
		require.NoError(t, idx.VisitBasename(ctx, "item.lua", func(string) { count++ }))
		require.Equal(t, files, count)
		reads++
		return "", nil
	}, nil)
	require.NoError(t, err)
	require.Equal(t, 1, reads)
	require.Equal(t, files, stats.Files)
	require.Zero(t, stats.LuaMoves+stats.GodotMoves)
	require.Zero(t, stats.Flushes)
	require.Zero(t, stats.SpillBytes)
	assertFileImportRunsRemoved(t, b)
	require.NoError(t, pendingVisit(ctx, b.pending, fileImportPrefix, func(row) error {
		return fmt.Errorf("file-import scratch survived completion")
	}))
}

func TestRebuildFileImportsRunAndTail(t *testing.T) {
	b := newBuild(t, FullProfile)
	ctx := t.Context()
	const threshold = 1 << 10
	b.AddNode(&graph.Node{ID: "a.lua", Name: "a.lua", Kind: graph.KindFile, Language: "lua"})
	var want []*graph.Edge
	for i, from := range []string{"a.lua", "b.lua", "c.lua"} {
		e := &graph.Edge{From: from, To: "unresolved::import::target", Kind: graph.EdgeImports, Line: i + 1}
		if i == 1 {
			e.Via = strings.Repeat("x", threshold)
		}
		b.AddEdge(e)
		moved := *e
		moved.To, moved.Origin = "target.lua", graph.OriginASTResolved
		want = append(want, &moved)
	}
	var bytes uint64
	stats, err := b.RebuildFileImports(ctx, threshold, nil, func(_ *RebuildFileImportIndex, e *graph.Edge) (string, error) {
		if e.From == "c.lua" {
			// The small first move must spill before the oversized move
			// is appended. Each run contains one move; the third is the tail.
			paths, err := filepath.Glob(filepath.Join(b.scratchParent, "pending", "file-import-*", "*.sst"))
			require.NoError(t, err)
			require.Len(t, paths, 2)
			for i, path := range paths {
				info, err := os.Stat(path)
				require.NoError(t, err)
				bytes += uint64(info.Size())
				var got []*graph.Edge
				require.NoError(t, visitFileImportRun(ctx, path, fileImportLua, func(r row) error {
					_, raw, err := readKeyString(r.value)
					if err != nil {
						return err
					}
					move, err := decodeEdge(raw)
					if err != nil {
						return err
					}
					got = append(got, move)
					return nil
				}))
				require.Equal(t, want[i:i+1], got)
			}
			require.NoError(t, pendingVisit(ctx, b.pending, fileImportLua, func(row) error {
				return fmt.Errorf("file-import move was written to the overlay")
			}))
		}
		return "target.lua", nil
	}, nil)
	require.NoError(t, err)
	require.Equal(t, 3, stats.LuaMoves)
	require.Equal(t, 2, stats.Flushes)
	require.Positive(t, bytes)
	require.Equal(t, bytes, stats.SpillBytes)
	assertFileImportRunsRemoved(t, b)
	var got []*graph.Edge
	require.NoError(t, b.RebuildView().VisitEdges(ctx, func(e *graph.Edge) error {
		got = append(got, e)
		return nil
	}))
	require.Equal(t, want, got)
}

func TestRebuildFileImportsGodotRunAndTailOrder(t *testing.T) {
	b := newBuild(t, FullProfile)
	ctx := t.Context()
	p := fileImportPass{build: b, spillAt: 1 << 10}
	t.Cleanup(func() { require.NoError(t, os.RemoveAll(p.dir)) })
	stream := fileImportMoves{prefix: fileImportGodot}
	for i, from := range []string{"a.tscn", "b.tscn", "c.tscn", "d.tscn", "e.tscn", "f.tscn"} {
		kind := graph.EdgeReferences
		if i%2 == 1 {
			kind = graph.EdgeImports
		}
		e := &graph.Edge{From: from, To: "unresolved::res://target.gd", Kind: kind, FilePath: from, Line: 1}
		require.NoError(t, p.stageResolved(ctx, &stream, e, "target.gd"))
		if i == 1 || i == 3 {
			// Fix the layout: two mixed runs, then a mixed in-memory tail.
			require.Len(t, stream.rows, 2)
			require.NoError(t, p.spill(ctx, &stream))
		}
	}
	require.Equal(t, 2, stream.runs)
	require.Len(t, stream.rows, 2)
	var got []string
	require.NoError(t, stream.visit(ctx, p.dir, func(r row) error {
		from, _, err := readKeyString(r.key[len(fileImportGodot)+1+2:]) // move prefix, group, edge family and direction
		if err != nil {
			return err
		}
		got = append(got, from)
		return nil
	}))
	require.Equal(t, []string{"a.tscn", "c.tscn", "e.tscn", "b.tscn", "d.tscn", "f.tscn"}, got)
}

func assertFileImportRunsRemoved(t *testing.T, b *BuildStore) {
	t.Helper()
	dirs, err := filepath.Glob(filepath.Join(b.scratchParent, "pending", "file-import-*"))
	require.NoError(t, err)
	require.Empty(t, dirs)
}
