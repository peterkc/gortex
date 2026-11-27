package store_pebble

import (
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/zzet/gortex/internal/graph"
)

func TestRebuildEFCoreModelProducerCustodyOnly(t *testing.T) {
	ctx := t.Context()
	const file = "Models.cs"
	fresh := &graph.Node{ID: "fresh", Name: "Fresh", Kind: graph.KindType, Language: "csharp", FilePath: file}
	occupied := &graph.Node{ID: "occupied", Name: "Occupied", Kind: graph.KindType, Language: "csharp", FilePath: file}
	table := &graph.Node{ID: "table::model", Name: "model", Kind: graph.KindTable}
	proposal := func(n *graph.Node) *graph.Edge {
		return &graph.Edge{From: n.ID, To: table.ID, Kind: graph.EdgeModelsTable, FilePath: file, Line: 1,
			Meta: map[string]any{"witness": "projected model"}}
	}
	incumbent := proposal(occupied)
	incumbent.Meta = map[string]any{"witness": "incumbent"}
	facts := ParseFacts{
		EffectiveLanguage: "csharp",
		File:              graph.FileMetaRow{FilePath: file},
		Nodes:             []*graph.Node{fresh, occupied, table},
	}
	seed := func(b *BuildStore) {
		t.Helper()
		require.NoError(t, b.withWrites(func() error {
			return b.addEdgeWithCustody(incumbent, rebuildEdgeCustody{proof: rebuildProofDirect})
		}))
	}
	actual, receipts := seedSyntheticCustody(t, facts)
	prior, _ := seedSyntheticCustody(t, facts)
	seed(actual)
	seed(prior)
	freshEdge := proposal(fresh)
	// The same saved row written through the old custody path.
	require.NoError(t, prior.withWrites(func() error {
		return prior.addEdgeWithCustody(freshEdge, rebuildEdgeCustody{proof: rebuildProofUntrackedMove})
	}))

	changed, err := actual.RebuildEFCoreModels(ctx, receipts, RebuildEFCoreRules{
		Fact:     func(*graph.Node) (bool, bool) { return true, true },
		SameFact: func(a, b *graph.Node) bool { return a.ID == b.ID },
		Owned:    func(*graph.Edge) bool { return false },
		SameEdge: func(a, b *graph.Edge) bool { return graph.EdgeIdentityFor(a) == graph.EdgeIdentityFor(b) },
		Project: func(nodes []*graph.Node) func(*graph.Node, []*graph.Edge) (*graph.Edge, bool) {
			return func(n *graph.Node, _ []*graph.Edge) (*graph.Edge, bool) {
				for _, fact := range nodes {
					if fact.ID == n.ID {
						return proposal(n), true
					}
				}
				return nil, false
			}
		},
		Table: func(*graph.Node, *graph.Edge) *graph.Node { return nil },
	})
	require.NoError(t, err)
	require.Equal(t, uint64(2), changed)
	key := pendingEdgeKey(freshEdge)
	proof, err := readRebuildEdgeCustody(actual.pending, key)
	require.NoError(t, err)
	require.Equal(t, rebuildEdgeCustody{proof: rebuildProofEFCoreModel}, proof)
	want := rebuildFrameworkCustodyRows(t, prior)
	got := rebuildFrameworkCustodyRows(t, actual)
	recorder := string(rebuildLinkRecorderKey(key))
	require.NotEqual(t, want[recorder], got[recorder])
	delete(got, recorder)
	delete(want, recorder)
	require.Equal(t, want, got, "all row bytes apart from the new producer marker")
}
