package store_pebble

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/zzet/gortex/internal/graph"
)

func TestRebuildFnPointerScratchStaysPending(t *testing.T) {
	for _, threshold := range []int{1, 1 << 20} {
		t.Run(fmt.Sprint(threshold), func(t *testing.T) {
			b := newBuild(t, FullProfile)
			flushed := b.pending.Metrics().Levels[0].TableBytesFlushed
			p, err := b.CaptureFnPointerCensus(t.Context(), threshold, func(*graph.Edge) bool { return false })
			require.NoError(t, err)
			defer func() { require.NoError(t, p.Close()) }()
			wide := "from"
			if threshold > 1 {
				wide += strings.Repeat("x", 128<<10)
			}
			require.NoError(t, p.Seed(wide, "fn"))
			for i := range 10 {
				e := &graph.Edge{Kind: graph.EdgeReferences, From: "source", To: "target", Line: i}
				require.NoError(t, p.SaveCopy(e, fmt.Sprint(i), wide))
			}
			changed, err := p.Sweep(t.Context())
			require.NoError(t, err)
			require.True(t, changed)
			targets, err := p.Targets(t.Context(), "9", 64)
			require.NoError(t, err)
			require.Equal(t, []string{"fn"}, targets)
			spill, err := p.Apply(t.Context())
			require.NoError(t, err)
			require.Zero(t, spill)
			require.Empty(t, p.runDir)
			// Only the small fixture rules out an automatic Pebble flush.
			if threshold == 1 {
				require.Equal(t, flushed, b.pending.Metrics().Levels[0].TableBytesFlushed)
			}
			require.NoError(t, p.Close())
			require.NoError(t, pendingVisit(t.Context(), b.pending, fnPointerPrefix, func(r row) error {
				t.Errorf("scratch row survived Close: %x", r.key)
				return nil
			}))
		})
	}
}

func TestRebuildFnPointerMoveRunsAndTail(t *testing.T) {
	b := newBuild(t, FullProfile)
	ctx := t.Context()
	const threshold = 1 << 10
	var originals []*graph.Edge
	for i, from := range []string{"a", "b", "c"} {
		for _, kind := range []graph.EdgeKind{graph.EdgeReferences, graph.EdgeCalls} {
			e := &graph.Edge{From: from, To: "unresolved::old", Kind: kind, FilePath: "dispatch.go", Line: 1}
			if i < 2 {
				e.Via = strings.Repeat("x", threshold)
			}
			originals = append(originals, e)
		}
	}
	b.AddBatch(nil, originals)
	p, err := b.CaptureFnPointerCensus(ctx, threshold, func(*graph.Edge) bool { return false })
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, p.Close()) })
	columns := [...]byte{fnPointerRegMoves, fnPointerCallMoves}
	var want [2][]graph.EdgeIdentity
	var spill FnPointerSpill
	for i, old := range originals {
		j := i % 2
		moved := *old
		moved.To = "new"
		require.NoError(t, p.StageMove(ctx, graph.EdgeReindex{Edge: &moved, OldTo: old.To}))
		want[j] = append(want[j], graph.EdgeIdentityFor(old))
		if i < 4 {
			info, err := os.Stat(p.runPath(columns[j], i/2))
			require.NoError(t, err)
			value, err := encodeEdge(&moved)
			require.NoError(t, err)
			spill.Flushes++
			spill.TableBytes += uint64(info.Size())
			spill.InputBytes += int64(len(fnPointerEdgeKey(columns[j], old)) + len(appendKeyString(nil, old.To)) + len(value))
		}
	}
	for i, column := range columns {
		require.Equal(t, 2, p.moves[i].runs)
		require.Len(t, p.moves[i].rows, 1)
		require.NoError(t, pendingVisit(ctx, b.pending, fnPointerColumn(column), func(row) error {
			return fmt.Errorf("deferred function-pointer move entered the overlay")
		}))
		var got []graph.EdgeIdentity
		require.NoError(t, p.visitMoves(ctx, column, func(r row) error {
			old, err := decodeLinkRecorder(r.key[len(fnPointerColumn(column)):])
			if err == nil {
				got = append(got, old)
			}
			return err
		}))
		require.Equal(t, want[i], got)
	}
	got, err := p.Apply(ctx)
	require.NoError(t, err)
	require.Equal(t, spill, got)
	for _, ids := range want {
		for _, id := range ids {
			saved, err := p.View().Edge(ctx, id)
			require.NoError(t, err)
			require.Nil(t, saved)
			id.To = "new"
			saved, err = p.View().Edge(ctx, id)
			require.NoError(t, err)
			require.NotNil(t, saved)
		}
	}
	require.NoError(t, p.Close())
	_, err = os.Stat(p.runDir)
	require.ErrorIs(t, err, os.ErrNotExist)
}

