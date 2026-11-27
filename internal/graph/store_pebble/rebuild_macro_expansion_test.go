package store_pebble

import (
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/zzet/gortex/internal/graph"
)

func TestRebuildMacroExpansionProposalWinnerAndCustody(t *testing.T) {
	const file, language = "macro.c", "c"
	first := &graph.Node{ID: "a", Name: "ALPHA", Kind: graph.KindMacro, FilePath: file, Language: language}
	last := &graph.Node{ID: "z", Name: "OMEGA", Kind: graph.KindMacro, FilePath: file, Language: language}
	caller := &graph.Node{ID: "caller", Name: "caller", Kind: graph.KindFunction, FilePath: file, Language: language}
	callee := &graph.Node{ID: "callee", Name: "callee", Kind: graph.KindFunction, FilePath: file, Language: language}
	call := func(from, to string, line int) *graph.Edge {
		return &graph.Edge{From: from, To: to, Kind: graph.EdgeCalls, FilePath: file, Line: line}
	}
	incumbent := call(caller.ID, callee.ID, 11)
	incumbent.Meta = map[string]any{"macro": "older macro payload"}
	b, receipts := seedSyntheticCustody(t, ParseFacts{
		EffectiveLanguage: language,
		File:              graph.FileMetaRow{FilePath: file},
		Nodes:             []*graph.Node{first, last, caller, callee},
		Edges: []*graph.Edge{
			call(first.ID, callee.ID, 1), call(last.ID, callee.ID, 2),
			call(caller.ID, last.ID, 10), call(caller.ID, first.ID, 10),
			call(caller.ID, first.ID, 11), incumbent,
		},
	})
	rules := RebuildMacroExpansionRules{
		CFamily: func(lang string) bool { return lang == language },
		Macro:   func(n *graph.Node) bool { return n.Kind == graph.KindMacro },
		Body:    func(e *graph.Edge) string { return e.To },
		MayAdd: func(existing *graph.Edge) bool {
			return existing == nil || existing.Meta["macro"] != nil
		},
		Add: func(from, to, _ string, line int, name string) *graph.Edge {
			e := call(from, to, line)
			e.Meta = map[string]any{"macro": name}
			return e
		},
	}
	ctx := t.Context()
	require.NoError(t, b.FreezeMacroExpansionInputs(ctx, rules.Macro))
	stats, err := b.RebuildMacroExpansion(ctx, receipts, rules)
	require.NoError(t, err)
	require.Equal(t, RebuildMacroExpansionStats{Sites: 3, Owned: 3, Adds: 1}, stats)
	winner := rules.Add(caller.ID, callee.ID, file, 10, last.Name)
	saved, err := b.pendingEdge(pendingEdgeKey(winner))
	require.NoError(t, err)
	require.Equal(t, winner, saved)
	saved, err = b.pendingEdge(pendingEdgeKey(incumbent))
	require.NoError(t, err)
	require.Equal(t, incumbent, saved)
	requireSyntheticCustody(t, b, map[graph.EdgeIdentity]rebuildEdgeProof{
		graph.EdgeIdentityFor(winner): rebuildProofMacroExpansion,
	})
}
