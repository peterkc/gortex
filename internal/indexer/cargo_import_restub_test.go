package indexer

import (
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/resolver"
)

func TestCargoRestubLeavesUnmarkedImportsUnchanged(t *testing.T) {
	for _, file := range []string{"provider/src/lib.rs", "provider/main.go"} {
		t.Run(file, func(t *testing.T) {
			g := graph.New()
			g.AddNode(&graph.Node{ID: file, Kind: graph.KindFile, FilePath: file, RepoPrefix: "provider"})
			edge := &graph.Edge{From: "consumer/source", FilePath: "consumer/source", To: file, Kind: graph.EdgeImports, CrossRepo: true}
			if file == "provider/main.go" {
				edge.Meta = map[string]any{resolver.CargoImportStubMeta: "external::other_crate/action"}
			}
			g.AddEdge(edge)
			require.Empty(t, restubTrackedCargoRepoIncoming(g, "provider"))
			require.Equal(t, file, edge.To)
			require.True(t, edge.CrossRepo)
		})
	}
}
