package store_pebble

import (
	"bytes"
	"context"
	"encoding/binary"
	"path/filepath"
	"sync/atomic"
	"testing"

	"github.com/cockroachdb/pebble/v2"
	"github.com/stretchr/testify/require"
	"github.com/zzet/gortex/internal/graph"
)

func TestRebuildPlaceholderClaimDeletesPoints(t *testing.T) {
	// Flush the original rows first, and keep the two flushes in separate
	// SSTables so a reverted DeleteRange cannot be elided by compaction.
	opts := pendingOptions(nil)
	opts.DisableAutomaticCompactions = true
	db, err := pebble.Open(filepath.Join(t.TempDir(), "pending"), opts)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, db.Close()) })
	b := &BuildStore{pending: db}
	rp := graph.PlaceholderRepoint{OldFrom: "unresolved::x", NewFrom: "winner", FilePath: "f.go", Line: 1}
	site := placeholderSite(rp.OldFrom, rp.FilePath, rp.Line)
	other := placeholderSite(rp.OldFrom, rp.FilePath, 2)
	p := &RebuildPlaceholderSources{build: b, spillAt: 1 << 20, sites: map[string]struct{}{string(site): {}, string(other): {}}}
	var wantMoves []row
	for i, target := range []string{"a", "b", "c"} {
		edgeKey := pendingEdgeKey(&graph.Edge{From: rp.OldFrom, To: target, Kind: graph.EdgeArgOf, FilePath: rp.FilePath, Line: rp.Line})
		require.NoError(t, db.Set(append(bytes.Clone(site), edgeKey...), nil, pebble.NoSync))
		moveKey := binary.BigEndian.AppendUint64(bytes.Clone(placeholderMoves), uint64(i))
		wantMoves = append(wantMoves, row{append(moveKey, edgeKey...), []byte(rp.NewFrom)})
	}
	otherKey := append(bytes.Clone(other), pendingEdgeKey(&graph.Edge{From: rp.OldFrom, To: "a", Kind: graph.EdgeArgOf, FilePath: rp.FilePath, Line: 2})...)
	require.NoError(t, db.Set(otherKey, []byte("keep"), pebble.NoSync))
	require.NoError(t, db.Flush())

	require.NoError(t, p.claim(t.Context(), rp))
	require.Empty(t, placeholderLookupRows(t, b, site))
	require.Equal(t, []row{{otherKey, []byte("keep")}}, placeholderLookupRows(t, b, placeholderSites))
	require.Equal(t, wantMoves, placeholderLookupRows(t, b, placeholderMoves))
	require.Equal(t, 3, p.stats.Claimed)
	require.Equal(t, map[string]struct{}{string(other): {}}, p.sites)

	require.NoError(t, db.Flush())
	levels, err := db.SSTables(pebble.WithProperties())
	require.NoError(t, err)
	var rangeDeletes uint64
	var tables int
	for _, level := range levels {
		for _, table := range level {
			require.NotNil(t, table.Properties)
			rangeDeletes += table.Properties.NumRangeDeletions
			tables++
		}
	}
	require.Positive(t, tables)
	require.Zero(t, rangeDeletes)
	require.NoError(t, p.Close())
}

func TestRebuildPlaceholderClaimNoRowsOrRepeated(t *testing.T) {
	// Read-only pending rejects even an empty batch commit. First claim
	// normally, then verify both no-op paths without a writable pending DB.
	dir := filepath.Join(t.TempDir(), "pending")
	opts := pendingOptions(nil)
	db, err := pebble.Open(dir, opts)
	require.NoError(t, err)
	b := &BuildStore{pending: db}
	t.Cleanup(func() {
		if b.pending != nil {
			require.NoError(t, b.pending.Close())
		}
	})
	rp := graph.PlaceholderRepoint{OldFrom: "unresolved::x", NewFrom: "winner"}
	site := placeholderSite(rp.OldFrom, rp.FilePath, rp.Line)
	empty := placeholderSite(rp.OldFrom, rp.FilePath, 1)
	p := &RebuildPlaceholderSources{build: b, spillAt: 1 << 20, sites: map[string]struct{}{string(site): {}, string(empty): {}}}
	edgeKey := pendingEdgeKey(&graph.Edge{From: rp.OldFrom, To: "a", Kind: graph.EdgeArgOf})
	require.NoError(t, db.Set(append(bytes.Clone(site), edgeKey...), nil, pebble.NoSync))
	require.NoError(t, p.claim(t.Context(), rp))
	require.NoError(t, db.Flush())
	require.NoError(t, db.Close())
	b.pending = nil
	ro := pendingOptions(nil)
	ro.ErrorIfExists = false
	ro.ReadOnly = true
	b.pending, err = pebble.Open(dir, ro)
	require.NoError(t, err)
	wantMoves := placeholderLookupRows(t, b, placeholderMoves)
	require.Len(t, wantMoves, 1)
	rp.NewFrom = "loser"
	require.NoError(t, p.claim(t.Context(), rp))
	rp.Line = 1
	require.NoError(t, p.claim(t.Context(), rp))
	require.Equal(t, wantMoves, placeholderLookupRows(t, b, placeholderMoves))
	require.Empty(t, placeholderLookupRows(t, b, placeholderSites))
	require.Equal(t, 1, p.stats.Claimed)
	require.Equal(t, map[string]struct{}{string(empty): {}}, p.sites)
}

func TestRebuildPlaceholderClaimCanceledDuringVisit(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	// Cancel when the first move row's forced flush finishes, before the
	// visit can advance to its second site row. The seed flush is not armed.
	var armed atomic.Bool
	opts := pendingOptions(nil)
	opts.DisableAutomaticCompactions = true
	opts.AddEventListener(pebble.EventListener{FlushEnd: func(pebble.FlushInfo) {
		if armed.Load() {
			cancel()
		}
	}})
	db, err := pebble.Open(filepath.Join(t.TempDir(), "pending"), opts)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, db.Close()) })
	b := &BuildStore{pending: db}
	rp := graph.PlaceholderRepoint{OldFrom: "unresolved::x", NewFrom: "winner"}
	site := placeholderSite(rp.OldFrom, rp.FilePath, rp.Line)
	p := &RebuildPlaceholderSources{build: b, spillAt: 1, sites: map[string]struct{}{string(site): {}}}
	for _, target := range []string{"a", "b"} {
		edgeKey := pendingEdgeKey(&graph.Edge{From: rp.OldFrom, To: target, Kind: graph.EdgeArgOf})
		require.NoError(t, db.Set(append(bytes.Clone(site), edgeKey...), nil, pebble.NoSync))
	}
	require.NoError(t, db.Flush())
	wantSites := placeholderLookupRows(t, b, site)
	require.Len(t, wantSites, 2)
	armed.Store(true)
	require.ErrorIs(t, p.claim(ctx, rp), context.Canceled)
	require.Equal(t, 1, p.stats.Claimed)
	require.Equal(t, wantSites, placeholderLookupRows(t, b, site))
	require.Contains(t, p.sites, string(site))
}
