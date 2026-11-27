package store_pebble

import (
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/zzet/gortex/internal/graph"
)

func TestRebuildGoReceiverOrderWitness(t *testing.T) {
	const file, types = "pkg/m.go", "pkg/t.go"
	target := &graph.Node{ID: types + "::T", Name: "T", Kind: graph.KindType, FilePath: types, Language: "go"}
	method := &graph.Node{ID: file + "::T.Handle", Name: "Handle", Kind: graph.KindMethod, FilePath: file, Language: "go"}
	raw := &graph.Edge{From: method.ID, To: file + "::T", Kind: graph.EdgeMemberOf, FilePath: file, Line: 2}
	loser, incumbent := *raw, *raw
	loser.To = "pkg/plugin.go::T"
	incumbent.To, incumbent.Line = target.ID, 3
	blocked := *raw
	blocked.Line = incumbent.Line
	live := *raw
	live.To = target.ID

	facts := ParseFacts{Nodes: []*graph.Node{target, method}, Edges: []*graph.Edge{raw, &loser, &blocked, &incumbent}}
	tracked, receipts := seedSyntheticCustody(t, facts)
	control, _ := seedSyntheticCustody(t, facts)
	moved, err := tracked.RebuildGoMethodReceivers(t.Context(), receipts)
	require.NoError(t, err)
	require.Equal(t, 3, moved)
	for _, old := range []*graph.Edge{raw, &loser, &blocked} {
		moved := *old
		moved.To = target.ID
		require.NoError(t, control.ApplyLinkReindexes(t.Context(), []graph.EdgeReindex{{Edge: &moved, OldTo: old.To}}))
	}
	for _, prefix := range [][]byte{{familyNode}, {familyEdge, edgeOut}, {familyEdge, edgeIn}, rebuildLinkRecorderPrefix} {
		rows := func(b *BuildStore) [][2]string {
			var rows [][2]string
			require.NoError(t, pendingVisit(t.Context(), b.pending, prefix, func(r row) error {
				rows = append(rows, [2]string{string(r.key), string(r.value)})
				return nil
			}))
			return rows
		}
		require.Equal(t, rows(control), rows(tracked))
	}
	witness, err := pendingGet(tracked.pending, rebuildReceiverOrderKey(pendingEdgeKey(&live)), copyValue)
	require.NoError(t, err)
	require.Equal(t, pendingEdgeKey(raw), witness)
	witness, err = pendingGet(tracked.pending, rebuildReceiverOrderKey(pendingEdgeKey(&incumbent)), copyValue)
	require.NoError(t, err)
	require.Nil(t, witness, "occupied destination must not acquire receiver order")
}
