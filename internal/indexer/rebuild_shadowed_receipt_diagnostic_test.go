package indexer

import (
	"cmp"
	"context"
	"fmt"
	"iter"
	"slices"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/graph/store_pebble"
)

type rebuildCensusKey struct {
	Node string             `json:"node,omitempty"`
	Edge graph.EdgeIdentity `json:"edge"`
}

type rebuildCensusClaim struct {
	Receipt      string `json:"receipt"`
	Language     string `json:"language"`
	RecordedPath string `json:"recorded_path"`
}

type rebuildCensusCollision struct {
	Key       rebuildCensusKey     `json:"key"`
	Group     string               `json:"group"`
	Claimants []rebuildCensusClaim `json:"claimants"`
}

// replayRebuildCensusClaims uses the same ordered receipt boundary as the clone
// census. It never consults the settled graph for an earlier owner.
func replayRebuildCensusClaims(ctx context.Context, receipts iter.Seq2[store_pebble.ParseFacts, error],
	claim func(rebuildCensusKey, rebuildCensusClaim, uint64) error,
) error {
	var prior string
	var ordinal uint64
	for facts, err := range receipts {
		if err != nil {
			return err
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		if err := checkRebuildCloneReceipt(facts, prior); err != nil {
			return err
		}
		prior = facts.File.FilePath
		ordinal++
		owner := rebuildCensusClaim{Receipt: prior, Language: facts.EffectiveLanguage}
		for _, n := range facts.Nodes {
			if n.ID == "" {
				return fmt.Errorf("empty node ID in %q", prior)
			}
			owner.RecordedPath = n.FilePath
			if err := claim(rebuildCensusKey{Node: n.ID}, owner, ordinal); err != nil {
				return err
			}
		}
		for _, e := range facts.Edges {
			owner.RecordedPath = e.FilePath
			if err := claim(rebuildCensusKey{Edge: graph.EdgeIdentityFor(e)}, owner, ordinal); err != nil {
				return err
			}
		}
	}
	return ctx.Err()
}

// The production count decides admission. Only a complete positive triggers
// test-only key and claimant replays, with separate logical byte budgets.
// The count itself keeps no claimant list and is not changed.
func diagnoseRebuildFrozenReceiptCensus(ctx context.Context, build *store_pebble.BuildStore,
	receipts iter.Seq2[store_pebble.ParseFacts, error],
) (store_pebble.RebuildShadowedIDStats, []rebuildCensusCollision, error) {
	stats, err := build.CountRebuildShadowedIDs(ctx, rebuildCloneClaimBytes, func(node func(string, uint64) error, edge func(graph.EdgeIdentity, uint64) error) error {
		return replayRebuildCensusClaims(ctx, receipts, func(key rebuildCensusKey, _ rebuildCensusClaim, ordinal uint64) error {
			if key.Node != "" {
				return node(key.Node, ordinal)
			}
			return edge(key.Edge, ordinal)
		})
	})
	if err != nil || stats.Shadowed+stats.ShadowedEdges == 0 {
		return stats, nil, err
	}
	// Find the exact collided keys without retaining paths for unique keys.
	// Reuse the production limit for key bytes, not a map-overhead estimate.
	first := make(map[rebuildCensusKey]uint64)
	collided := make(map[rebuildCensusKey][]rebuildCensusClaim)
	var used uint64
	err = replayRebuildCensusClaims(ctx, receipts, func(key rebuildCensusKey, _ rebuildCensusClaim, ordinal uint64) error {
		old, seen := first[key]
		if seen && (old == ordinal || old == 0) {
			return nil
		}
		// Lower than or equal to the encoded production key and value size.
		n := uint64(8 + len(key.Node) + len(key.Edge.From) + len(key.Edge.To) + len(key.Edge.Kind) + len(key.Edge.FilePath))
		if n > rebuildCloneClaimBytes-used {
			return fmt.Errorf("%w: collision diagnostic key budget", store_pebble.ErrRebuildLinkInputSize)
		}
		used += n
		if seen {
			first[key] = 0
			collided[key] = nil
		} else {
			first[key] = ordinal
		}
		return nil
	})
	if err != nil {
		return stats, nil, err
	}
	first = nil
	used = 0
	err = replayRebuildCensusClaims(ctx, receipts, func(key rebuildCensusKey, owner rebuildCensusClaim, _ uint64) error {
		owners, found := collided[key]
		if !found || (len(owners) != 0 && owners[len(owners)-1].Receipt == owner.Receipt) {
			return nil
		}
		// Bound the retained claimant report separately, before adding a row.
		n := uint64(64 + len(owner.Receipt) + len(owner.Language) + len(owner.RecordedPath))
		if n > rebuildCloneClaimBytes-used {
			return fmt.Errorf("%w: collision diagnostic claimant budget", store_pebble.ErrRebuildLinkInputSize)
		}
		used += n
		collided[key] = append(owners, owner)
		return nil
	})
	if err != nil {
		return stats, nil, err
	}
	rows := make([]rebuildCensusCollision, 0, len(collided))
	var nodes, edges uint64
	for key, owners := range collided {
		group := "edge:" + string(key.Edge.Kind)
		if key.Node != "" {
			nodes++
			group = "node:shared-recorded-path"
			for _, owner := range owners[1:] {
				if owner.RecordedPath != owners[0].RecordedPath {
					group = "node:distinct-recorded-paths"
					break
				}
			}
		} else {
			edges++
		}
		rows = append(rows, rebuildCensusCollision{Key: key, Group: group, Claimants: owners})
	}
	if nodes != stats.Shadowed || edges != stats.ShadowedEdges {
		return stats, nil, fmt.Errorf("collision diagnostic disagrees with census: nodes=%d/%d edges=%d/%d", nodes, stats.Shadowed, edges, stats.ShadowedEdges)
	}
	slices.SortFunc(rows, func(a, b rebuildCensusCollision) int {
		return cmp.Or(cmp.Compare(a.Group, b.Group), cmp.Compare(a.Key.Node, b.Key.Node),
			cmp.Compare(a.Key.Edge.From, b.Key.Edge.From), cmp.Compare(a.Key.Edge.To, b.Key.Edge.To),
			cmp.Compare(a.Key.Edge.FilePath, b.Key.Edge.FilePath), cmp.Compare(a.Key.Edge.Line, b.Key.Edge.Line))
	})
	return stats, rows, nil
}

func TestRebuildFrozenReceiptCollisionDiagnostic(t *testing.T) {
	first := &graph.Node{ID: "same::F", FilePath: "shared.go", Language: "go", Kind: graph.KindFunction, Name: "F"}
	second := &graph.Node{ID: "different::G", FilePath: "a.go", Language: "go", Kind: graph.KindFunction, Name: "G"}
	other := *second
	other.FilePath = "z.go"
	a := rebuildCloneClaimFixture("a.go", first)
	z := rebuildCloneClaimFixture("z.go", first)
	a.Nodes = append(a.Nodes, second)
	z.Nodes = append(z.Nodes, &other)
	a.File.NodeCount++
	z.File.NodeCount++
	call := &graph.Edge{From: first.ID, To: second.ID, Kind: graph.EdgeCalls, FilePath: "a.go", Line: 4}
	imp := *call
	imp.Kind = graph.EdgeImports
	otherSite := *call
	otherSite.Line++
	last := *call
	last.Origin = "later-payload"
	a.Edges = []*graph.Edge{call, &imp, &otherSite}
	z.Edges = []*graph.Edge{&last, &imp}
	build, _ := openRebuildTestFileStore(t)
	receipts := seedRebuildCloneInputReceipts(t, build, a, z)
	stats, groups, err := diagnoseRebuildFrozenReceiptCensus(t.Context(), build, receipts)
	require.NoError(t, err)
	require.EqualValues(t, 2, stats.Shadowed)
	require.EqualValues(t, 2, stats.ShadowedEdges)
	require.Len(t, groups, 4)
	require.Equal(t, rebuildCensusKey{Edge: graph.EdgeIdentityFor(call)}, groups[0].Key)
	require.Equal(t, rebuildCensusKey{Edge: graph.EdgeIdentityFor(&imp)}, groups[1].Key)
	require.Equal(t, "node:distinct-recorded-paths", groups[2].Group)
	require.Equal(t, second.ID, groups[2].Key.Node)
	require.Equal(t, "node:shared-recorded-path", groups[3].Group)
	require.Equal(t, first.ID, groups[3].Key.Node)
	for _, group := range groups {
		require.Len(t, group.Claimants, 2)
		require.Equal(t, "a.go", group.Claimants[0].Receipt)
		require.Equal(t, "z.go", group.Claimants[1].Receipt)
	}
}
