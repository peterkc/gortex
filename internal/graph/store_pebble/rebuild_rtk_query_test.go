package store_pebble

import (
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/zzet/gortex/internal/graph"
)

func rtkQueryStoreTestRules() RebuildRTKQueryRules {
	return RebuildRTKQueryRules{
		Node: func(n *graph.Node) (string, bool) {
			ep, _ := n.Meta["rtk_endpoint"].(string)
			hook, _ := n.Meta["rtk_generated_hook"].(bool)
			return ep, hook
		},
		Call: func(e *graph.Edge) (string, bool) {
			if e.Meta["via"] == "rtk-query" {
				return e.Meta["rtk_endpoint"].(string), true
			}
			if graph.IsUnresolvedTarget(e.To) {
				return graph.UnresolvedName(e.To), false
			}
			return "", false
		},
		Getter: func(n *graph.Node, binding string) bool {
			_, action := n.Meta["store_factory"]
			return n.Name == binding && n.FilePath != "" && !action
		},
		Rebind: func(e *graph.Edge, target string, matched bool) bool {
			if !matched || e.To == target {
				return false
			}
			e.To = target
			return true
		},
	}
}

func TestRebuildRTKQueryStationaryRawClaim(t *testing.T) {
	const file = "app.ts"
	caller := &graph.Node{ID: "caller", Name: "caller", Kind: graph.KindFunction, FilePath: file, Language: "typescript"}
	hook := &graph.Node{ID: "hook", Name: "useQuery", Kind: graph.KindFunction, FilePath: file, Language: "typescript",
		Meta: map[string]any{"rtk_generated_hook": true}}
	endpoint := &graph.Node{ID: "endpoint", Name: "query", Kind: graph.KindFunction, FilePath: file, Language: "typescript",
		Meta: map[string]any{"rtk_endpoint": "query"}}
	raw := &graph.Edge{From: caller.ID, To: "unresolved::useQuery", Kind: graph.EdgeCalls, FilePath: file, Line: 2}
	facts := ParseFacts{
		EffectiveLanguage: "typescript", File: graph.FileMetaRow{FilePath: file, NodeCount: 3},
		Nodes: []*graph.Node{caller, hook, endpoint}, Edges: []*graph.Edge{raw},
	}
	b := newBuild(t, FullProfile)
	require.NoError(t, b.AddParsedFile("", facts))
	alias := *raw
	alias.To = "resolved"
	require.NoError(t, b.ApplyLinkReindexes(t.Context(), []graph.EdgeReindex{{Edge: &alias, OldTo: raw.To}}))
	// Reuse the raw key while the already resolved Call retains its lineage.
	b.AddEdge(raw)
	before := rebuildFrameworkCustodyRows(t, b)
	stats, err := b.RebuildRTKQuery(t.Context(), func(yield func(ParseFacts, error) bool) { yield(facts, nil) }, rtkQueryStoreTestRules())
	require.ErrorIs(t, err, ErrRebuildRTKQuery)
	var refused *RebuildUnknownProvenanceError
	require.ErrorAs(t, err, &refused)
	want := graph.EdgeIdentityFor(raw)
	want.To = hook.ID
	require.Equal(t, &RebuildUnknownProvenanceError{Count: 1, First: want}, refused)
	require.Zero(t, stats)
	require.Equal(t, before, rebuildFrameworkCustodyRows(t, b))
}
