package store_pebble

import (
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/zzet/gortex/internal/graph"
)

func TestRebuildImplementsPrivateProducerStamp(t *testing.T) {
	const file = "service.ts"
	owner := &graph.Node{ID: file + "::Service", Name: "Service", FilePath: file, Kind: graph.KindType, Language: "typescript", StartLine: 2}
	iface := &graph.Node{ID: file + "::Runner", Name: "Runner", FilePath: file, Kind: graph.KindInterface, Language: "typescript",
		Meta: map[string]any{"methods": []string{"run"}}}
	method := &graph.Node{ID: owner.ID + ".run", Name: "run", FilePath: file, Kind: graph.KindMethod, Language: "typescript"}
	member := &graph.Edge{From: method.ID, To: owner.ID, Kind: graph.EdgeMemberOf, FilePath: file, Line: 3}
	facts := ParseFacts{
		File: graph.FileMetaRow{FilePath: file, NodeCount: 3}, EffectiveLanguage: "typescript",
		Nodes: []*graph.Node{owner, iface, method}, Edges: []*graph.Edge{member},
	}
	want := &graph.Edge{From: owner.ID, To: iface.ID, Kind: graph.EdgeImplements, FilePath: file, Line: owner.StartLine,
		Meta: map[string]any{"via": "method-set-inference"}}
	b := newBuild(t, FullProfile)
	require.NoError(t, b.AddParsedFile("", facts))
	err := b.RebuildImplements(t.Context(), func(yield func(ParseFacts, error) bool) {
		yield(facts, nil)
	}, func(meta map[string]any) []string {
		names, _ := meta["methods"].([]string)
		return names
	}, func(from *graph.Node, to string) *graph.Edge {
		return &graph.Edge{From: from.ID, To: to, Kind: graph.EdgeImplements, FilePath: from.FilePath, Line: from.StartLine,
			Meta: map[string]any{"via": "method-set-inference"}}
	})
	require.NoError(t, err)
	key := pendingEdgeKey(want)
	wantBytes, err := encodeEdge(want)
	require.NoError(t, err)
	got, err := pendingGet(b.pending, key, copyValue)
	require.NoError(t, err)
	require.Equal(t, wantBytes, got, "producer evidence does not change the saved row")
	proved, err := rebuildImplementsStamped(b.pending, key)
	require.NoError(t, err)
	require.True(t, proved)

	unproved := newBuild(t, FullProfile)
	unproved.AddEdge(want)
	proved, err = rebuildImplementsStamped(unproved.pending, key)
	require.NoError(t, err)
	require.False(t, proved, "the same payload is not a producer witness")

	changed := *want
	changed.Origin = graph.OriginASTInferred
	b.AddEdge(&changed)
	proved, err = rebuildImplementsStamped(b.pending, key)
	require.NoError(t, err)
	require.False(t, proved, "a changed payload cannot borrow the old proof")
}
