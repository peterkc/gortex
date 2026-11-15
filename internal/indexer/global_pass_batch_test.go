package indexer

import (
	"testing"

	"github.com/zzet/gortex/internal/graph"
)

type batchCountingStore struct {
	graph.Store
	addBatchCalls int
	addEdgeCalls  int
	batchNodes    int
	batchEdges    int
}

func (s *batchCountingStore) AddBatch(nodes []*graph.Node, edges []*graph.Edge) {
	s.addBatchCalls++
	s.batchNodes += len(nodes)
	s.batchEdges += len(edges)
	s.Store.AddBatch(nodes, edges)
}

func (s *batchCountingStore) AddEdge(edge *graph.Edge) {
	s.addEdgeCalls++
	s.Store.AddEdge(edge)
}

func TestEmitTestEdgesUsesSingleBatchWrite(t *testing.T) {
	base := graph.New()
	base.AddBatch(
		[]*graph.Node{
			{ID: "repo::test", Kind: graph.KindFunction, RepoPrefix: "repo"},
			{ID: "repo::prod", Kind: graph.KindFunction, RepoPrefix: "repo"},
		},
		[]*graph.Edge{{From: "repo::test", To: "repo::prod", Kind: graph.EdgeCalls}},
	)
	store := &batchCountingStore{Store: base}

	if got := emitTestEdgesLocked(store, map[string]bool{"repo::test": true}, nil); got != 1 {
		t.Fatalf("emitted edges = %d, want 1", got)
	}
	if store.addBatchCalls != 1 || store.batchEdges != 1 {
		t.Fatalf("batch writes = %d calls/%d edges, want 1/1", store.addBatchCalls, store.batchEdges)
	}
	if store.addEdgeCalls != 0 {
		t.Fatalf("per-edge writes = %d, want 0", store.addEdgeCalls)
	}
}

func TestCapabilityEdgesUseSingleBatchWrite(t *testing.T) {
	for _, tc := range []struct {
		name string
		run  func(graph.Store) (int, int, int, error)
	}{
		{
			name: "global",
			run:  synthesizeCapabilityEdges,
		},
		{
			name: "scoped",
			run: func(store graph.Store) (int, int, int, error) {
				return synthesizeCapabilityEdgesScoped(store, map[string]bool{"repo": true})
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			base := graph.New()
			base.AddBatch(
				[]*graph.Node{{ID: "repo::source", Kind: graph.KindFunction, RepoPrefix: "repo"}},
				[]*graph.Edge{{
					From: "repo::source", To: "cfg::env::TOKEN", Kind: graph.EdgeReadsConfig,
				}},
			)
			store := &batchCountingStore{Store: base}

			readsEnv, execProc, fieldAccess, err := tc.run(store)
			if err != nil {
				t.Fatal(err)
			}
			if readsEnv != 1 || execProc != 0 || fieldAccess != 0 {
				t.Fatalf("counts = (%d, %d, %d), want (1, 0, 0)", readsEnv, execProc, fieldAccess)
			}
			if store.addBatchCalls != 1 || store.batchEdges != 1 {
				t.Fatalf("batch writes = %d calls/%d edges, want 1/1", store.addBatchCalls, store.batchEdges)
			}
			if store.addEdgeCalls != 0 {
				t.Fatalf("per-edge writes = %d, want 0", store.addEdgeCalls)
			}
		})
	}
}
