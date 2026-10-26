package resolver

import (
	"github.com/stretchr/testify/require"
	"github.com/zzet/gortex/internal/graph"
	"testing"
)

func TestCrossRepoChunkedRestubMetadataPersists(t *testing.T) {
	for _, backend := range []string{"memory", "sqlite"} {
		for _, chunk := range []string{"0", "1"} {
			t.Run(backend+"/chunk="+chunk, func(t *testing.T) {
				t.Setenv("GORTEX_RESOLVE_CHUNK", chunk)
				g := goImportReviewStore(t, backend)
				call := &graph.Edge{From: "caller/main.py::run", To: "provider/gone.py::missing", Kind: graph.EdgeCalls, FilePath: "caller/main.py", Line: 2, Confidence: 0.85, Origin: graph.OriginASTResolved}
				graph.StashRestubProvenance(call)
				call.To = "unresolved::missing"
				g.AddBatch([]*graph.Node{{ID: "caller/main.py", Kind: graph.KindFile, FilePath: "caller/main.py", RepoPrefix: "caller", Language: "python"}, {ID: call.From, Name: "run", Kind: graph.KindFunction, FilePath: call.FilePath, RepoPrefix: "caller", Language: "python"}}, []*graph.Edge{call})
				for i := 0; i < 2; i++ {
					_, err := NewCrossRepo(g).ResolveAllContext(t.Context())
					require.NoError(t, err)
					rows := g.GetOutEdges(call.From)
					require.Len(t, rows, 1)
					require.Equal(t, "unresolved::missing", rows[0].To)
					require.False(t, graph.HasRestubProvenance(rows[0]))
					require.Empty(t, rows[0].Origin)
					require.Zero(t, rows[0].Confidence)
				}
			})
		}
	}
}

func TestCrossRepoMetaReindexStillRequiresLiveEdge(t *testing.T) {
	g := graph.New()
	cr := NewCrossRepo(g)
	call := &graph.Edge{From: "a/main.py::run", To: "unresolved::missing", Kind: graph.EdgeCalls, FilePath: "a/main.py", Line: 2}
	require.Empty(t, cr.filterLiveReindex([]graph.EdgeReindex{{Edge: call, OldTo: call.To}}))
	g.AddEdge(call)
	copy := *call
	copy.To = "b/gone.py::missing"
	require.Empty(t, cr.filterLiveReindex([]graph.EdgeReindex{{Edge: &copy, OldTo: call.To}}))
	require.Equal(t, call.To, copy.To)
}
