package store_pebble

import (
	"fmt"
	"path/filepath"
	"strings"
	"testing"

	"github.com/cockroachdb/pebble/v2"
	"github.com/stretchr/testify/require"
	"github.com/zzet/gortex/internal/graph"
)

func TestRebuildPlaceholderSourcesSpill(t *testing.T) {
	for _, threshold := range []int{1, 1 << 20} {
		t.Run(fmt.Sprint(threshold), func(t *testing.T) {
			ctx := t.Context()
			b := newBuild(t, FullProfile)
			const source = "unresolved::field"
			suffix := ""
			if threshold > 1 {
				suffix = strings.Repeat("x", 128<<10)
			}
			for i := range 9 {
				b.AddEdge(&graph.Edge{From: source, Kind: graph.EdgeArgOf, To: fmt.Sprint(i) + suffix})
			}
			ref := &graph.Edge{From: "caller", To: source, Kind: graph.EdgeReferences}
			b.AddEdge(ref)
			resolved := *ref
			resolved.To = "field"
			batch := []graph.EdgeReindex{{Edge: &resolved, OldTo: source}}
			b.ReindexEdges(batch)
			// Count physical spill from scratch, not the seed mutations.
			require.NoError(t, b.pending.Flush())
			p, err := b.CapturePlaceholderSources(ctx, threshold)
			require.NoError(t, err)
			defer func() { require.NoError(t, p.Close()) }()
			require.NoError(t, p.Record(ctx, batch))
			stats, err := p.Apply(ctx)
			require.NoError(t, err)
			require.Equal(t, 9, stats.Claimed)
			require.Equal(t, 9, stats.Reindexes)
			require.Equal(t, 1, stats.Windows)
			require.Positive(t, stats.Flushes)
			require.Positive(t, stats.SpillBytes)
			var saved int
			require.NoError(t, b.RebuildView().VisitEdges(ctx, func(e *graph.Edge) error {
				if e.From == resolved.To && e.Kind == graph.EdgeArgOf {
					saved++
				}
				return nil
			}))
			require.Equal(t, 9, saved)
			require.NoError(t, p.Close())
			require.NoError(t, pendingVisit(ctx, b.pending, placeholderPrefix, func(row) error {
				return fmt.Errorf("placeholder scratch survived Close")
			}))
		})
	}
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
	require.NoError(t, pendingVisit(ctx, b.pending, placeholderPrefix, func(row) error {
		return fmt.Errorf("placeholder scratch survived failed refetch")
	}))
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
	p := &RebuildPlaceholderSources{build: &BuildStore{pending: ro}}
	err = p.Close()
	require.ErrorIs(t, err, ErrRebuildPlaceholderSources)
	require.ErrorIs(t, err, pebble.ErrReadOnly)
	raw, err := pendingGet(ro, placeholderPrefix, copyValue)
	require.NoError(t, err)
	require.Equal(t, []byte("scratch"), raw)
}
