package store_pebble

import (
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/zzet/gortex/internal/graph"
)

func TestRebuildRailsUsesEFCoreModelProof(t *testing.T) {
	const file = "route.rb"
	caller := &graph.Node{ID: "caller", Name: "route", Kind: graph.KindFunction, Language: "ruby", FilePath: file}
	model := &graph.Node{ID: "model", Name: "Invoice", Kind: graph.KindType, Language: "csharp", FilePath: "Model.cs"}
	table := &graph.Node{ID: "table::invoice", Name: "invoice", Kind: graph.KindTable}
	call := &graph.Edge{From: caller.ID, To: "unresolved::*.find", Kind: graph.EdgeCalls, FilePath: file, Line: 2}
	modelEdge := &graph.Edge{From: model.ID, To: table.ID, Kind: graph.EdgeModelsTable, FilePath: model.FilePath, Line: 1}
	rules := RebuildRailsRules{
		Call:      func(*graph.Edge) (string, string) { return model.Name, "find" },
		Source:    func(path string) bool { return path == file },
		Owner:     func(e *graph.Edge) string { return e.To },
		Candidate: func(n *graph.Node) bool { return n.Kind == graph.KindType },
		Preferred: func(string, string) bool { return false },
		Directory: func(string) string { return "" },
		Model:     func(string) bool { return true },
		Rebind: func(e *graph.Edge, target string) graph.EdgeReindex {
			oldTo := e.To
			e.To = target
			return graph.EdgeReindex{Edge: e, OldTo: oldTo}
		},
	}
	for _, tc := range []struct {
		name  string
		proof rebuildEdgeProof
	}{
		{"ef core proof", rebuildProofEFCoreModel},
		{"untracked move", rebuildProofUntrackedMove},
	} {
		t.Run(tc.name, func(t *testing.T) {
			b, receipts := seedSyntheticCustody(t, ParseFacts{
				EffectiveLanguage: "ruby",
				File:              graph.FileMetaRow{FilePath: file},
				Nodes:             []*graph.Node{caller, table},
				Edges:             []*graph.Edge{call},
			}, ParseFacts{
				EffectiveLanguage: "csharp",
				File:              graph.FileMetaRow{FilePath: model.FilePath},
				Nodes:             []*graph.Node{model},
			})
			require.NoError(t, b.withWrites(func() error {
				return b.addEdgeWithCustody(modelEdge, rebuildEdgeCustody{proof: tc.proof})
			}))
			before := rebuildFrameworkCustodyRows(t, b)
			ctx := t.Context()
			require.NoError(t, b.FreezeRailsInputs(ctx, func(*graph.Edge) bool { return true }))
			stats, err := b.RebuildRails(ctx, receipts, rules)
			if tc.proof == rebuildProofUntrackedMove {
				require.ErrorIs(t, err, ErrRebuildUnknownProvenance)
				require.Zero(t, stats)
				require.Equal(t, before, rebuildFrameworkCustodyRows(t, b))
				return
			}
			require.NoError(t, err)
			require.Equal(t, RebuildRailsStats{Stubs: 1, Moves: 1}, stats)
			moved := *call
			moved.To = model.ID
			custody, err := readRebuildEdgeCustody(b.pending, pendingEdgeKey(&moved))
			require.NoError(t, err)
			require.Equal(t, rebuildEdgeCustody{raw: pendingEdgeKey(call)}, custody)
		})
	}
}
