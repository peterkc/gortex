package store_pebble

import (
	"iter"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/zzet/gortex/internal/graph"
)

func TestRebuildExternalCallOptionalCreator(t *testing.T) {
	const packageID = "external-call::dep::example.test/pkg"
	for _, tc := range []struct {
		name     string
		second   string
		existing bool
		refuse   bool
	}{
		{name: "later different creator", second: "dart", refuse: true},
		{name: "same language frontier", second: "python"},
		{name: "preexisting module", second: "dart", existing: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			b := newBuild(t, FullProfile)
			nodes := []*graph.Node{
				{ID: "a", FilePath: "a.py", Language: "python"},
				{ID: "b", FilePath: "b.deferred", Language: tc.second},
				{ID: "z", FilePath: "z.go", Language: "go"},
			}
			var facts []ParseFacts
			for _, n := range nodes {
				n.Kind, n.Name, n.QualName = graph.KindFunction, n.ID, n.ID
				e := &graph.Edge{From: n.ID, To: "dep::example.test/pkg::Call", Kind: graph.EdgeCalls, FilePath: n.FilePath, Line: 1}
				b.AddBatch([]*graph.Node{n}, []*graph.Edge{e})
				facts = append(facts, ParseFacts{EffectiveLanguage: n.Language, File: graph.FileMetaRow{FilePath: n.FilePath}, Edges: []*graph.Edge{e}})
			}
			if tc.existing {
				b.AddNode(&graph.Node{ID: packageID, Kind: graph.KindModule, Language: "existing"})
			}
			candidate := func(n *graph.Node) RebuildExternalCallCandidate {
				return RebuildExternalCallCandidate{
					Node: &graph.Node{ID: packageID, Kind: graph.KindModule, Language: n.Language}, Language: n.Language,
				}
			}
			rules := RebuildExternalCallRules{
				Candidate: func(e *graph.Edge, n *graph.Node) (RebuildExternalCallCandidate, bool) {
					return candidate(n), strings.HasPrefix(e.To, "dep::")
				},
				Maybe: func(e *graph.Edge, n *graph.Node, _ string, live bool) []RebuildExternalCallCandidate {
					if live && e.From == "a" {
						return []RebuildExternalCallCandidate{candidate(n)}
					}
					return nil
				},
				Rebind: func(e *graph.Edge, to, _ string) graph.EdgeReindex {
					old := e.To
					e.To = to
					return graph.EdgeReindex{Edge: e, OldTo: old}
				},
			}
			receipts := iter.Seq2[ParseFacts, error](func(yield func(ParseFacts, error) bool) {
				for i := len(facts) - 1; i >= 0; i-- {
					if !yield(facts[i], nil) {
						return
					}
				}
			})
			before := rebuildFrameworkCustodyRows(t, b)
			count, err := b.RebuildExternalCallSynthesis(t.Context(), receipts, rules)
			if tc.refuse {
				require.ErrorIs(t, err, ErrRebuildExternalCallSynthesis)
				require.ErrorContains(t, err, "uncertain retained external-call package creator")
				require.Zero(t, count)
				require.Equal(t, before, rebuildFrameworkCustodyRows(t, b), "refusal cleans scratch without mutating the graph")
				return
			}
			require.NoError(t, err)
			require.Equal(t, uint64(3), count)
			n, err := b.pendingNode(packageID)
			require.NoError(t, err)
			require.NotNil(t, n)
			wantLanguage := "python"
			if tc.existing {
				wantLanguage = "existing"
			}
			require.Equal(t, wantLanguage, n.Language)
			for _, f := range facts {
				old := f.Edges[0]
				rebound := *old
				rebound.To = packageID
				wantKey := pendingEdgeKey(&rebound)
				stored, err := b.pendingEdge(wantKey)
				require.NoError(t, err)
				require.Equal(t, &rebound, stored)
				requireRebuildLinkCustody(t, b, &rebound, rebuildEdgeCustody{raw: pendingEdgeKey(old)})
				gone, err := b.pendingEdge(pendingEdgeKey(old))
				require.NoError(t, err)
				require.Nil(t, gone)
			}
			after := rebuildFrameworkCustodyRows(t, b)
			count, err = b.RebuildExternalCallSynthesis(t.Context(), receipts, rules)
			require.NoError(t, err)
			require.Zero(t, count)
			require.Equal(t, after, rebuildFrameworkCustodyRows(t, b))
		})
	}
}
