package store_pebble

import (
	"strings"
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

func TestRebuildRTKQueryRowCaps(t *testing.T) {
	const file, binding = "app.ts", "useStore"
	large := strings.Repeat("x", 16<<20)
	for _, tc := range []struct {
		name, row string
	}{
		{"endpoint", "node row"},
		{"hook", "node row"},
		{"caller", "node row"},
		{"deciding getter", "node row"},
		{"unused getter", ""},
		{"foreign function", ""},
		{"owned Call", "edge row"},
		{"foreign resolved Call", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			caller := &graph.Node{ID: "caller", Name: "caller", Kind: graph.KindFunction, FilePath: file, Language: "typescript"}
			hook := &graph.Node{ID: "hook", Name: "useQuery", Kind: graph.KindFunction, FilePath: file, Language: "typescript",
				Meta: map[string]any{"rtk_generated_hook": true}}
			endpoint := &graph.Node{ID: "endpoint", Name: "query", Kind: graph.KindFunction, FilePath: file, Language: "typescript",
				Meta: map[string]any{"rtk_endpoint": "query"}}
			getter := &graph.Node{ID: "getter", Name: binding, Kind: graph.KindVariable, FilePath: file, Language: "typescript"}
			foreign := &graph.Node{ID: "foreign", Name: "unrelated", Kind: graph.KindFunction, FilePath: file, Language: "typescript"}
			call := &graph.Edge{From: caller.ID, To: "unresolved::useQuery", Kind: graph.EdgeCalls, FilePath: file, Line: 2,
				Meta: map[string]any{"store_binding": binding}}
			resolved := &graph.Edge{From: caller.ID, To: foreign.ID, Kind: graph.EdgeCalls, FilePath: file, Line: 3}
			nodes := []*graph.Node{caller, hook, endpoint, getter, foreign}
			var oversized *graph.Node
			switch tc.name {
			case "endpoint":
				oversized = endpoint
			case "hook":
				oversized = hook
			case "caller":
				oversized = caller
			case "deciding getter", "unused getter":
				oversized = getter
			case "foreign function":
				oversized = foreign
			case "owned Call":
				call.Context = large
			case "foreign resolved Call":
				resolved.Context = large
			}
			if oversized != nil {
				if oversized.Meta == nil {
					oversized.Meta = map[string]any{}
				}
				oversized.Meta["large"] = large
			}
			facts := ParseFacts{
				EffectiveLanguage: "typescript", File: graph.FileMetaRow{FilePath: file, NodeCount: len(nodes)},
				Nodes: nodes, Edges: []*graph.Edge{call, resolved},
			}
			b := newBuild(t, FullProfile)
			require.NoError(t, b.AddParsedFile("", facts))
			if tc.name == "deciding getter" {
				rival := *hook
				rival.ID, rival.FilePath = "rival", "other.ts"
				rivalFacts := ParseFacts{
					EffectiveLanguage: "typescript", File: graph.FileMetaRow{FilePath: rival.FilePath, NodeCount: 1},
					Nodes: []*graph.Node{&rival},
				}
				require.NoError(t, b.AddParsedFile("", rivalFacts))
			}
			before := rebuildFrameworkCustodyRows(t, b)
			stats, err := b.RebuildRTKQuery(t.Context(), func(yield func(ParseFacts, error) bool) {
				require.Empty(t, tc.row, "oversized stage-owned row must refuse before receipt replay")
				yield(facts, nil)
			}, rtkQueryStoreTestRules())
			if tc.row != "" {
				require.ErrorIs(t, err, ErrRebuildRTKQuery)
				require.ErrorIs(t, err, ErrRebuildLinkInputSize)
				require.ErrorContains(t, err, "RTK Query "+tc.row)
				require.Zero(t, stats)
				require.Equal(t, before, rebuildFrameworkCustodyRows(t, b))
				return
			}
			require.NoError(t, err)
			require.Equal(t, RebuildRTKQueryStats{Resolved: 1, Moves: 1}, stats)
			moved := *call
			moved.To = hook.ID
			stored, err := b.pendingEdge(pendingEdgeKey(&moved))
			require.NoError(t, err)
			require.Equal(t, &moved, stored)
		})
	}
}

