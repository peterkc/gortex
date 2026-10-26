package indexer

import (
	"fmt"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/resolver"
)

type bridgeIncomingCountStore struct {
	graph.Store
	fullReads, lightReads, hydrated int
}

func (s *bridgeIncomingCountStore) GetInEdgesByNodeIDs(ids []string) map[string][]*graph.Edge {
	s.fullReads++
	return s.Store.GetInEdgesByNodeIDs(ids)
}
func (s *bridgeIncomingCountStore) GetInEdgeIdentitiesByNodeIDs(ids []string) map[string][]graph.EdgeIdentity {
	s.lightReads++
	return graph.InEdgeIdentitiesByNodeIDs(s.Store, ids)
}
func (s *bridgeIncomingCountStore) GetEdgeCandidates(endpoints []graph.EdgeEndpoint, sites []graph.EdgeSite) graph.EdgeCandidateSet {
	s.hydrated += len(sites)
	return s.Store.GetEdgeCandidates(endpoints, sites)
}

func TestBridgeRepoRestubHydratesOnlyForeignSites(t *testing.T) {
	for _, backend := range []string{"memory", "sqlite"} {
		for _, lang := range []string{"python", "cargo"} {
			t.Run(backend+"/"+lang, func(t *testing.T) {
				g := &bridgeIncomingCountStore{Store: cargoWarmStore(t, backend)}
				file, importKey, callKey := "provider/mod.py", resolver.PythonImportStubMeta, resolver.PythonCallStubMeta
				restub := restubTrackedPythonRepoIncoming
				if lang == "cargo" {
					file, importKey, callKey, restub = "provider/mod.rs", resolver.CargoImportStubMeta, resolver.CargoCallStubMeta, restubTrackedCargoRepoIncoming
				}
				target := file + "::f"
				g.AddNode(&graph.Node{ID: file, FilePath: file, Kind: graph.KindFile, RepoPrefix: "provider"})
				g.AddNode(&graph.Node{ID: target, FilePath: file, Kind: graph.KindFunction, RepoPrefix: "provider"})
				for i := range 40 {
					g.AddEdge(&graph.Edge{From: fmt.Sprintf("provider/local%d::run", i), To: target, FilePath: "provider/local", Kind: graph.EdgeCalls, CrossRepo: true, Meta: map[string]any{callKey: "unresolved::f"}})
					g.AddEdge(&graph.Edge{From: fmt.Sprintf("foreign/other%d", i), To: file, FilePath: "foreign/other", Kind: graph.EdgeContains})
				}
				g.AddEdge(&graph.Edge{From: "consumer/app", To: file, FilePath: "consumer/app", Kind: graph.EdgeImports, Line: 1, CrossRepo: true, Meta: map[string]any{importKey: "external::pkg.mod"}})
				g.AddEdge(&graph.Edge{From: "consumer/app::run", To: target, FilePath: "consumer/app", Kind: graph.EdgeCalls, Line: 2, CrossRepo: true, ConfidenceLabel: "high", Meta: map[string]any{callKey: "unresolved::f"}})
				g.AddEdge(&graph.Edge{From: "other/app", To: file, FilePath: "other/app", Kind: graph.EdgeImports, Line: 3, CrossRepo: true})
				require.Equal(t, []string{"consumer/app"}, restub(g, "provider"))
				require.Zero(t, g.fullReads, "repository parking must not fetch full inbound adjacency")
				require.Positive(t, g.lightReads)
				require.Equal(t, 3, g.hydrated, "only foreign import/call sites need metadata")
				require.Equal(t, file, g.GetOutEdges("other/app")[0].To)
				require.Equal(t, target, g.GetOutEdges("provider/local0::run")[0].To)
				call := g.GetOutEdges("consumer/app::run")[0]
				require.Equal(t, "unresolved::f", call.To)
				require.False(t, call.CrossRepo)
				require.Empty(t, call.ConfidenceLabel)
				require.True(t, graph.HasRestubProvenance(call))
				require.Empty(t, restub(g, "provider"))
			})
		}
	}
}
