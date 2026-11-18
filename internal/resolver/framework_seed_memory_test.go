package resolver

import (
	"iter"
	"reflect"
	"slices"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/zzet/gortex/internal/graph"
)

type frameworkCandidateYieldCounter struct {
	*frameworkScopeTrapStore
	yields int
}

func (s *frameworkCandidateYieldCounter) EdgesInScopeSeq(repos, files []string, kinds ...graph.EdgeKind) iter.Seq[graph.ScopedEdgeRow] {
	base := graph.EdgesInScopeSeq(s.Store, repos, files, kinds...)
	return func(yield func(graph.ScopedEdgeRow) bool) {
		for row := range base {
			s.yields++
			if !yield(row) {
				return
			}
		}
	}
}

func TestFrameworkCandidateGateDoesNotRetainWholeKind(t *testing.T) {
	// Given one matching call followed by unrelated calls, when the gate
	// finds its witness, then it neither yields nor caches the remaining kind.
	g := graph.New()
	g.AddNode(frameworkTestNode("repo", "changed.go", "caller", graph.KindFunction, "caller", "go", nil))
	g.AddEdge(&graph.Edge{From: "caller", To: "unresolved::stub", Kind: graph.EdgeCalls, FilePath: "changed.go", Line: 1, Meta: map[string]any{"via": "grpc.stub"}})
	for line := 2; line <= 100; line++ {
		g.AddEdge(&graph.Edge{From: "caller", To: "unresolved::ordinary", Kind: graph.EdgeCalls, FilePath: "changed.go", Line: line})
	}
	store := &frameworkCandidateYieldCounter{frameworkScopeTrapStore: &frameworkScopeTrapStore{Store: g}}
	seed := newFrameworkDeclaredSeed(store, map[string]bool{"repo": true}, []string{"changed.go"})
	has, witness := seed.passCandidate(SynthGRPCStub)
	require.True(t, has)
	require.Equal(t, 1, witness.Line)
	requireNoFrameworkWholeKindCache(t, seed)
	require.Equal(t, 1, store.yields)
	require.Equal(t, 2, seed.rows, "one file node plus the gate witness")
	// A later declared name reader still gets the whole projection.
	require.Contains(t, seed.declaredNames(frameworkSynthSeedDeclarationFor(SynthGRPCStub)), "stub")
	require.Equal(t, 101, store.yields)
	require.Equal(t, 102, seed.rows, "name extraction counts the complete stream")
}

