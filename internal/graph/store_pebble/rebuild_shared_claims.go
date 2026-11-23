package store_pebble

import (
	"bytes"
	"context"
	"encoding/binary"
	"fmt"
	"iter"

	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/sql"
)

// RebuildSharedClaimStats keeps the raw census separate from accepted keys.
type RebuildSharedClaimStats struct {
	RebuildShadowedIDStats
	AcceptedStrings, AcceptedAnnotations, AcceptedColumns, UnexplainedNodes uint64
}

// ReduceRebuildSharedClaims checks an immutable, repeatable receipt sequence
// before calling use. Receipts have distinct literal file paths; use replays
// them in the same order and passes their one-based ordinals to keep. Keep is
// valid only during use. Neither pass writes graph rows. A complete provenance
// refusal returns the census; other errors invalidate it.
func (b *BuildStore) ReduceRebuildSharedClaims(ctx context.Context, maxScratchBytes uint64,
	receipts iter.Seq2[ParseFacts, error], use func(keep func(id string, receipt uint64) (bool, error)) error,
) (RebuildSharedClaimStats, error) {
	var stats RebuildSharedClaimStats
	claim := func(node func(string, uint64) error, edge func(graph.EdgeIdentity, uint64) error) error {
		var ordinal uint64
		for f, err := range receipts {
			if err != nil {
				return err
			}
			ordinal++
			for _, n := range f.Nodes {
				if err := node(n.ID, ordinal); err != nil {
					return err
				}
			}
			for _, e := range f.Edges {
				if err := edge(graph.EdgeIdentityFor(e), ordinal); err != nil {
					return err
				}
			}
		}
		return nil
	}
	raw, err := b.censusRebuildClaims(ctx, maxScratchBytes, claim, func(raw *RebuildShadowedIDStats, put func([]byte, []byte) error) error {
		if raw.Shadowed != 0 {
			if err := b.qualifyRebuildSharedClaims(ctx, receipts, &stats, put); err != nil {
				return err
			}
		}
		accepted := stats.AcceptedStrings + stats.AcceptedAnnotations + stats.AcceptedColumns
		if accepted > raw.Shadowed {
			return invalid("shared claim census changed during replay")
		}
		stats.UnexplainedNodes = raw.Shadowed - accepted
		if stats.UnexplainedNodes != 0 || raw.ShadowedEdges != 0 || use == nil {
			return nil
		}
		return use(func(id string, receipt uint64) (bool, error) {
			if err := ctx.Err(); err != nil {
				return false, err
			}
			if raw.Shadowed == 0 {
				return true, nil
			}
			key := append(keyStrings(rebuildClaimPrefix, id), sharedClaimWinnerKey)
			winner, err := pendingGet(b.pending, key, func(v []byte) (uint64, error) {
				if len(v) <= 8 {
					return 0, invalid("missing qualified shared claim winner")
				}
				return binary.BigEndian.Uint64(v), nil
			})
			return err == nil && (winner == 0 || winner == receipt), err
		})
	})
	if err != nil {
		return RebuildSharedClaimStats{}, err
	}
	stats.RebuildShadowedIDStats = raw
	if count := stats.UnexplainedNodes + stats.ShadowedEdges; count != 0 {
		return stats, fmt.Errorf("receipt shadowed_id=%d unexplained_id=%d shadowed_edge=%d: %w",
			stats.Shadowed, stats.UnexplainedNodes, stats.ShadowedEdges, &RebuildUnknownProvenanceError{Count: count})
	}
	return stats, nil
}

// A suffix after a complete node key cannot match a different node ID or an
// edge key. The winner value stores its ordinal and complete encoded node.
const (
	sharedClaimWinnerKey byte = 1
	sharedClaimPathKey   byte = 2
)

