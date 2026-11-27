package store_pebble

import (
	"strings"
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

func TestRebuildConventionFreezeAndRunGuards(t *testing.T) {
	b, receipts := seedSyntheticCustody(t)
	ctx := t.Context()
	rules := rebuildConventionTestRules()
	require.ErrorIs(t, b.FreezeFastAPIInputs(ctx, nil), ErrRebuildFastAPI)
	moves, err := b.RebuildFastAPI(ctx, receipts, rules)
	require.ErrorIs(t, err, ErrRebuildFastAPI)
	require.Zero(t, moves)
	match := func(graph.FrameworkCensusEdge) bool { return true }
	require.NoError(t, b.FreezeFastAPIInputs(ctx, match))
	require.ErrorIs(t, b.FreezeFastAPIInputs(ctx, match), ErrRebuildFastAPI)
	moves, err = b.RebuildFastAPI(ctx, receipts, rules)
	require.NoError(t, err)
	require.Zero(t, moves)
	require.NoError(t, b.FreezeFastAPIInputs(ctx, match))
	moves, err = b.RebuildFastAPI(ctx, nil, rules)
	require.ErrorIs(t, err, ErrRebuildFastAPI)
	require.Zero(t, moves)
}

func TestRebuildConventionKindAndWindowOrder(t *testing.T) {
	const file = "app.py"
	caller := &graph.Node{ID: "caller", Kind: graph.KindFunction, FilePath: file, Language: "python"}
	target := &graph.Node{ID: "target", Name: "load", Kind: graph.KindFunction, FilePath: file, Language: "python"}
	makeSite := func(to string, line int) *graph.Edge {
		return &graph.Edge{From: caller.ID, To: "unresolved::" + to, Kind: graph.EdgeCalls, FilePath: file, Line: line,
			Meta: map[string]any{"source": to}}
	}
	first, second := makeSite("z", 1), makeSite("a", 1)
	ignored := makeSite("other-kind", 2)
	ignored.Kind = graph.EdgeRendersChild
	edges := []*graph.Edge{first, ignored}
	for i := 1; i < graph.SQLiteReindexChunkSize; i++ {
		edges = append(edges, makeSite("fill", i+2))
	}
	edges = append(edges, second)
	b, receipts := seedSyntheticCustody(t, ParseFacts{
		EffectiveLanguage: "python",
		File:              graph.FileMetaRow{FilePath: file},
		Nodes:             []*graph.Node{caller, target},
		Edges:             edges,
	})
	rules := rebuildConventionTestRules()
	rules.Shape = func(string, string) ([]string, []string) { return []string{"load"}, nil }
	require.NoError(t, b.FreezeFastAPIInputs(t.Context(), func(graph.FrameworkCensusEdge) bool { return true }))
	moves, err := b.RebuildFastAPI(t.Context(), receipts, rules)
	require.NoError(t, err)
	require.EqualValues(t, graph.SQLiteReindexChunkSize+1, moves)
	want := *first
	want.To = target.ID
	moved, err := b.pendingEdge(pendingEdgeKey(&want))
	require.NoError(t, err)
	require.NotNil(t, moved)
	require.Equal(t, "z", moved.Meta["source"])
	for _, old := range []*graph.Edge{first, second} {
		row, err := b.pendingEdge(pendingEdgeKey(old))
		require.NoError(t, err)
		require.Nil(t, row)
	}
	row, err := b.pendingEdge(pendingEdgeKey(ignored))
	require.NoError(t, err)
	require.NotNil(t, row)
}

func TestRebuildConventionNodeCapsAndSingleReplay(t *testing.T) {
	const file = "app.py"
	padding := strings.Repeat("x", 16<<20)
	for _, oversized := range []string{"source", "candidate", "unrelated"} {
		t.Run(oversized, func(t *testing.T) {
			source := &graph.Node{ID: "source", Kind: graph.KindFunction, FilePath: file, Language: "python"}
			candidate := &graph.Node{ID: "candidate", Name: "load", Kind: graph.KindFunction, FilePath: file, Language: "python"}
			unrelated := &graph.Node{ID: "unrelated", Name: "other", Kind: graph.KindFunction, FilePath: file, Language: "python"}
			nodes := []*graph.Node{source, candidate, unrelated}
			for _, node := range nodes {
				if node.ID == oversized {
					node.Meta = map[string]any{"padding": padding}
				}
			}
			call := &graph.Edge{From: source.ID, To: "unresolved::load", Kind: graph.EdgeCalls, FilePath: file, Line: 1}
			ref := *call
			ref.Kind = graph.EdgeReferences
			b, receipts := seedSyntheticCustody(t, ParseFacts{
				EffectiveLanguage: "python",
				File:              graph.FileMetaRow{FilePath: file},
				Nodes:             nodes,
				Edges:             []*graph.Edge{&ref, call},
			})
			replays := 0
			once := func(yield func(ParseFacts, error) bool) {
				replays++
				receipts(yield)
			}
			require.NoError(t, b.FreezeFastAPIInputs(t.Context(), func(graph.FrameworkCensusEdge) bool { return true }))
			moves, err := b.RebuildFastAPI(t.Context(), once, rebuildConventionTestRules())
			if oversized == "unrelated" {
				require.NoError(t, err)
				require.EqualValues(t, 2, moves)
				require.Equal(t, 1, replays)
				return
			}
			require.ErrorIs(t, err, ErrRebuildLinkInputSize)
			require.Zero(t, moves)
			require.Zero(t, replays)
		})
	}
}
