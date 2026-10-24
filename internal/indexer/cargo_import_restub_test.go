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

func TestCargoRestubKeepsSameRepoCallsOnNativePath(t *testing.T) {
	for _, mode := range []string{"point", "batch"} {
		t.Run(mode, func(t *testing.T) {
			g := graph.New()
			file, definition, caller := "repo/a.rs", "repo/a.rs::foo", "repo/b.rs::run"
			g.AddNode(&graph.Node{ID: file, FilePath: file, Kind: graph.KindFile, RepoPrefix: "repo"})
			g.AddNode(&graph.Node{ID: definition, FilePath: file, Name: "foo", Kind: graph.KindFunction, RepoPrefix: "repo"})
			g.AddNode(&graph.Node{ID: caller, FilePath: "repo/b.rs", Name: "run", Kind: graph.KindFunction, RepoPrefix: "repo"})
			edge := &graph.Edge{From: caller, FilePath: "repo/b.rs", Kind: graph.EdgeCalls, To: definition,
				Meta: map[string]any{resolver.CargoCallStubMeta: "unresolved::*.foo"}}
			g.AddEdge(edge)
			if mode == "point" {
				idx := &Indexer{graph: g}
				idx.restubIncomingRefs(file)
			} else {
				stages := []*incrementalBatchStage{{graphPath: file, priorNodes: g.GetFileNodes(file)}}
				view := loadIncrementalPriorView(g, stages)
				restubIncomingRefsFromView(g, stages, view)
			}
			require.Equal(t, "unresolved::foo", edge.To)
			require.False(t, edge.CrossRepo)
		})
	}
}
