package store_pebble

import (
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/zzet/gortex/internal/graph"
)

func TestRebuildTemporalWinnerCustody(t *testing.T) {
	const file, caller, target = "workflow.go", "caller", "activity"
	b, err := OpenBuild(filepath.Join(t.TempDir(), "working"), FullProfile)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, b.Close()) })
	ctx := t.Context()
	call := func(to string, line int) *graph.Edge {
		return &graph.Edge{From: caller, To: to, Kind: graph.EdgeCalls, FilePath: file, Line: line}
	}
	raw, prior, direct := call("unresolved::raw", 1), call("unresolved::prior", 1), call("unresolved::direct", 2)
	trigger := call("unresolved::wrapper", 3)
	shadow := call("unresolved::shadow", 3)
	keeper := call(target, 5)
	keeper.Meta = map[string]any{"keeper": true}
	facts := ParseFacts{
		EffectiveLanguage: "go", File: graph.FileMetaRow{FilePath: file, NodeCount: 2},
		Nodes: []*graph.Node{
			{ID: caller, Name: "Run", Kind: graph.KindFunction, Language: "go", FilePath: file},
			{ID: target, Name: "Activity", Kind: graph.KindFunction, Language: "go", FilePath: file},
		},
		Edges: []*graph.Edge{raw, direct, trigger, shadow, keeper},
	}
	require.NoError(t, b.AddParsedFile("", facts))
	require.NoError(t, b.ApplyLinkReindexes(ctx, []graph.EdgeReindex{{Edge: prior, OldTo: raw.To}}))
	// Give the shadowed live key real raw custody. The staged payload at
	// that key must never borrow it, even if it displaces the durable row.
	shadowed := call("unresolved::staged", 3)
	require.NoError(t, b.ApplyLinkReindexes(ctx, []graph.EdgeReindex{{Edge: shadowed, OldTo: shadow.To}}))
	_, err = b.RebuildTemporal(ctx, func(yield func(ParseFacts, error) bool) { yield(facts, nil) }, 1,
		func(p *RebuildTemporalPass) (RebuildTemporalStats, error) {
			for _, e := range []*graph.Edge{prior, direct, trigger} {
				require.NoError(t, p.WantRecorder(ctx, e))
			}
			require.NoError(t, p.WantNode(ctx, caller))
			require.NoError(t, p.WantNode(ctx, target))
			require.NoError(t, p.QueueMove(ctx, prior.To, call(target, 1), false))
			require.NoError(t, p.QueueMove(ctx, direct.To, call(target, 2), false))
			require.NoError(t, p.QueueMove(ctx, shadowed.To, call(target, 3), true))
			require.NoError(t, p.QueueAdd(ctx, call(target, 3))) // duplicate of the staged move
			require.NoError(t, p.QueueAdd(ctx, call(target, 4)))
			require.NoError(t, p.QueueAdd(ctx, call(target, 5))) // loses to the durable keeper
			return RebuildTemporalStats{}, nil
		})
	require.NoError(t, err)
	for _, tc := range []struct {
		line  int
		raw   *graph.Edge
		proof rebuildEdgeProof
	}{
		{1, raw, 0}, {2, direct, 0}, {3, nil, rebuildProofTemporalSynthetic},
		{4, nil, rebuildProofTemporalSynthetic}, {5, nil, 0},
	} {
		key := pendingEdgeKey(call(target, tc.line))
		stored, err := b.pendingEdge(key)
		require.NoError(t, err)
		require.NotNil(t, stored)
		got, err := readRebuildEdgeCustody(b.pending, key)
		require.NoError(t, err)
		require.Equal(t, tc.proof, got.proof)
		if tc.raw != nil {
			require.Equal(t, pendingEdgeKey(tc.raw), got.raw)
		} else {
			require.Empty(t, got.raw)
		}
		if tc.line == keeper.Line {
			require.Equal(t, true, stored.Meta["keeper"])
		}
	}
	assertCustodyRemovalLeavesWorkingRows(t, b, 4)
}
