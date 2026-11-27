package store_pebble

import (
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/zzet/gortex/internal/graph"
)

func TestRebuildSidekiqNeedsAnchorForEmptyPool(t *testing.T) {
	const file = "jobs.rb"
	worker := &graph.Node{ID: "worker", Name: "perform", Kind: graph.KindMethod, FilePath: file, Language: "ruby",
		Meta: map[string]any{"worker": "OtherJob"}}
	call := &graph.Edge{From: "caller", To: "settled", Kind: graph.EdgeCalls, FilePath: file, Line: 1}
	for _, withAnchor := range []bool{false, true} {
		facts := ParseFacts{EffectiveLanguage: "ruby", File: graph.FileMetaRow{FilePath: file},
			Nodes: []*graph.Node{worker}, Edges: []*graph.Edge{call}}
		b, receipts := seedSyntheticCustody(t, facts)
		before := rebuildFrameworkCustodyRows(t, b)
		if !withAnchor {
			receipts = func(yield func(ParseFacts, error) bool) {
				facts.Nodes = nil
				yield(facts, nil)
			}
		}
		rules := RebuildSidekiqRules{
			Worker: func(n *graph.Node) string {
				name, _ := n.Meta["worker"].(string)
				return name
			},
			Stub:   func(*graph.Edge) string { return "MissingJob" },
			Simple: func(s string) string { return s },
			Getter: func(*graph.Node, string) bool { return false },
			Rebind: func(e *graph.Edge, id string, matched bool) bool {
				if !matched {
					id = "unresolved::*.perform"
				}
				if e.To == id {
					return false
				}
				e.To = id
				return true
			},
		}
		want := *call
		want.To = "unresolved::*.perform"
		stats, err := b.RebuildSidekiq(t.Context(), receipts, rules)
		if withAnchor {
			require.NoError(t, err)
			require.Equal(t, RebuildSidekiqStats{Stubs: 1, Moves: 1}, stats)
			moved, err := b.pendingEdge(pendingEdgeKey(&want))
			require.NoError(t, err)
			require.Equal(t, &want, moved)
			continue
		}
		var refused *RebuildUnknownProvenanceError
		require.ErrorAs(t, err, &refused)
		require.Equal(t, &RebuildUnknownProvenanceError{Count: 1, First: graph.EdgeIdentityFor(&want)}, refused)
		require.Zero(t, stats)
		require.Equal(t, before, rebuildFrameworkCustodyRows(t, b))
	}
}
