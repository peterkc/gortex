package resolver

import (
	"github.com/stretchr/testify/require"
	"github.com/zzet/gortex/internal/graph"
	"iter"
	"testing"
)

type goPendingDrainStore struct {
	*goModuleDroppingStore
	identitySweeps int
}

func (s *goPendingDrainStore) RepoNodeIdentitiesSeq(repos []string, kinds ...graph.NodeKind) iter.Seq[graph.RepoNodeIdentity] {
	s.identitySweeps++
	return graph.RepoNodeIdentitiesSeq(s.Store, repos, kinds...)
}

func TestGoModulePendingDrainsOnNonGoMutation(t *testing.T) {
	for _, backend := range []string{"memory", "sqlite"} {
		t.Run(backend, func(t *testing.T) {
			g := &goPendingDrainStore{goModuleDroppingStore: &goModuleDroppingStore{Store: goImportReviewStore(t, backend), drop: true}}
			g.AddNode(&graph.Node{ID: "a/main.go::Caller", FilePath: "a/main.go", Kind: graph.KindFunction, Name: "Caller", Language: "go", RepoPrefix: "a", WorkspaceID: "shared"})
			g.AddNode(&graph.Node{ID: "b/lib.go::Fn", FilePath: "b/lib.go", Kind: graph.KindFunction, Name: "Fn", Language: "go", RepoPrefix: "b", WorkspaceID: "shared"})
			edge := &graph.Edge{From: "a/main.go::Caller", FilePath: "a/main.go", To: "dep::example.org/lib::Fn", Kind: graph.EdgeCalls, Line: 1}
			g.AddEdge(edge)
			state := &GoModuleDepRetry{}
			makeResolver := func() *CrossRepoResolver {
				cr := NewCrossRepo(g)
				cr.SetGoModules(map[string]string{"b": "example.org/lib"})
				cr.SetGoModuleDepRetry(state, 1)
				return cr
			}
			require.NoError(t, makeResolver().ResolveDeferredGoModuleEdges([]*graph.Edge{edge}))
			require.Len(t, state.pending, 1)
			g.drop = true // The next drain must preserve a second refusal.
			g.identitySweeps = 0
			_, admission := makeResolver().ResolveMutationFrontiersBounded(nil, nil, []string{"a/README.md"})
			require.False(t, admission.Refused)
			require.Len(t, state.pending, 1)
			require.False(t, g.drop, "the queued batch must have been attempted")
			require.Equal(t, "dep::example.org/lib::Fn", g.GetOutEdges(edge.From)[0].To)
			_, admission = makeResolver().ResolveMutationFrontiersBounded(nil, nil, []string{"a/app.py"})
			require.False(t, admission.Refused)
			require.Empty(t, state.pending)
			var calls []*graph.Edge
			for _, row := range g.GetOutEdges(edge.From) {
				if row.Kind == graph.EdgeCalls {
					calls = append(calls, row)
				}
			}
			require.Len(t, calls, 1)
			require.Equal(t, "b/lib.go::Fn", calls[0].To)
			require.True(t, calls[0].CrossRepo)
			require.False(t, state.complete, "a queue drain must not certify the legacy sweep")
			makeResolver().ResolveMutationFrontiersBounded(nil, nil, []string{"a/app.ts"})
			require.Zero(t, g.identitySweeps, "non-Go saves may drain, not reseed or recheck all repositories")
		})
	}
}
