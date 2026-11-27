package store_pebble

import (
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/zzet/gortex/internal/graph"
)

func TestRebuildMediatRRefusesUnprovedSyntheticRepeat(t *testing.T) {
	const file = "dispatch.cs"
	handler := func(id string) *graph.Node {
		return &graph.Node{ID: id, Kind: graph.KindMethod, Name: "Handle", FilePath: file, Language: "csharp"}
	}
	first, second := handler("a"), handler("b")
	original := &graph.Edge{From: "dispatch", To: "unresolved::*.Handle", Kind: graph.EdgeCalls, FilePath: file, Line: 7}
	b, receipts := seedSyntheticCustody(t, producerProofFacts(first), producerProofFacts(second), ParseFacts{
		EffectiveLanguage: "csharp", File: graph.FileMetaRow{FilePath: file}, Edges: []*graph.Edge{original},
	})
	rules := RebuildMediatRRules{
		Handler: func(*graph.Node) (string, string) { return "notification", "Message" },
		Call:    func(*graph.Edge) (string, string) { return "notification", "Message" },
		Getter:  func(*graph.Node, string) bool { return false },
		Rebind: func(e *graph.Edge, id string, _ bool) bool {
			if e.To == id {
				return false
			}
			e.To = id
			return true
		},
		Fanout: func(e *graph.Edge, id, _ string) *graph.Edge {
			n := *e
			n.To = id
			return &n
		},
	}
	stats, err := b.RebuildMediatR(t.Context(), receipts, rules)
	require.NoError(t, err)
	require.Equal(t, RebuildMediatRStats{Stubs: 1, Resolved: 2, Moves: 1, Adds: 1}, stats)
	primary, secondary := *original, *original
	primary.To, secondary.To = first.ID, second.ID
	raw, err := readRebuildEdgeCustody(b.pending, pendingEdgeKey(&primary))
	require.NoError(t, err)
	require.Equal(t, rebuildEdgeCustody{raw: pendingEdgeKey(original)}, raw)
	proof, err := readRebuildEdgeCustody(b.pending, pendingEdgeKey(&secondary))
	require.NoError(t, err)
	require.Equal(t, rebuildEdgeCustody{proof: rebuildProofMediatRFanout}, proof)

	before := rebuildFrameworkCustodyRows(t, b)
	stats, err = b.RebuildMediatR(t.Context(), receipts, rules)
	require.ErrorIs(t, err, ErrRebuildMediatR)
	var refusal *RebuildUnknownProvenanceError
	require.ErrorAs(t, err, &refusal)
	require.Equal(t, &RebuildUnknownProvenanceError{Count: 1, First: graph.EdgeIdentityFor(&primary)}, refusal)
	require.Zero(t, stats)
	require.Equal(t, before, rebuildFrameworkCustodyRows(t, b))
}
