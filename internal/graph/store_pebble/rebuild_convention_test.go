package store_pebble

import (
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/zzet/gortex/internal/graph"
)

func TestRebuildConventionNonMovingClaim(t *testing.T) {
	const file = "app.py"
	source := &graph.Node{ID: "caller", Kind: graph.KindFunction, FilePath: file, Language: "python"}
	target := &graph.Node{ID: "target", Name: "load", Kind: graph.KindFunction, FilePath: file, Language: "python"}
	raw := &graph.Edge{From: source.ID, To: "unresolved::load", Kind: graph.EdgeCalls, FilePath: file, Line: 1}
	for _, shared := range []bool{false, true} {
		b, receipts := seedSyntheticCustody(t, ParseFacts{
			EffectiveLanguage: "python",
			File:              graph.FileMetaRow{FilePath: file},
			Nodes:             []*graph.Node{source, target},
			Edges:             []*graph.Edge{raw},
		})
		if shared {
			settled := *raw
			settled.To = "settled"
			require.NoError(t, b.withWrites(func() error {
				return b.addEdgeWithCustody(&settled, rebuildEdgeCustody{raw: pendingEdgeKey(raw)})
			}))
		}
		before := rebuildFrameworkCustodyRows(t, b)
		require.NoError(t, b.FreezeFastAPIInputs(t.Context(), func(graph.FrameworkCensusEdge) bool { return true }))
		moves, err := b.RebuildFastAPI(t.Context(), receipts, rebuildConventionTestRules())
		if shared {
			var refused *RebuildUnknownProvenanceError
			require.ErrorAs(t, err, &refused)
			require.Equal(t, &RebuildUnknownProvenanceError{Count: 1, First: graph.EdgeIdentityFor(raw)}, refused)
			require.Zero(t, moves)
			require.Equal(t, before, rebuildFrameworkCustodyRows(t, b))
			continue
		}
		require.NoError(t, err)
		require.EqualValues(t, 1, moves)
	}
}

func rebuildConventionTestRules() RebuildConventionRules {
	return RebuildConventionRules{
		Shape:    func(target, _ string) ([]string, []string) { return []string{graph.UnresolvedName(target)}, nil },
		Source:   func(string) bool { return true },
		Eligible: func(*graph.Node) bool { return true },
		Rank:     func(string, string, []string) int { return 3 },
		Rebind: func(e *graph.Edge, id string, confidence float64) graph.EdgeReindex {
			old := e.To
			e.To, e.Confidence = id, confidence
			return graph.EdgeReindex{Edge: e, OldTo: old}
		},
	}
}

func TestRebuildConventionReceiptDecisionFields(t *testing.T) {
	const file = "app.py"
	source := &graph.Node{ID: "caller", Kind: graph.KindFunction, FilePath: file, Language: "python"}
	target := &graph.Node{ID: "target", Name: "load", Kind: graph.KindFunction, FilePath: "deps/load.py", Language: "python"}
	call := &graph.Edge{From: source.ID, To: "unresolved::load", Kind: graph.EdgeCalls, FilePath: file, Line: 1,
		Meta: map[string]any{"via": "live"}}
	for _, tc := range []struct {
		name, via, owner string
		refuse           bool
	}{
		{"same native decision", "raw", file, false},
		{"shape directories differ", "changed", file, true},
		{"wrong receipt owner", "raw", "other.py", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			facts := ParseFacts{EffectiveLanguage: "python", File: graph.FileMetaRow{FilePath: file},
				Nodes: []*graph.Node{source}, Edges: []*graph.Edge{call}}
			targetFacts := producerProofFacts(target)
			b, _ := seedSyntheticCustody(t, facts, targetFacts)
			before := rebuildFrameworkCustodyRows(t, b)
			raw := *call
			raw.Meta = map[string]any{"via": tc.via}
			rawTarget := *target
			rawTarget.FilePath = "deps/renamed.py"
			rawOwner := ParseFacts{EffectiveLanguage: "python", File: graph.FileMetaRow{FilePath: tc.owner}, Edges: []*graph.Edge{&raw}}
			facts.Edges = nil
			receipts := func(yield func(ParseFacts, error) bool) {
				for _, f := range []ParseFacts{facts, producerProofFacts(&rawTarget), rawOwner} {
					if !yield(f, nil) {
						return
					}
				}
			}
			rules := rebuildConventionTestRules()
			rules.Shape = func(target, via string) ([]string, []string) {
				dirs := []string{"deps"}
				if via == "changed" {
					dirs = []string{"routes"}
				}
				return []string{graph.UnresolvedName(target)}, dirs
			}
			require.NoError(t, b.FreezeFastAPIInputs(t.Context(), func(graph.FrameworkCensusEdge) bool { return true }))
			moves, err := b.RebuildFastAPI(t.Context(), receipts, rules)
			if tc.refuse {
				require.ErrorIs(t, err, ErrRebuildUnknownProvenance)
				require.Zero(t, moves)
				require.Equal(t, before, rebuildFrameworkCustodyRows(t, b))
				return
			}
			require.NoError(t, err)
			require.EqualValues(t, 1, moves)
			want := *call
			want.To, want.Confidence = target.ID, 0.9
			edge, err := b.pendingEdge(pendingEdgeKey(&want))
			require.NoError(t, err)
			require.NotNil(t, edge)
			require.Equal(t, 0.9, edge.Confidence)
			require.Equal(t, "live", edge.Meta["via"])
		})
	}
}
