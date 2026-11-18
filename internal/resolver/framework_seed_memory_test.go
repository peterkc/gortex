package resolver

import (
	"iter"
	"slices"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/zzet/gortex/internal/graph"
)

type frameworkCandidateReadCounter struct {
	*frameworkScopeTrapStore
	rows int
}

func (s *frameworkCandidateReadCounter) EdgesInScopeSeq(repos, files []string, kinds ...graph.EdgeKind) iter.Seq[graph.ScopedEdgeRow] {
	base := graph.EdgesInScopeSeq(s.Store, repos, files, kinds...)
	return func(yield func(graph.ScopedEdgeRow) bool) {
		for row := range base {
			s.rows++
			if !yield(row) {
				return
			}
		}
	}
}

func TestFrameworkCandidateGateDoesNotRetainWholeKind(t *testing.T) {
	// Given one matching call followed by unrelated calls, when the gate
	// finds its witness, then it neither reads nor caches the remaining kind.
	g := graph.New()
	g.AddNode(frameworkTestNode("repo", "changed.go", "caller", graph.KindFunction, "caller", "go", nil))
	g.AddEdge(&graph.Edge{From: "caller", To: "unresolved::stub", Kind: graph.EdgeCalls, FilePath: "changed.go", Line: 1, Meta: map[string]any{"via": "grpc.stub"}})
	for line := 2; line <= 100; line++ {
		g.AddEdge(&graph.Edge{From: "caller", To: "unresolved::ordinary", Kind: graph.EdgeCalls, FilePath: "changed.go", Line: line})
	}
	store := &frameworkCandidateReadCounter{frameworkScopeTrapStore: &frameworkScopeTrapStore{Store: g}}
	seed := newFrameworkDeclaredSeed(store, map[string]bool{"repo": true}, []string{"changed.go"})
	has, witness := seed.passCandidate(SynthGRPCStub)
	require.True(t, has)
	require.Equal(t, 1, witness.Line)
	require.Empty(t, seed.fileEdges, "a witness must not retain the entire kind across passes")
	require.Equal(t, 1, store.rows)
	// A later declared name reader still gets the whole projection.
	require.Len(t, seed.fileEdgesOf(graph.EdgeCalls), 100)
	require.Equal(t, 101, store.rows)
}

func TestFrameworkIncomingSeedOmitsSourceOwnedRows(t *testing.T) {
	// Given incoming sources both inside and outside the changed file, when
	// the seed loads them, then source-owned duplicates are not retained.
	for _, backend := range frameworkExactEdgeBackends() {
		t.Run(backend.name, func(t *testing.T) {
			store := backend.open(t)
			store.AddBatch([]*graph.Node{
				frameworkTestNode("repo", "changed.go", "target", graph.KindMethod, "Target", "go", nil),
				frameworkTestNode("repo", "changed.go", "inside", graph.KindFunction, "Inside", "go", nil),
				frameworkTestNode("other", "outside.go", "outside", graph.KindFunction, "Outside", "go", nil),
			}, []*graph.Edge{
				{From: "inside", To: "target", Kind: graph.EdgeCalls, FilePath: "misleading.go", Line: 1},
				{From: "inside", To: "target", Kind: graph.EdgeCalls, Line: 2},
				{From: "outside", To: "target", Kind: graph.EdgeCalls, FilePath: "changed.go", Line: 3},
			})
			seed := newFrameworkDeclaredSeed(store, map[string]bool{"repo": true}, []string{"changed.go", "changed.go"})
			incoming := seed.incomingOf(graph.EdgeCalls)
			require.Len(t, incoming, 1)
			require.Equal(t, "outside", incoming[0].From)
			for _, name := range []string{SynthGRPCStub, SynthTemporalStub, SynthMediatR} {
				view := seed.passStore(name)
				edges := slices.Collect(view.EdgesByKind(graph.EdgeCalls))
				require.Len(t, edges, 3, "the later %s reader must still see every call", name)
			}
			require.Contains(t, seed.endpointNodes(), "outside")
			require.NotContains(t, seed.endpointNodes(), "inside")
		})
	}
}

func TestFrameworkCandidateGateScansCompleteMissAndNoFileScope(t *testing.T) {
	// Given a no-candidate file or a repo-only scope, when the gate runs,
	// then it reads the whole file to reject, but never widens to the repo.
	store := &frameworkCandidateReadCounter{frameworkScopeTrapStore: &frameworkScopeTrapStore{Store: declaredSeedFixture(4)}}
	seed := newFrameworkDeclaredSeed(store, map[string]bool{"repo": true}, []string{"repo/config.go"})
	has, witness := seed.passCandidate(SynthFactoryChain)
	require.False(t, has)
	require.Nil(t, witness)
	require.Equal(t, 1, store.rows)
	require.Empty(t, seed.fileEdges)
	store.rows = 0
	seed = newFrameworkDeclaredSeed(store, map[string]bool{"repo": true}, nil)
	has, witness = seed.passCandidate(SynthFactoryChain)
	require.False(t, has)
	require.Nil(t, witness)
	require.Zero(t, store.rows)
}

func TestFrameworkFullRunsDoNotLoadDeclaredSeed(t *testing.T) {
	// Given nil scope or attested full coverage, when the registry runs,
	// then it never reads the changed-file seed.
	for _, attested := range []bool{false, true} {
		store := &frameworkScopeTrapStore{Store: declaredSeedFixture(4)}
		if attested {
			RunFrameworkSynthesizersScopedWithCensus(store, map[string]bool{"repo": true}, true)
		} else {
			RunFrameworkSynthesizers(store)
		}
		require.Zero(t, store.fileBatchReads)
		require.Zero(t, store.batchInEdges)
	}
}
