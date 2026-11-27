package store_pebble

import (
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/zzet/gortex/internal/graph"
)

func TestRebuildFnPointerStageMoveProof(t *testing.T) {
	const file, lang = "saved.c", "c"
	caller := &graph.Node{ID: "caller", Name: "caller", Kind: graph.KindFunction, FilePath: file, Language: lang}
	target := &graph.Node{ID: "target", Name: "target", Kind: graph.KindFunction, FilePath: file, Language: lang}
	reg := &graph.Edge{From: caller.ID, To: "unresolved::target", Kind: graph.EdgeReferences, FilePath: file, Line: 1}
	call := &graph.Edge{From: caller.ID, To: "unresolved::*.fn", Kind: graph.EdgeCalls, FilePath: file, Line: 2}
	facts := ParseFacts{
		EffectiveLanguage: lang,
		File:              graph.FileMetaRow{FilePath: file},
		Nodes:             []*graph.Node{caller, target},
		Edges:             []*graph.Edge{reg, call},
	}
	bind := func(e *graph.Edge, to string) bool {
		if e.To == to {
			return false
		}
		e.To = to
		return true
	}
	rules := RebuildFnPointerStageRules{
		CFamily:      func(language string) bool { return language == lang },
		Registration: func(*graph.Edge) (string, string, string) { return "slot\x00fn", "", target.Name },
		Dispatch:     func(*graph.Edge) (string, string, bool) { return "slot", "fn", true },
		Callable:     func(n *graph.Node) bool { return n.Kind == graph.KindFunction },
		Getter:       func(*graph.Node, string) bool { return false },
		BindRef:      bind,
		BindCall:     func(e *graph.Edge, to string, matched bool) bool { return matched && bind(e, to) },
		Fanout:       func(e *graph.Edge, _, _, _ string) *graph.Edge { return e },
		Sweeps:       8,
		Cap:          64,
	}
	for _, tc := range []struct {
		name string
		omit *graph.Edge
	}{
		{"moving registration without receipt", reg},
		{"moving dispatch without receipt", call},
		{"both receipts", nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := t.Context()
			b, _ := seedSyntheticCustody(t, facts)
			snapshot := func() map[string]string {
				rows := map[string]string{}
				for _, prefix := range [][]byte{{familyNode}, {familyEdge}, rebuildLinkRecorderPrefix} {
					require.NoError(t, pendingVisit(ctx, b.pending, prefix, func(r row) error {
						rows[string(r.key)] = string(r.value)
						return nil
					}))
				}
				return rows
			}
			before := snapshot()
			require.NoError(t, b.FreezeFnPointerInputs(ctx, func(*graph.Edge) bool { return true }))
			receipt := facts
			receipt.Edges = nil
			for _, e := range facts.Edges {
				if e != tc.omit {
					receipt.Edges = append(receipt.Edges, e)
				}
			}
			stats, err := b.RebuildFnPointerStage(ctx, func(yield func(ParseFacts, error) bool) { yield(receipt, nil) }, rules)
			if tc.omit != nil {
				var refused *RebuildUnknownProvenanceError
				require.ErrorIs(t, err, ErrRebuildFnPointer)
				require.ErrorAs(t, err, &refused)
				want := graph.EdgeIdentityFor(tc.omit)
				want.To = target.ID
				require.Equal(t, want, refused.First)
				require.EqualValues(t, 1, refused.Count)
				require.Zero(t, stats)
				require.Equal(t, before, snapshot())
				return
			}
			require.NoError(t, err)
			require.Equal(t, RebuildFnPointerStageStats{RefMoves: 1, CallMoves: 1, Resolved: 1, Sweeps: 1}, stats)
			for _, original := range facts.Edges {
				raw := pendingEdgeKey(original)
				old, err := b.pendingEdge(raw)
				require.NoError(t, err)
				require.Nil(t, old)
				move := *original
				move.To = target.ID
				live := pendingEdgeKey(&move)
				saved, err := b.pendingEdge(live)
				require.NoError(t, err)
				require.Equal(t, &move, saved)
				custody, err := readRebuildEdgeCustody(b.pending, live)
				require.NoError(t, err)
				require.Equal(t, rebuildEdgeCustody{raw: raw}, custody)
			}
		})
	}
}