func TestFrameworkIncomingSeedOmitsSourceOwnedRows(t *testing.T) {
	// Given incoming sources both inside and outside the changed file, when
	// the seed loads them, then source-owned duplicates are not retained.
	for _, backend := range frameworkExactEdgeBackends() {
		for _, mode := range []string{"base", "delta"} {
			t.Run(backend.name+"/"+mode, func(t *testing.T) {
				store := backend.open(t)
				store.AddBatch([]*graph.Node{
					frameworkTestNode("repo", "changed.go", "target", graph.KindMethod, "Target", "go", nil),
					frameworkTestNode("repo", "changed.go", "inside", graph.KindFunction, "Inside", "go", nil),
					frameworkTestNode("other", "outside.go", "outside", graph.KindFunction, "Outside", "go", nil),
				}, []*graph.Edge{
					{From: "inside", To: "target", Kind: graph.EdgeCalls, FilePath: "misleading.go", Line: 1,
						Meta: map[string]any{"via": "grpc.stub", "grpc_service": "InsideService", "grpc_method": "Target"}},
					{From: "inside", To: "target", Kind: graph.EdgeCalls, Line: 2,
						Meta: map[string]any{"via": "grpc.stub", "grpc_service": "InsideService", "grpc_method": "Target"}},
					{From: "outside", To: "target", Kind: graph.EdgeCalls, FilePath: "changed.go", Line: 3},
				})
				if mode == "delta" {
					store = graph.NewDeltaWriter(store, nil)
				}
				incomingView := make(map[graph.EdgeIdentity]*graph.Edge)
				for _, edge := range store.GetInEdges("target") {
					if edge.From == "inside" {
						incomingView[graph.EdgeIdentityFor(edge)] = edge
					}
				}
				sourceView := make(map[graph.EdgeIdentity]*graph.Edge)
				for row := range graph.EdgesInScopeSeq(store, []string{"repo"}, []string{"changed.go"}, graph.EdgeCalls) {
					sourceView[graph.EdgeIdentityFor(row.Edge)] = row.Edge
				}
				require.Len(t, incomingView, 2)
				require.Equal(t, incomingView, sourceView)
				seed := newFrameworkDeclaredSeed(store, map[string]bool{"repo": true}, []string{"changed.go", "changed.go"})
				incoming := seed.incomingOf(graph.EdgeCalls)
				require.Len(t, incoming, 1)
				require.Equal(t, "outside", incoming[0].From)
				has, witness := seed.passCandidate(SynthGRPCStub)
				require.True(t, has, "the skipped inside rows must still reach the candidate gate")
				require.Equal(t, "inside", witness.From)
				require.Contains(t, seed.declaredNames(frameworkSynthSeedDeclarationFor(SynthGRPCStub)), "InsideService")
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
}

func TestFrameworkCandidateGateScansCompleteMissAndNoFileScope(t *testing.T) {
	// Given a no-candidate file or a repo-only scope, when the gate runs,
	// then it reads the whole file to reject, but never widens to the repo.
	store := &frameworkCandidateYieldCounter{frameworkScopeTrapStore: &frameworkScopeTrapStore{Store: declaredSeedFixture(4)}}
	seed := newFrameworkDeclaredSeed(store, map[string]bool{"repo": true}, []string{"repo/config.go"})
	has, witness := seed.passCandidate(SynthFactoryChain)
	require.False(t, has)
	require.Nil(t, witness)
	require.Equal(t, 1, store.yields)
	requireNoFrameworkWholeKindCache(t, seed)
	store.yields = 0
	seed = newFrameworkDeclaredSeed(store, map[string]bool{"repo": true}, nil)
	has, witness = seed.passCandidate(SynthFactoryChain)
	require.False(t, has)
	require.Nil(t, witness)
	require.Zero(t, store.yields)
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

// Inspect the seed, not a backend-owned edge: Graph itself owns its rows.
func requireNoFrameworkWholeKindCache(t *testing.T, seed *frameworkDeclaredSeed) {
	t.Helper()
	cache := reflect.ValueOf(seed).Elem().FieldByName("fileEdges")
	if cache.IsValid() {
		require.Zero(t, cache.Len(), "whole-kind rows survived the declared pass")
	}
}

func TestFrameworkDeclaredRegistryPassDoesNotRetainWholeKind(t *testing.T) {
	// Given real declared-name passes, when the registry reaches its next
	// pass, then no whole-kind input remains on the shared seed.
	for _, fixture := range []struct {
		name  string
		build func() *graph.Graph
		file  string
	}{
		{SynthGRPCStub, buildGRPCHandlerSideScopedFixture, "a/server.go"},
		{SynthStoreFactory, buildStoreFactoryScopedFixture, "a/caller.ts"},
	} {
		t.Run(fixture.name, func(t *testing.T) {
			seed := newFrameworkDeclaredSeed(fixture.build(), map[string]bool{"a": true}, []string{fixture.file})
			var completed bool
			for _, synth := range defaultFrameworkSynthesizers() {
				if completed {
					requireNoFrameworkWholeKindCache(t, seed)
					// This later gate must not recreate a retained Calls bucket.
					seed.passCandidate(SynthFactoryChain)
					requireNoFrameworkWholeKindCache(t, seed)
					break
				}
				if synth.Name() != fixture.name {
					continue
				}
				has, _ := seed.passCandidate(synth.Name())
				require.True(t, has)
				n, err := synth.Synthesize(seed.passStore(synth.Name()))
				require.NoError(t, err)
				require.Positive(t, n, "the declared-name pass must reach its write boundary")
				require.NotEmpty(t, seed.names, "the pass must actually read declared names")
				completed = true
			}
			require.True(t, completed)
		})
	}
}

type frameworkSeedMutationStore struct {
	*frameworkScopeTrapStore
	mutations int
}

func (s *frameworkSeedMutationStore) ReindexEdges(batch []graph.EdgeReindex) {
	s.Store.ReindexEdges(batch)
	if s.mutations != 0 || len(batch) == 0 {
		return
	}
	s.mutations++
	for _, edge := range s.Store.GetOutEdges("noise") {
		switch edge.Line {
		case 1:
			old := edge.To
			edge.To = "unresolved::moved"
			edge.Meta["changed"] = true
			s.Store.ReindexEdges([]graph.EdgeReindex{{Edge: edge, OldTo: old}})
		case 2:
			graph.RemoveEdgesExact(s.Store, []*graph.Edge{edge})
		}
	}
}

func TestFrameworkSeedRegistryWriteBoundaryMatchesFullRun(t *testing.T) {
	// Given an earlier gRPC write and same-kind reindex/deletion, when later
	// registry passes run, then scoped output equals independent full output.
	for _, backend := range frameworkExactEdgeBackends() {
		t.Run(backend.name, func(t *testing.T) {
			results := make([]map[graph.EdgeIdentity]graph.Edge, 0, 2)
			for _, scoped := range []bool{false, true} {
				store := backend.open(t)
				fixture := buildGRPCHandlerSideScopedFixture()
				var nodes []*graph.Node
				var edges []*graph.Edge
				for _, node := range fixture.AllNodes() {
					if node.RepoPrefix == "a" {
						nodes = append(nodes, node)
					}
				}
				for _, edge := range fixture.AllEdges() {
					if node := fixture.GetNode(edge.From); node != nil && node.RepoPrefix == "a" {
						edges = append(edges, edge)
					}
				}
				store.AddBatch(nodes, edges)
				store.AddNode(frameworkTestNode("a", "a/server.go", "noise", graph.KindFunction, "noise", "go", nil))
				for line := 1; line <= 2; line++ {
					store.AddEdge(&graph.Edge{From: "noise", To: "unresolved::noise", Kind: graph.EdgeCalls,
						FilePath: "a/server.go", Line: line, Meta: map[string]any{"marker": true}})
				}
				factory, _ := factoryChainPriorGraph()
				for _, node := range factory.AllNodes() {
					node.RepoPrefix = "a"
					store.AddNode(node)
				}
				store.AddBatch(nil, factory.AllEdges())
				mutating := &frameworkSeedMutationStore{frameworkScopeTrapStore: &frameworkScopeTrapStore{Store: store}}
				var rep FrameworkSynthReport
				if scoped {
					rep = RunFrameworkSynthesizersScopedForFiles(mutating, map[string]bool{"a": true}, []string{"a/server.go", "a.go", "main.go"}, false)
				} else {
					rep = RunFrameworkSynthesizers(mutating)
				}
				require.NoError(t, rep.Err)
				require.Equal(t, 1, mutating.mutations, "the earlier pass must reach its write boundary")
				remaining := store.GetOutEdges("noise")
				require.Len(t, remaining, 1)
				require.Equal(t, "unresolved::moved", remaining[0].To)
				result := make(map[graph.EdgeIdentity]graph.Edge)
				for _, edge := range store.AllEdges() {
					if _, synthesized := edge.Meta[MetaSynthesizedBy]; synthesized {
						result[graph.EdgeIdentityFor(edge)] = *edge
					}
				}
				require.NotEmpty(t, result)
				results = append(results, result)
			}
			require.Equal(t, results[0], results[1], "compare complete payloads, not only identities")
		})
	}
}

func TestFrameworkPopulatedDeltaIncomingMatchesSourceProjection(t *testing.T) {
	// Given an inside row reindexed in a populated delta, when both views
	// read it, then they agree on the full payload and omit the old target.
	for _, backend := range frameworkExactEdgeBackends() {
		t.Run(backend.name, func(t *testing.T) {
			base := backend.open(t)
			base.AddBatch([]*graph.Node{
				frameworkTestNode("repo", "changed.go", "inside", graph.KindFunction, "Inside", "go", nil),
				frameworkTestNode("repo", "changed.go", "old", graph.KindMethod, "Old", "go", nil),
				frameworkTestNode("repo", "changed.go", "target", graph.KindMethod, "Target", "go", nil),
			}, []*graph.Edge{{From: "inside", To: "old", Kind: graph.EdgeCalls, FilePath: "misleading.go", Line: 7,
				Meta: map[string]any{"via": "grpc.stub", "grpc_service": "InsideService", "grpc_method": "Target"}}})
			store := graph.NewDeltaWriter(base, nil)
			edge := store.GetOutEdges("inside")[0]
			edge.To = "target"
			edge.Meta["reindexed"] = true
			store.ReindexEdges([]graph.EdgeReindex{{Edge: edge, OldTo: "old"}})
			require.Empty(t, store.GetInEdges("old"))
			incoming := store.GetInEdges("target")
			require.Len(t, incoming, 1)
			var projected []*graph.Edge
			for row := range graph.EdgesInScopeSeq(store, []string{"repo"}, []string{"changed.go"}, graph.EdgeCalls) {
				projected = append(projected, row.Edge)
			}
			require.Equal(t, incoming, projected)
			seed := newFrameworkDeclaredSeed(store, map[string]bool{"repo": true}, []string{"changed.go"})
			require.Empty(t, seed.incomingOf(graph.EdgeCalls))
			has, _ := seed.passCandidate(SynthGRPCStub)
			require.True(t, has)
			require.Contains(t, seed.declaredNames(frameworkSynthSeedDeclarationFor(SynthGRPCStub)), "InsideService")
		})
	}
}
