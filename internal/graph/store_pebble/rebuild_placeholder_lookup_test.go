package store_pebble

import (
	"bytes"
	"context"
	"encoding/binary"
	"path/filepath"
	"testing"

	"github.com/cockroachdb/pebble/v2"
	"github.com/stretchr/testify/require"
	"github.com/zzet/gortex/internal/graph"
)

// The pre-lookup claim kernel from cab57980, with the current move stream.
// Keep its range deletes and do not use the membership set.
func legacyPlaceholderClaim(ctx context.Context, p *RebuildPlaceholderSources, rp graph.PlaceholderRepoint) error {
	if rp.NewFrom == rp.OldFrom || rp.OldFrom == "" || rp.NewFrom == "" {
		return nil
	}
	site := placeholderSite(rp.OldFrom, rp.FilePath, rp.Line)
	before := p.stats.Claimed
	if err := pendingVisit(ctx, p.build.pending, site, func(r row) error {
		key := binary.BigEndian.AppendUint64(bytes.Clone(placeholderMoves), uint64(p.stats.Claimed))
		p.stats.Claimed++
		return p.stageMove(ctx, append(key, r.key[len(site):]...), []byte(rp.NewFrom))
	}); err != nil {
		return err
	}
	if p.stats.Claimed > before {
		return p.build.pending.DeleteRange(site, prefixEnd(site), pebble.NoSync)
	}
	return nil
}

func legacyPlaceholderRecord(ctx context.Context, p *RebuildPlaceholderSources, batch []graph.EdgeReindex) error {
	return placeholderError(p.build.withWrites(func() error {
		for i, ri := range batch {
			if err := ctx.Err(); err != nil {
				return err
			}
			if err := p.link(ri); err != nil {
				return err
			}
			for _, rp := range graph.PlaceholderSourceRepoints(batch[i : i+1]) {
				if err := legacyPlaceholderClaim(ctx, p, rp); err != nil {
					return err
				}
			}
		}
		return ctx.Err()
	}))
}

func placeholderLookupRows(t *testing.T, b *BuildStore, prefix []byte) []row {
	t.Helper()
	var rows []row
	require.NoError(t, pendingVisit(t.Context(), b.pending, prefix, func(r row) error {
		rows = append(rows, row{bytes.Clone(r.key), bytes.Clone(r.value)})
		return nil
	}))
	return rows
}

func placeholderMoveRows(t *testing.T, p *RebuildPlaceholderSources) []row {
	t.Helper()
	var rows []row
	require.NoError(t, p.visitMoves(t.Context(), func(r row) error {
		rows = append(rows, row{bytes.Clone(r.key), bytes.Clone(r.value)})
		return nil
	}))
	return rows
}

