package store_pebble

import (
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/zzet/gortex/internal/graph"
)

func TestRebuildCSharpIfaceDispatchProvedCallProducer(t *testing.T) {
	for _, proof := range []rebuildEdgeProof{rebuildProofMediatRFanout, rebuildProofUntrackedMove} {
		b := newBuild(t, FullProfile)
		const file = "Types.cs"
		iface := &graph.Node{ID: "iface", Name: "IFoo", Kind: graph.KindInterface, Language: "csharp", FilePath: file}
		anchor := &graph.Node{ID: "anchor", Name: "Run", Kind: graph.KindMethod, Language: "csharp", FilePath: file,
			Meta: map[string]any{"iface_member": true}}
		impl := &graph.Node{ID: "impl", Name: "Foo", Kind: graph.KindType, Language: "csharp", FilePath: file}
		method := &graph.Node{ID: "method", Name: "Run", Kind: graph.KindMethod, Language: "csharp", FilePath: file}
		call := &graph.Edge{From: "caller", To: anchor.ID, Kind: graph.EdgeCalls, FilePath: file, Line: 7}
		nodes := []*graph.Node{iface, anchor, impl, method}
		edges := []*graph.Edge{
			{From: impl.ID, To: iface.ID, Kind: graph.EdgeImplements, FilePath: file},
			{From: anchor.ID, To: iface.ID, Kind: graph.EdgeMemberOf, FilePath: file},
			{From: method.ID, To: impl.ID, Kind: graph.EdgeMemberOf, FilePath: file},
		}
		b.AddBatch(nodes, edges)
		require.NoError(t, b.putEdgeWithCustody(call, rebuildEdgeCustody{proof: proof}))
		facts := ParseFacts{EffectiveLanguage: "csharp", File: graph.FileMetaRow{FilePath: file}, Nodes: nodes, Edges: edges}
		receipts := func(yield func(ParseFacts, error) bool) { yield(facts, nil) }
		rules := RebuildCSharpIfaceRules{
			Declared: func(*graph.Edge) bool { return true },
			Candidate: func(n *graph.Node, repo string) bool {
				return n.RepoPrefix == repo && n.Language == "csharp" && (n.Kind == graph.KindInterface || n.Kind == graph.KindType)
			},
			Anchor: func(n *graph.Node) bool { return n.Meta["iface_member"] == true },
			Source: func(*graph.Edge) bool { return true },
			Emit: func(e *graph.Edge, id, _ string, _ int) *graph.Edge {
				copy := *e
				copy.To = id
				return &copy
			},
			Cap: 128,
		}
		before := rebuildFrameworkCustodyRows(t, b)
		adds, err := b.RebuildCSharpIfaceDispatch(t.Context(), receipts, rules)
		if proof == rebuildProofUntrackedMove {
			require.ErrorIs(t, err, ErrRebuildUnknownProvenance)
			require.Zero(t, adds)
			require.Equal(t, before, rebuildFrameworkCustodyRows(t, b))
			continue
		}
		require.NoError(t, err)
		require.EqualValues(t, 1, adds)
		out := *call
		out.To = method.ID
		got, err := b.pendingEdge(pendingEdgeKey(&out))
		require.NoError(t, err)
		require.Equal(t, &out, got)
	}
}