func TestRebuildRTKQueryGetterInCallerFile(t *testing.T) {
	const file, other, binding = "app.ts", "other.ts", "useStore"
	for _, getterReceipt := range []bool{false, true} {
		name := "missing getter receipt"
		if getterReceipt {
			name = "exact getter receipt"
		}
		t.Run(name, func(t *testing.T) {
			caller := &graph.Node{ID: "caller", Name: "caller", Kind: graph.KindFunction, FilePath: file, Language: "typescript"}
			hook := &graph.Node{ID: "hook", Name: "useQuery", Kind: graph.KindFunction, FilePath: file, Language: "typescript",
				Meta: map[string]any{"rtk_generated_hook": true}}
			rival := *hook
			rival.ID, rival.FilePath = "rival", other
			endpoint := &graph.Node{ID: "endpoint", Name: "query", Kind: graph.KindFunction, FilePath: file, Language: "typescript",
				Meta: map[string]any{"rtk_endpoint": "query"}}
			getter := &graph.Node{ID: "getter", Name: binding, Kind: graph.KindVariable, FilePath: file, Language: "typescript"}
			call := &graph.Edge{From: caller.ID, To: "unresolved::useQuery", Kind: graph.EdgeCalls, FilePath: file, Line: 2,
				Meta: map[string]any{"store_binding": binding}}
			app := ParseFacts{
				EffectiveLanguage: "typescript",
				File:              graph.FileMetaRow{FilePath: file, NodeCount: 4},
				Nodes:             []*graph.Node{caller, hook, endpoint, getter},
				Edges:             []*graph.Edge{call},
			}
			elsewhere := ParseFacts{
				EffectiveLanguage: "typescript",
				File:              graph.FileMetaRow{FilePath: other, NodeCount: 1},
				Nodes:             []*graph.Node{&rival},
			}
			b := newBuild(t, FullProfile)
			require.NoError(t, b.AddParsedFile("", app))
			require.NoError(t, b.AddParsedFile("", elsewhere))
			before := rebuildFrameworkCustodyRows(t, b)

			receiptCaller := *caller
			receiptCaller.FilePath = other
			elsewhere.Nodes = []*graph.Node{&rival, &receiptCaller}
			elsewhere.File.NodeCount = len(elsewhere.Nodes)
			app.Nodes = []*graph.Node{hook, endpoint}
			if getterReceipt {
				app.Nodes = append(app.Nodes, getter)
			}
			app.File.NodeCount = len(app.Nodes)
			stats, err := b.RebuildRTKQuery(t.Context(), func(yield func(ParseFacts, error) bool) {
				if yield(app, nil) {
					yield(elsewhere, nil)
				}
			}, rtkQueryStoreTestRules())
			moved := *call
			moved.To = hook.ID
			if !getterReceipt {
				require.ErrorIs(t, err, ErrRebuildRTKQuery)
				var refused *RebuildUnknownProvenanceError
				require.ErrorAs(t, err, &refused)
				require.Equal(t, &RebuildUnknownProvenanceError{Count: 1, First: graph.EdgeIdentityFor(&moved)}, refused)
				require.Zero(t, stats)
				require.Equal(t, before, rebuildFrameworkCustodyRows(t, b))
				return
			}
			require.NoError(t, err)
			require.Equal(t, RebuildRTKQueryStats{Resolved: 1, Moves: 1}, stats)
			stored, err := b.pendingEdge(pendingEdgeKey(&moved))
			require.NoError(t, err)
			require.Equal(t, &moved, stored)
			old, err := b.pendingEdge(pendingEdgeKey(call))
			require.NoError(t, err)
			require.Nil(t, old)
		})
	}
}