func (b *BuildStore) qualifyRebuildSharedClaims(ctx context.Context, receipts iter.Seq2[ParseFacts, error],
	stats *RebuildSharedClaimStats, put func([]byte, []byte) error,
) error {
	var ordinal uint64
	for f, err := range receipts {
		if err != nil {
			return err
		}
		ordinal++
		// Keep only this receipt's contested IDs. Native same-file re-add
		// is an upsert, not another cross-receipt claim.
		last := make(map[string]*graph.Node)
		for _, n := range f.Nodes {
			if err := ctx.Err(); err != nil {
				return err
			}
			if prev, seen := last[n.ID]; seen {
				if prev == nil || prev.FilePath != n.FilePath || (prev.RepoPrefix != "" && n.RepoPrefix == "") {
					last[n.ID] = nil // not an ordinary whole-row same-file upsert
				} else {
					last[n.ID] = n
				}
				continue
			}
			collided, err := pendingGet(b.pending, keyStrings(rebuildClaimPrefix, n.ID), func(v []byte) (bool, error) {
				if len(v) != 8 {
					return false, invalid("shared claim census ordinal")
				}
				return binary.BigEndian.Uint64(v) == 0, nil
			})
			if err != nil {
				return err
			}
			if collided {
				last[n.ID] = n
			}
		}
		// These sidecars belong to per-file declarations, never to any of
		// the declared shared families. They cannot follow a losing claim.
		for _, c := range f.Constants {
			if _, contested := last[c.NodeID]; contested {
				last[c.NodeID] = nil
			}
		}
		for id := range f.Shingles {
			if _, contested := last[id]; contested {
				last[id] = nil
			}
		}
		for _, n := range f.Nodes {
			winner, seen := last[n.ID]
			if !seen {
				continue
			}
			if err := ctx.Err(); err != nil {
				return err
			}
			if err := b.reduceRebuildSharedClaim(n.ID, winner, ordinal, stats, put); err != nil {
				return err
			}
			delete(last, n.ID)
		}
	}
	return ctx.Err()
}

func (b *BuildStore) reduceRebuildSharedClaim(id string, n *graph.Node, ordinal uint64,
	stats *RebuildSharedClaimStats, put func([]byte, []byte) error,
) error {
	key := append(keyStrings(rebuildClaimPrefix, id), sharedClaimWinnerKey)
	prior, err := pendingGet(b.pending, key, copyValue)
	if err != nil || len(prior) == 1 { // permanently unexplained
		return err
	}
	var prev *graph.Node
	if prior != nil {
		if len(prior) <= 8 {
			return invalid("shared claim winner")
		}
		prev, err = decodeNode(prior[8:])
		if err != nil {
			return err
		}
	}
	family := stats.sharedClaimFamily(n)
	reject := func() error {
		if prev != nil {
			(*stats.sharedClaimFamily(prev))--
		}
		return put(key, []byte{0})
	}
	if family == nil || (prev != nil && (family != stats.sharedClaimFamily(prev) || n.Kind != prev.Kind ||
		(n.RepoPrefix == "") != (prev.RepoPrefix == ""))) {
		return reject()
	}
	if n.Kind != graph.KindColumn {
		// Native sharing needs distinct paths for every pair, even if a
		// repeated path was not the winning path.
		pathKey := appendKeyString(append(keyStrings(rebuildClaimPrefix, id), sharedClaimPathKey), n.FilePath)
		seen, err := pendingGet(b.pending, pathKey, func([]byte) (bool, error) { return true, nil })
		if err != nil {
			return err
		}
		if seen {
			return reject()
		}
		if err := put(pathKey, []byte{1}); err != nil {
			return err
		}
	}
	if prev != nil && prev.FilePath < n.FilePath {
		return nil
	}
	row, err := encodeNode(n)
	if err != nil {
		return err
	}
	// T169 witnessed native SQL column placement vary with parse order.
	// Retain one whole row ordered by stamped path, then encoded row bytes.
	if prev != nil && prev.FilePath == n.FilePath && bytes.Compare(prior[8:], row) <= 0 {
		return nil
	}
	if prev == nil {
		(*family)++
	}
	return put(key, append(binary.BigEndian.AppendUint64(nil, ordinal), row...))
}

func (s *RebuildSharedClaimStats) sharedClaimFamily(n *graph.Node) *uint64 {
	if n == nil || n.FilePath == "" {
		return nil
	}
	synthetic, _ := n.Meta["synthetic"].(bool)
	switch n.Kind {
	case graph.KindString:
		return &s.AcceptedStrings
	case graph.KindType:
		if synthetic && n.Meta["kind"] == "annotation" && n.Language != "" && n.Name != "" &&
			n.ID == "annotation::"+n.Language+"::"+n.Name {
			return &s.AcceptedAnnotations
		}
	case graph.KindColumn:
		if !synthetic && n.Language == "go" {
			table, _ := n.Meta["table"].(string)
			column, _ := n.Meta["column"].(string)
			dialect, _ := n.Meta["dialect"].(string)
			schema, _ := n.Meta["schema"].(string)
			if table != "" && column != "" && dialect != "" && n.Name == column && n.ID == sql.ColumnNodeID(dialect, schema, table, column) {
				return &s.AcceptedColumns
			}
		}
	}
	return nil
}
