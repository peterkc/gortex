package indexer

import (
	"sort"
	"testing"

	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	"github.com/zzet/gortex/internal/config"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/parser"
	"github.com/zzet/gortex/internal/resolver"
)

func TestContentLinkEdgeBudget(t *testing.T) {
	require.Equal(t, 2000, contentLinkEdgeBudget(0))
	require.Equal(t, 2000, contentLinkEdgeBudget(10000), "10% (1000) is below the 2000 floor")
	require.Equal(t, 5000, contentLinkEdgeBudget(50000), "10% of live edges above the floor")
}

type allNodesCountingGraph struct {
	*graph.Graph
	allNodesCalls int
}

// This success spy preserves Graph's live candidate pointers explicitly.
func (s *allNodesCountingGraph) MarkEdgesCrossRepo(edges []*graph.Edge) int {
	for _, edge := range edges {
		edge.CrossRepo = true
	}
	return len(edges)
}

func (g *allNodesCountingGraph) AllNodes() []*graph.Node {
	g.allNodesCalls++
	return g.Graph.AllNodes()
}

func contentLinkFixture() *graph.Graph {
	g := graph.New()
	g.AddBatch([]*graph.Node{
		{ID: "repoA/pkg/a.go::SharedSymbol", Kind: graph.KindFunction, Name: "SharedSymbol", FilePath: "repoA/pkg/a.go", RepoPrefix: "repoA"},
		{ID: "repoB/pkg/b.go::SharedSymbol", Kind: graph.KindMethod, Name: "SharedSymbol", FilePath: "repoB/pkg/b.go", RepoPrefix: "repoB"},
		{ID: "repoA/spec.txt::doc:0", Kind: graph.KindDoc, FilePath: "repoA/spec.txt", RepoPrefix: "repoA",
			Meta: map[string]any{"data_class": "content", "section_text": "SharedSymbol explains the integration."}},
	}, nil)
	return g
}

func contentLinkTargets(g graph.Store, kind graph.EdgeKind) []string {
	var targets []string
	for edge := range g.EdgesByKind(kind) {
		if edge.From == "repoA/spec.txt::doc:0" {
			targets = append(targets, edge.To)
		}
	}
	sort.Strings(targets)
	return targets
}

func TestContentLinkingAndGlobalPassAvoidAllNodesWithCrossRepoParity(t *testing.T) {
	cfg := config.Default().Index
	newIndexer := func(store graph.Store) *Indexer {
		idx := New(store, parser.NewRegistry(), cfg, zap.NewNop())
		idx.repoPrefix = "repoA"
		return idx
	}

	baseline := contentLinkFixture()
	newIndexer(baseline).linkContentToCode()
	want := contentLinkTargets(baseline, graph.EdgeMotivates)
	require.Equal(t, []string{
		"repoA/pkg/a.go::SharedSymbol",
		"repoB/pkg/b.go::SharedSymbol",
	}, want)

	counting := &allNodesCountingGraph{Graph: contentLinkFixture()}
	newIndexer(counting).linkContentToCode()
	require.Zero(t, counting.allNodesCalls)
	require.Equal(t, want, contentLinkTargets(counting, graph.EdgeMotivates))

	global := &allNodesCountingGraph{Graph: contentLinkFixture()}
	newIndexer(global).linkContentToCode()
	if _, err := resolver.DetectCrossRepoEdges(global); err != nil {
		t.Fatal(err)
	}
	require.Zero(t, global.allNodesCalls, "the automatic global cold path must never request a node snapshot")
	require.Equal(t, want, contentLinkTargets(global, graph.EdgeMotivates))
	require.Equal(t, []string{"repoB/pkg/b.go::SharedSymbol"},
		contentLinkTargets(global, graph.EdgeCrossRepoMotivates),
		"global name union must retain cross-repository content links")
}

func TestLinkContentToCode(t *testing.T) {
	g := graph.New()
	g.AddBatch([]*graph.Node{
		{ID: "pkg/order.go::ProcessOrder", Kind: graph.KindFunction, Name: "ProcessOrder", FilePath: "pkg/order.go"},
		{ID: "deck.pptx::doc:slide-1", Kind: graph.KindDoc, FilePath: "deck.pptx",
			Meta: map[string]any{"data_class": "content", "asset_kind": "slide",
				"section_text": "This deck explains why ProcessOrder validates inventory before charging."}},
		// Markdown prose has no data_class — it must NOT be linked by the content pass.
		{ID: "README.md::doc:intro", Kind: graph.KindDoc, FilePath: "README.md",
			Meta: map[string]any{"asset_kind": "markdown_section", "section_text": "ProcessOrder is the core."}},
	}, nil)

	idx := New(g, parser.NewRegistry(), config.Default().Index, zap.NewNop())
	idx.linkContentToCode()

	var motivates []*graph.Edge
	for _, e := range g.AllEdges() {
		if e.Kind == graph.EdgeMotivates {
			motivates = append(motivates, e)
		}
	}
	require.Len(t, motivates, 1, "the content chunk links; markdown prose (no data_class) does not")
	require.Equal(t, "deck.pptx::doc:slide-1", motivates[0].From)
	require.Equal(t, "pkg/order.go::ProcessOrder", motivates[0].To)
	require.Equal(t, graph.OriginTextMatched, motivates[0].Origin)
	require.Equal(t, "lexical", motivates[0].Meta["signal"])
}
