package store_pebble

import (
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/zzet/gortex/internal/graph"
)

func TestRebuildKMPExpectActualMuxAndCustody(t *testing.T) {
	makeNode := func(id, file, name, lang, role string) *graph.Node {
		return &graph.Node{ID: id, Name: name, Kind: graph.KindFunction, FilePath: file, Language: lang,
			StartLine: 3, Meta: map[string]any{"kmp_role": role}}
	}
	expect := makeNode("common.go::clock", "common.go", "clock", "go", "expect")
	protected := makeNode("common.go::clock#param:p", "common.go", "clock", "go", "expect")
	actual := makeNode("actual.java::clock", "actual.java", "clock", "java", "actual")
	makeEdge := func(n *graph.Node, to string, kind graph.EdgeKind) *graph.Edge {
		return &graph.Edge{From: n.ID, To: to, Kind: kind, FilePath: n.FilePath, Line: n.StartLine,
			Origin: graph.OriginASTInferred}
	}
	stationary := makeEdge(expect, actual.ID, graph.EdgeReferences)
	stationary.Origin = graph.OriginASTResolved
	b, receipts := seedSyntheticCustody(t,
		ParseFacts{EffectiveLanguage: "go", File: graph.FileMetaRow{FilePath: expect.FilePath}, Nodes: []*graph.Node{expect, protected},
			Edges: []*graph.Edge{stationary}},
		ParseFacts{EffectiveLanguage: "java", File: graph.FileMetaRow{FilePath: actual.FilePath}, Nodes: []*graph.Node{actual}})
	ctx := t.Context()
	stats, err := b.RebuildKMPExpectActual(ctx, receipts, 1, makeEdge)
	require.NoError(t, err)
	require.Zero(t, stats)

	// The native gate admits the pass based on the live census. The Kotlin
	// marker isn't itself a participating declaration or a proof endpoint.
	b.AddNode(&graph.Node{ID: "marker.kt", Kind: graph.KindFile, FilePath: "marker.kt", Language: " Kotlin "})
	stats, err = b.RebuildKMPExpectActual(ctx, receipts, 1, makeEdge)
	require.NoError(t, err)
	require.EqualValues(t, 2, stats.Edges)
	live, err := b.pendingEdge(pendingEdgeKey(stationary))
	require.NoError(t, err)
	require.Equal(t, stationary, live)
	dropped, err := b.pendingEdge(pendingEdgeKey(makeEdge(actual, protected.ID, graph.EdgeImplements)))
	require.NoError(t, err)
	require.Nil(t, dropped)
	requireSyntheticCustody(t, b, map[graph.EdgeIdentity]rebuildEdgeProof{
		graph.EdgeIdentityFor(makeEdge(actual, expect.ID, graph.EdgeImplements)):    rebuildProofKMPExpectActual,
		graph.EdgeIdentityFor(makeEdge(protected, actual.ID, graph.EdgeReferences)): rebuildProofKMPExpectActual,
	})
}
