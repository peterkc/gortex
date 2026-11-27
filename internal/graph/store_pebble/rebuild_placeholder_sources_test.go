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

func TestRebuildPlaceholderSourcesSpill(t *testing.T) {
	for _, tc := range []struct {
		name      string
		threshold int
		padding   int
		runs      int
	}{
		{"each move spills", 1, 0, 9},
		{"run and tail", 1 << 20, 128 << 10, 1},
		{"tail only", 1 << 20, 0, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := t.Context()
			b := newBuild(t, FullProfile)
			const source = "unresolved::field"
			suffix := strings.Repeat("x", tc.padding)
			for i := range 9 {
				b.AddEdge(&graph.Edge{From: source, Kind: graph.EdgeArgOf, To: fmt.Sprint(i) + suffix})
			}
			ref := &graph.Edge{From: "caller", To: source, Kind: graph.EdgeReferences}
			b.AddEdge(ref)
			resolved := *ref
			resolved.To = "field"
			batch := []graph.EdgeReindex{{Edge: &resolved, OldTo: source}}
			b.ReindexEdges(batch)
			p, err := b.CapturePlaceholderSources(ctx, tc.threshold)
			require.NoError(t, err)
			defer func() { require.NoError(t, p.Close()) }()
			require.Equal(t, 9, p.stats.Census)
			require.Zero(t, p.stats.Flushes)
			require.Zero(t, p.stats.SpillBytes)

			require.NoError(t, p.Record(ctx, batch))
			require.Empty(t, placeholderLookupRows(t, b, placeholderMoves))
			paths, err := filepath.Glob(filepath.Join(b.scratchParent, "pending", "placeholder-moves-*", "*.sst"))
			require.NoError(t, err)
			require.Len(t, paths, tc.runs)
			var spill uint64
			for _, path := range paths {
				info, err := os.Stat(path)
				require.NoError(t, err)
				require.Positive(t, info.Size())
				spill += uint64(info.Size())
			}
			stats, err := p.Apply(ctx)
			require.NoError(t, err)
			require.Equal(t, 9, stats.Claimed)
			require.Equal(t, 9, stats.Reindexes)
			require.Equal(t, 1, stats.Windows)
			require.Equal(t, tc.runs, stats.Flushes)
			require.Equal(t, spill, stats.SpillBytes)
			var saved int
			require.NoError(t, b.RebuildView().VisitEdges(ctx, func(e *graph.Edge) error {
				if e.From == resolved.To && e.Kind == graph.EdgeArgOf {
					saved++
				}
				return nil
			}))
			require.Equal(t, 9, saved)
			require.NoError(t, p.Close())
			assertPlaceholderRunsRemoved(t, b)
			require.NoError(t, pendingVisit(ctx, b.pending, placeholderPrefix, func(row) error {
				return fmt.Errorf("placeholder scratch survived Close")
			}))
		})
	}
}

func assertPlaceholderRunsRemoved(t *testing.T, b *BuildStore) {
	t.Helper()
	paths, err := filepath.Glob(filepath.Join(b.scratchParent, "pending", "placeholder-moves-*"))
	require.NoError(t, err)
	require.Empty(t, paths)
}

func TestRebuildPlaceholderSourcesReadFailure(t *testing.T) {
	ctx := t.Context()
	b := newBuild(t, FullProfile)
	e := &graph.Edge{From: "unresolved::x", To: "callee", Kind: graph.EdgeValueFlow}
	b.AddEdge(e)
	p, err := b.CapturePlaceholderSources(ctx, 1)
	require.NoError(t, err)
	defer func() { require.NoError(t, p.Close()) }()
	require.NoError(t, p.Record(ctx, []graph.EdgeReindex{{OldTo: e.From, Edge: &graph.Edge{To: "winner"}}}))
	require.NoError(t, b.pending.Set(pendingEdgeKey(e), []byte{0xff}, pebble.NoSync))
	stats, err := p.Apply(ctx)
	require.ErrorIs(t, err, ErrRebuildPlaceholderSources)
	require.ErrorIs(t, err, errInvalidRecord)
	require.Zero(t, stats)
	require.ErrorIs(t, b.RebuildView().mutationError(), errInvalidRecord)
	require.NoError(t, p.Close()) // cleanup bypasses the latched mutation failure
	assertPlaceholderRunsRemoved(t, b)
	require.NoError(t, pendingVisit(ctx, b.pending, placeholderPrefix, func(row) error {
		return fmt.Errorf("placeholder scratch survived failed refetch")
	}))
}

func TestRebuildPlaceholderSourcesMissingRun(t *testing.T) {
	ctx := t.Context()
	b := newBuild(t, FullProfile)
	e := &graph.Edge{From: "unresolved::x", To: "callee", Kind: graph.EdgeValueFlow}
	b.AddEdge(e)
	p, err := b.CapturePlaceholderSources(ctx, 1)
	require.NoError(t, err)
	defer func() { require.NoError(t, p.Close()) }()
	require.NoError(t, p.Record(ctx, []graph.EdgeReindex{{OldTo: e.From, Edge: &graph.Edge{To: "winner"}}}))
	require.Equal(t, 1, p.stats.Flushes)
	require.NoError(t, os.Remove(p.runPath(0)))
	before := rebuildFrameworkCustodyRows(t, b)
	stats, err := p.Apply(ctx)
	require.ErrorIs(t, err, ErrRebuildPlaceholderSources)
	require.ErrorIs(t, err, os.ErrNotExist)
	require.Zero(t, stats)
	require.Equal(t, before, rebuildFrameworkCustodyRows(t, b))
	require.NoError(t, p.Close())
	assertPlaceholderRunsRemoved(t, b)
}

func TestRebuildPlaceholderSourcesCloseFailure(t *testing.T) {
	// A read-only private DB rejects the range tombstone without an I/O hook.
	dir := filepath.Join(t.TempDir(), "pending")
	db, err := pebble.Open(dir, &pebble.Options{})
	require.NoError(t, err)
	require.NoError(t, db.Set(placeholderPrefix, []byte("scratch"), pebble.Sync))
	require.NoError(t, db.Close())
	ro, err := pebble.Open(dir, &pebble.Options{ReadOnly: true})
	require.NoError(t, err)
	defer func() { require.NoError(t, ro.Close()) }()
	runDir := t.TempDir()
	p := &RebuildPlaceholderSources{build: &BuildStore{pending: ro}, runDir: runDir}
	err = p.Close()
	require.ErrorIs(t, err, ErrRebuildPlaceholderSources)
	require.ErrorIs(t, err, pebble.ErrReadOnly)
	_, statErr := os.Stat(runDir)
	require.ErrorIs(t, statErr, os.ErrNotExist)
	raw, err := pendingGet(ro, placeholderPrefix, copyValue)
	require.NoError(t, err)
	require.Equal(t, []byte("scratch"), raw)
}