func TestRebuildFnPointerSpillsBeforeAppend(t *testing.T) {
	b := newBuild(t, FullProfile)
	ctx := t.Context()
	const threshold = 1 << 10
	p, err := b.CaptureFnPointerCensus(ctx, threshold, func(*graph.Edge) bool { return false })
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, p.Close()) })

	// Each encoded row fits alone, but the two together exceed the threshold.
	first := graph.Edge{From: "a", To: "old", Kind: graph.EdgeCalls, Via: strings.Repeat("x", 3*threshold/4)}
	second := first
	second.From = "b"
	for _, old := range []graph.Edge{first, second} {
		moved := old
		moved.To = "new"
		require.NoError(t, p.StageMove(ctx, graph.EdgeReindex{Edge: &moved, OldTo: old.To}))
	}

	stream := &p.moves[fnPointerCallMoves-fnPointerRegMoves]
	require.Equal(t, 1, stream.runs)
	require.Len(t, stream.rows, 1)
	require.Equal(t, fnPointerEdgeKey(fnPointerCallMoves, &second), stream.rows[0].key)
	var keys []string
	require.NoError(t, visitLinkRun(ctx, p.runPath(fnPointerCallMoves, 0), func(r row) error {
		keys = append(keys, string(r.key))
		return nil
	}))
	require.Equal(t, []string{string(fnPointerEdgeKey(fnPointerCallMoves, &first))}, keys)
}

func TestRebuildFnPointerRefusesUnorderedMoves(t *testing.T) {
	for _, tc := range []struct {
		name, from string
	}{
		{"descending", "a"},
		{"duplicate", "b"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			b := newBuild(t, FullProfile)
			ctx := t.Context()
			p, err := b.CaptureFnPointerCensus(ctx, 1, func(*graph.Edge) bool { return false })
			require.NoError(t, err)
			t.Cleanup(func() { require.NoError(t, p.Close()) })
			move := &graph.Edge{From: "b", To: "new", Kind: graph.EdgeCalls, FilePath: "dispatch.go", Line: 1}
			require.NoError(t, p.StageMove(ctx, graph.EdgeReindex{Edge: move, OldTo: "old"}))
			before := p.spill
			staged, err := os.ReadFile(p.runPath(fnPointerCallMoves, 0))
			require.NoError(t, err)
			row := *move
			row.From, row.To = tc.from, "other"
			err = p.StageMove(ctx, graph.EdgeReindex{Edge: &row, OldTo: "old"})
			require.ErrorIs(t, err, errInvalidRecord)
			require.ErrorContains(t, err, "function-pointer move keys must be strictly ascending")
			require.Equal(t, before, p.spill)
			paths, err := filepath.Glob(filepath.Join(p.runDir, "*.sst"))
			require.NoError(t, err)
			require.Equal(t, []string{p.runPath(fnPointerCallMoves, 0)}, paths)
			after, err := os.ReadFile(paths[0])
			require.NoError(t, err)
			require.Equal(t, staged, after)
			require.Empty(t, p.moves[fnPointerCallMoves-fnPointerRegMoves].rows)
			runDir := p.runDir
			require.NoError(t, p.Close())
			_, err = os.Stat(runDir)
			require.ErrorIs(t, err, os.ErrNotExist)
		})
	}
}

func TestRebuildFnPointerFanoutMarksOnlyNewCalls(t *testing.T) {
	b := newBuild(t, FullProfile)
	const file = "dispatch.go"
	incumbent := &graph.Edge{From: "caller", To: "old", Kind: graph.EdgeCalls, FilePath: file, Line: 3,
		Origin: graph.OriginASTResolved}
	fanout := &graph.Edge{From: "caller", To: "extra", Kind: graph.EdgeCalls, FilePath: file, Line: 3,
		Origin: graph.OriginASTInferred}
	b.AddEdge(incumbent)
	ctx := t.Context()
	pass, err := b.CaptureFnPointerCensus(ctx, 1, func(*graph.Edge) bool { return false })
	require.NoError(t, err)
	defer func() { require.NoError(t, pass.Close()) }()
	collision := *incumbent
	collision.Origin = graph.OriginASTInferred
	require.NoError(t, pass.StageFanout(&collision))
	require.NoError(t, pass.StageFanout(fanout))
	spill, err := pass.Apply(ctx)
	require.NoError(t, err)
	require.Zero(t, spill)

	saved, err := b.pendingEdge(pendingEdgeKey(incumbent))
	require.NoError(t, err)
	require.Equal(t, incumbent, saved)
	custody, err := readRebuildEdgeCustody(b.pending, pendingEdgeKey(incumbent))
	require.NoError(t, err)
	require.Empty(t, custody)
	saved, err = b.pendingEdge(pendingEdgeKey(fanout))
	require.NoError(t, err)
	require.Equal(t, fanout, saved)
	_, err = rebuildReceiptKey(b.pending, pendingEdgeKey(fanout))
	require.ErrorIs(t, err, ErrRebuildUnknownProvenance)
}
