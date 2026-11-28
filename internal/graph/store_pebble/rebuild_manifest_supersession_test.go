package store_pebble

import (
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/zzet/gortex/internal/graph"
)

func newRebuildManifestCut(t *testing.T, receipt string, held *graph.Node) *BuildStore {
	t.Helper()
	b := newBuild(t, FullProfile)
	require.NoError(t, b.AddRebuildDeferredFile("", ParseFacts{
		File:              graph.FileMetaRow{FilePath: receipt, NodeCount: 1},
		EffectiveLanguage: "json",
		Nodes:             []*graph.Node{held},
	}, func(string) (bool, error) { return true, nil }))
	require.NoError(t, b.IsolateRebuildLinks(t.Context()))
	return b
}

func TestRebuildHeldManifestWriterRejectsNodeShape(t *testing.T) {
	for _, tc := range []struct {
		name   string
		change func(held, writer *graph.Node)
	}{
		{"not a manifest", func(held, writer *graph.Node) {
			held.ID, held.FilePath = "src/config.json", "src/config.json"
			*writer = *held
		}},
		{"writer is a module", func(_, writer *graph.Node) { writer.Kind = graph.KindModule }},
		{"writer ID differs from file path", func(held, writer *graph.Node) {
			held.FilePath, writer.FilePath = "pkg/package.json", "pkg/package.json"
		}},
		{"held row is not a file", func(held, _ *graph.Node) { held.Kind = graph.KindVariable }},
		{"held row belongs to another file path", func(held, _ *graph.Node) { held.FilePath = "pkg/package.json" }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			held := graph.Node{ID: "package.json", FilePath: "package.json", Kind: graph.KindFile, Language: "json"}
			writer := held
			tc.change(&held, &writer)
			b := newRebuildManifestCut(t, held.FilePath, &held)
			err := b.AddRebuildManifestArtifacts(t.Context(), []*graph.Node{&writer}, nil)
			require.ErrorIs(t, err, ErrRebuildLinkIsolation)
			require.ErrorContains(t, err, "manifest writer collides with held node")
		})
	}
}

func TestRebuildHeldManifestWriterEdges(t *testing.T) {
	for _, tc := range []struct {
		name      string
		kind      graph.EdgeKind
		writeNode bool
		incumbent bool
		want      string
	}{
		{"wrong edge kind", graph.EdgeReferences, true, false, "manifest writer edge uses held node"},
		{"held endpoint without a replacement", graph.EdgeDependsOnModule, false, false, "manifest writer edge uses held node"},
		{"identical live edge without ownership", graph.EdgeDependsOnModule, true, true, "manifest writer found unrelated incident edge"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			const file = "package.json"
			held := graph.Node{ID: file, FilePath: file, Kind: graph.KindFile, Language: "json", StartLine: 1}
			b := newRebuildManifestCut(t, file, &held)
			module := &graph.Node{ID: "module::npm:a@1", Name: "a", Kind: graph.KindModule, FilePath: file}
			nodes := []*graph.Node{module}
			if tc.writeNode {
				winner := held
				winner.StartLine = 0
				nodes = append(nodes, &winner)
			}
			edge := &graph.Edge{From: file, To: module.ID, Kind: tc.kind, FilePath: file}
			if tc.incumbent {
				b.AddEdge(edge)
				live, err := b.pendingEdge(pendingEdgeKey(edge))
				require.NoError(t, err)
				require.Equal(t, edge, live)
			}
			err := b.AddRebuildManifestArtifacts(t.Context(), nodes, []*graph.Edge{edge})
			require.ErrorIs(t, err, ErrRebuildLinkIsolation)
			require.ErrorContains(t, err, tc.want)
		})
	}
}

func TestRebuildHeldManifestWriterRestoration(t *testing.T) {
	for _, tc := range []struct {
		name       string
		receipt    string
		deleteEdge bool
	}{
		{"supersedes a winner from another receipt", "plugin.json", false},
		{"refuses a deleted writer edge", "package.json", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			const file = "package.json"
			ctx := t.Context()
			held := graph.Node{ID: file, FilePath: file, Kind: graph.KindFile, Language: "json", StartLine: 1}
			b := newRebuildManifestCut(t, tc.receipt, &held)
			winner := held
			winner.StartLine = 0
			module := &graph.Node{ID: "module::npm:a@1", Name: "a", Kind: graph.KindModule, FilePath: file}
			edge := &graph.Edge{From: file, To: module.ID, Kind: graph.EdgeDependsOnModule, FilePath: file}
			require.NoError(t, b.AddRebuildManifestArtifacts(ctx, []*graph.Node{&winner, module}, []*graph.Edge{edge}))
			if tc.deleteEdge {
				require.True(t, b.RemoveEdge(edge.From, edge.To, edge.Kind))
				err := b.RestoreRebuildDeclarations(ctx)
				require.ErrorIs(t, err, ErrRebuildLinkIsolation)
				require.ErrorContains(t, err, "manifest writer edge differs from capture")
				return
			}
			require.NoError(t, b.RestoreRebuildDeclarations(ctx))
			liveNode, err := b.RebuildView().Node(ctx, file)
			require.NoError(t, err)
			require.Equal(t, &winner, liveNode)
			liveEdge, err := b.pendingEdge(pendingEdgeKey(edge))
			require.NoError(t, err)
			require.Equal(t, edge, liveEdge)
		})
	}
}
