package store_pebble

import (
	"context"
	"iter"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/zzet/gortex/internal/graph"
)

func TestRebuildReactSetStateInstalledCustody(t *testing.T) {
	testRebuildSetStateInstalledCustody(t, "render", rebuildProofReactSetState, (*BuildStore).RebuildReactSetState)
}

func TestRebuildFlutterSetStateInstalledCustody(t *testing.T) {
	testRebuildSetStateInstalledCustody(t, "build", rebuildProofFlutterSetState, (*BuildStore).RebuildFlutterSetState)
}

func testRebuildSetStateInstalledCustody(t *testing.T, lifecycle string, proof rebuildEdgeProof,
	run func(*BuildStore, context.Context, iter.Seq2[ParseFacts, error], func(string) bool,
		func(*graph.Node, *graph.Node, string) *graph.Edge) (RebuildReactSetStateStats, error),
) {
	t.Helper()
	const file, class = "widget.go", "Widget"
	caller := &graph.Node{ID: "refresh", Name: "refresh", Kind: graph.KindMethod, FilePath: file, Language: "go", StartLine: 5}
	target := &graph.Node{ID: lifecycle, Name: lifecycle, Kind: graph.KindMethod, FilePath: file, Language: "go"}
	member := func(n *graph.Node) *graph.Edge {
		return &graph.Edge{From: n.ID, To: class, Kind: graph.EdgeMemberOf, FilePath: file, Line: n.StartLine}
	}
	call := &graph.Edge{From: caller.ID, To: "unresolved::setState", Kind: graph.EdgeCalls, FilePath: file, Line: 8}
	b, receipts := seedSyntheticCustody(t, ParseFacts{
		EffectiveLanguage: "go",
		File:              graph.FileMetaRow{FilePath: file},
		Nodes:             []*graph.Node{caller, target},
		Edges:             []*graph.Edge{member(caller), member(target), call},
	})
	makeCall := func(from, to *graph.Node, owner string) *graph.Edge {
		return &graph.Edge{From: from.ID, To: to.ID, Kind: graph.EdgeCalls, FilePath: from.FilePath, Line: from.StartLine,
			Origin: graph.OriginASTInferred, Meta: map[string]any{"component_class": owner}}
	}
	stats, err := run(b, t.Context(), receipts, func(id string) bool { return id == call.To }, makeCall)
	require.NoError(t, err)
	require.EqualValues(t, 1, stats.Edges)
	want := makeCall(caller, target, class)
	live, err := b.pendingEdge(pendingEdgeKey(want))
	require.NoError(t, err)
	require.Equal(t, want, live)
	requireSyntheticCustody(t, b, map[graph.EdgeIdentity]rebuildEdgeProof{graph.EdgeIdentityFor(want): proof})
}