func TestRebuildPlaceholderLookupEquality(t *testing.T) {
	const source = "unresolved::field"
	const file = "repo/f.go"
	edges := []*graph.Edge{
		{From: source, To: "arg1", Kind: graph.EdgeArgOf, FilePath: file, Line: 1},
		{From: source, To: "arg2", Kind: graph.EdgeValueFlow, FilePath: file, Line: 1},
		{From: "repo/" + source, To: "arg3", Kind: graph.EdgeArgOf, FilePath: file, Line: 1},
		{From: source, To: "later", Kind: graph.EdgeValueFlow, FilePath: file, Line: 2},
		{From: source, To: "unclaimed", Kind: graph.EdgeArgOf, FilePath: file, Line: 3},
	}
	move := func(old, dest string, line int) graph.EdgeReindex {
		return graph.EdgeReindex{OldTo: old, Edge: &graph.Edge{From: "repo/caller", To: dest, Kind: graph.EdgeReferences, FilePath: file, Line: line}}
	}
	batches := [][]graph.EdgeReindex{
		{move("unresolved::absent", "none", 1), move(source, "winner", 1)},
		{move(source, "loser", 1), move(source, "second", 2), {OldTo: "arg2", Edge: &graph.Edge{From: source, To: "retarget", Kind: graph.EdgeValueFlow, FilePath: file, Line: 1}}},
		{move("unresolved::absent", "none", 3)},
	}
	type result struct {
		rows    [][]row
		moves   [][]row
		stats   []RebuildPlaceholderStats
		applied []row
		err     string
	}
	run := func(t *testing.T, legacy bool, fail bool) result {
		b := newBuild(t, FullProfile)
		b.AddBatch(nil, edges)
		p, err := b.CapturePlaceholderSources(t.Context(), 1<<20)
		require.NoError(t, err)
		defer func() { require.NoError(t, p.Close()) }()
		if !legacy {
			require.Len(t, p.sites, 4)
		}
		// The legacy range deletes and the point deletes may produce different
		// physical spill sizes. Compare the logical counts only.
		logicalStats := func() RebuildPlaceholderStats {
			stats := p.stats
			stats.Flushes = 0
			stats.SpillBytes = 0
			return stats
		}
		var got result
		capture := func() {
			require.Empty(t, placeholderLookupRows(t, b, placeholderMoves))
			got.rows = append(got.rows, placeholderLookupRows(t, b, placeholderPrefix))
			got.moves = append(got.moves, placeholderMoveRows(t, p))
			got.stats = append(got.stats, logicalStats())
		}
		capture()
		record := func(batch []graph.EdgeReindex) error {
			if legacy {
				return legacyPlaceholderRecord(t.Context(), p, batch)
			}
			return p.Record(t.Context(), batch)
		}
		for i, batch := range batches {
			if fail && i == 1 {
				// After the first committed claim batch, make the pending store
				// read-only. The duplicate claims are empty. The next move
				// is staged, but its site delete fails before the later link.
				require.NoError(t, b.pending.Flush())
				require.NoError(t, b.pending.Close())
				opts := pendingOptions(nil)
				opts.ErrorIfExists = false
				opts.ReadOnly = true
				b.pending, err = pebble.Open(filepath.Join(b.scratchParent, "pending"), opts)
				require.NoError(t, err)
				site := placeholderSite(source, file, 2)
				wantSite := placeholderLookupRows(t, b, site)
				require.Len(t, wantSite, 1)
				err = record(batch)
				require.ErrorIs(t, err, ErrRebuildPlaceholderSources)
				require.ErrorIs(t, err, pebble.ErrReadOnly)
				got.err = err.Error()
				require.Equal(t, 4, p.stats.Claimed)
				require.Zero(t, p.stats.Links)
				require.Equal(t, wantSite, placeholderLookupRows(t, b, site))
				if !legacy {
					require.Contains(t, p.sites, string(site))
				}
				capture()
				require.Len(t, got.moves[len(got.moves)-1], 4)
				require.NoError(t, b.pending.Close())
				opts.ReadOnly = false
				b.pending, err = pebble.Open(filepath.Join(b.scratchParent, "pending"), opts)
				require.NoError(t, err)
				return got
			}
			if i == 1 {
				b.ReindexEdges(batch[2:]) // retarget the claimed live edge
			}
			require.NoError(t, record(batch))
			capture()
			if !legacy && i == 0 {
				require.Len(t, p.sites, 2)
				require.NotContains(t, p.sites, string(placeholderSite(source, file, 1)))
				require.NotContains(t, p.sites, string(placeholderSite("repo/"+source, file, 1)))
			}
		}
		stats, err := p.Apply(t.Context())
		require.NoError(t, err)
		require.Equal(t, 5, stats.Census)
		require.Equal(t, 4, stats.Claimed)
		require.Equal(t, 1, stats.Links)
		require.Equal(t, 4, stats.Reindexes)
		got.stats = append(got.stats, logicalStats())
		got.applied = placeholderLookupRows(t, b, []byte{familyEdge})
		return got
	}
	for _, fail := range []bool{false, true} {
		want := run(t, true, fail)
		got := run(t, false, fail)
		require.Equal(t, want, got)
	}
}
