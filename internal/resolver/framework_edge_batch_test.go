package resolver

import (
	"fmt"
	"testing"

	"github.com/zzet/gortex/internal/graph"
)

type frameworkBatchTestStore struct {
	graph.Store
	addEdgeCalls, addBatchCalls                 int
	getNodeCalls, getNodesByIDsCalls            int
	getOutEdgesCalls, getOutEdgesByNodeIDsCalls int
	getInEdgesCalls, getInEdgesByNodeIDsCalls   int
	batchEdges                                  [][]*graph.Edge
	panicOnBatch                                any
}

func (s *frameworkBatchTestStore) AddEdge(e *graph.Edge) {
	s.addEdgeCalls++
	s.Store.AddEdge(e)
}
func (s *frameworkBatchTestStore) AddBatch(nodes []*graph.Node, edges []*graph.Edge) {
	s.addBatchCalls++
	s.batchEdges = append(s.batchEdges, append([]*graph.Edge(nil), edges...))
	if s.panicOnBatch != nil {
		panic(s.panicOnBatch)
	}
	s.Store.AddBatch(nodes, edges)
}
func (s *frameworkBatchTestStore) GetNode(id string) *graph.Node {
	s.getNodeCalls++
	return s.Store.GetNode(id)
}
func (s *frameworkBatchTestStore) GetNodesByIDs(ids []string) map[string]*graph.Node {
	s.getNodesByIDsCalls++
	return s.Store.GetNodesByIDs(ids)
}
func (s *frameworkBatchTestStore) GetOutEdges(id string) []*graph.Edge {
	s.getOutEdgesCalls++
	return s.Store.GetOutEdges(id)
}
func (s *frameworkBatchTestStore) GetOutEdgesByNodeIDs(ids []string) map[string][]*graph.Edge {
	s.getOutEdgesByNodeIDsCalls++
	return s.Store.GetOutEdgesByNodeIDs(ids)
}
func (s *frameworkBatchTestStore) GetInEdges(id string) []*graph.Edge {
	s.getInEdgesCalls++
	return s.Store.GetInEdges(id)
}
func (s *frameworkBatchTestStore) GetInEdgesByNodeIDs(ids []string) map[string][]*graph.Edge {
	s.getInEdgesByNodeIDsCalls++
	return s.Store.GetInEdgesByNodeIDs(ids)
}

func TestLegacyFrameworkBoundaryBatches100EdgesAndPreservesPayload(t *testing.T) {
	g := graph.New()
	counting := &frameworkBatchTestStore{Store: g}
	synth := synthFunc{name: "batch-test", fn: func(store graph.Store) int {
		for i := 0; i < 100; i++ {
			edge := &graph.Edge{
				From: "caller", To: fmt.Sprintf("callee-%03d", i), Kind: graph.EdgeCalls,
				FilePath: "caller.go", Line: i + 1,
				Meta: map[string]any{"ordinal": i, "nested": map[string]any{"state": "captured"}},
			}
			store.AddEdge(edge)
			if i == 0 {
				edge.Meta["nested"].(map[string]any)["state"] = "mutated-after-add"
			}
		}
		store.AddEdge(&graph.Edge{
			From: "caller", To: "callee-001", Kind: graph.EdgeCalls, FilePath: "caller.go", Line: 2,
			Meta: map[string]any{"ordinal": "last-write"},
		})
		return 101
	}}

	if got, err := synth.synthesizeScoped(counting, nil); err != nil || got != 101 {
		t.Fatalf("synth count = %d, %v, want 101, nil", got, err)
	}
	if counting.addEdgeCalls != 0 || counting.addBatchCalls != 1 {
		t.Fatalf("backend writes = AddEdge:%d AddBatch:%d, want 0/1", counting.addEdgeCalls, counting.addBatchCalls)
	}
	if len(counting.batchEdges) != 1 || len(counting.batchEdges[0]) != 100 {
		t.Fatalf("flushed %d batches / %d edges, want 1 / 100", len(counting.batchEdges), len(counting.batchEdges[0]))
	}
	if counting.getNodeCalls != 0 || counting.getOutEdgesCalls != 0 || counting.getInEdgesCalls != 0 {
		t.Fatalf("point reads leaked: node=%d out=%d in=%d", counting.getNodeCalls, counting.getOutEdgesCalls, counting.getInEdgesCalls)
	}
	byTarget := map[string]*graph.Edge{}
	for _, edge := range g.GetOutEdges("caller") {
		byTarget[edge.To] = edge
	}
	if len(byTarget) != 100 {
		t.Fatalf("stored edges = %d, want 100", len(byTarget))
	}
	if got := byTarget["callee-000"].Meta["nested"].(map[string]any)["state"]; got != "captured" {
		t.Fatalf("captured nested metadata = %#v, want captured", got)
	}
	if got := byTarget["callee-001"].Meta["ordinal"]; got != "last-write" {
		t.Fatalf("duplicate metadata = %#v, want last-write", got)
	}
}

func TestFrameworkEdgeBatchReadYourWritesAndOrdering(t *testing.T) {
	g := graph.New()
	counting := &frameworkBatchTestStore{Store: g}
	first := synthFunc{name: "first", fn: func(store graph.Store) int {
		edge := &graph.Edge{From: "one", To: "two", Kind: graph.EdgeCalls, FilePath: "x.go", Line: 7}
		store.AddEdge(edge)
		if g.EdgeCount() != 0 {
			t.Fatal("underlying store saw staged edge before flush")
		}
		out, in := store.GetOutEdges("one"), store.GetInEdges("two")
		outBatch := store.GetOutEdgesByNodeIDs([]string{"one"})["one"]
		inBatch := store.GetInEdgesByNodeIDs([]string{"two"})["two"]
		if len(out) != 1 || len(in) != 1 || len(outBatch) != 1 || len(inBatch) != 1 {
			t.Fatalf("staged adjacency missing: out=%d in=%d outBatch=%d inBatch=%d", len(out), len(in), len(outBatch), len(inBatch))
		}
		seen := 0
		for candidate := range store.EdgesByKind(graph.EdgeCalls) {
			if candidate != nil && graph.EdgeIdentityFor(candidate) == graph.EdgeIdentityFor(edge) {
				seen++
			}
		}
		if seen != 1 {
			t.Fatalf("predicate scan saw staged edge %d times, want 1", seen)
		}
		return 1
	}}
	second := synthFunc{name: "second", fn: func(store graph.Store) int {
		if got := store.GetOutEdges("one"); len(got) != 1 || got[0].To != "two" {
			t.Fatalf("second synthesizer did not observe first flush: %#v", got)
		}
		store.AddEdge(&graph.Edge{From: "two", To: "three", Kind: graph.EdgeCalls})
		return 1
	}}
	if got, err := first.synthesizeScoped(counting, nil); err != nil || got != 1 {
		t.Fatalf("first synth count = %d, %v, want 1, nil", got, err)
	}
	if got, err := second.synthesizeScoped(counting, nil); err != nil || got != 1 {
		t.Fatalf("second synth count = %d, %v, want 1, nil", got, err)
	}
	if counting.addBatchCalls != 2 || counting.addEdgeCalls != 0 || g.EdgeCount() != 2 {
		t.Fatalf("writes = AddBatch:%d AddEdge:%d stored:%d, want 2/0/2", counting.addBatchCalls, counting.addEdgeCalls, g.EdgeCount())
	}
	if counting.batchEdges[0][0].From != "one" || counting.batchEdges[1][0].From != "two" {
		t.Fatalf("batch order = %#v", counting.batchEdges)
	}
}

func TestFrameworkEdgeBatchIncomingIdentityProjectionMergesStaged(t *testing.T) {
	g := graph.New()
	shadowed := &graph.Edge{From: "one", To: "unresolved::target", Kind: graph.EdgeCalls, FilePath: "x.go", Line: 7}
	g.AddEdge(shadowed)
	batch := newFrameworkEdgeBatchStore(g)
	batch.AddEdge(&graph.Edge{
		From: shadowed.From, To: shadowed.To, Kind: shadowed.Kind,
		FilePath: shadowed.FilePath, Line: shadowed.Line, Meta: map[string]any{"staged": true},
	})
	staged := &graph.Edge{From: "two", To: shadowed.To, Kind: graph.EdgeReferences, FilePath: "y.go", Line: 8}
	batch.AddEdge(staged)

	got := batch.GetInEdgeIdentitiesByNodeIDs([]string{"", shadowed.To, shadowed.To, "missing"})[shadowed.To]
	want := map[graph.EdgeIdentity]struct{}{
		graph.EdgeIdentityFor(shadowed): {},
		graph.EdgeIdentityFor(staged):   {},
	}
	if len(got) != len(want) {
		t.Fatalf("incoming staged identities = %#v, want %d unique rows", got, len(want))
	}
	for _, identity := range got {
		if _, ok := want[identity]; !ok {
			t.Fatalf("unexpected incoming staged identity: %#v", identity)
		}
	}
}

func TestFrameworkPartialLegacyPathBatchesWrites(t *testing.T) {
	g := graph.New()
	counting := &frameworkBatchTestStore{Store: g}
	synth := synthFunc{name: "partial", fn: func(store graph.Store) int {
		store.AddEdge(&graph.Edge{From: "repo::one", To: "repo::two", Kind: graph.EdgeCalls})
		return 1
	}}
	if got, err := synth.synthesizeScoped(counting, map[string]bool{"repo": true}); err != nil || got != 1 {
		t.Fatalf("partial count = %d, %v, want 1, nil", got, err)
	}
	if counting.addEdgeCalls != 0 || counting.addBatchCalls != 1 || g.EdgeCount() != 1 {
		t.Fatalf("partial writes = AddEdge:%d AddBatch:%d stored:%d, want 0/1/1", counting.addEdgeCalls, counting.addBatchCalls, g.EdgeCount())
	}
}

func TestFrameworkEdgeBatchFailureSemantics(t *testing.T) {
	t.Run("synth panic discards staged edges", func(t *testing.T) {
		g := graph.New()
		counting := &frameworkBatchTestStore{Store: g}
		got := captureFrameworkPanic(func() {
			runLegacyFrameworkSynth(counting, func(store graph.Store) int {
				store.AddEdge(&graph.Edge{From: "one", To: "two", Kind: graph.EdgeCalls})
				panic("synth failed")
			})
		})
		if got != "synth failed" || counting.addBatchCalls != 0 || counting.addEdgeCalls != 0 || g.EdgeCount() != 0 {
			t.Fatalf("panic=%#v writes=%d/%d stored=%d", got, counting.addBatchCalls, counting.addEdgeCalls, g.EdgeCount())
		}
	})
	t.Run("flush panic propagates", func(t *testing.T) {
		g := graph.New()
		counting := &frameworkBatchTestStore{Store: g, panicOnBatch: "commit failed"}
		got := captureFrameworkPanic(func() {
			runLegacyFrameworkSynth(counting, func(store graph.Store) int {
				store.AddEdge(&graph.Edge{From: "one", To: "two", Kind: graph.EdgeCalls})
				return 1
			})
		})
		if got != "commit failed" || counting.addBatchCalls != 1 || counting.addEdgeCalls != 0 || g.EdgeCount() != 0 {
			t.Fatalf("panic=%#v writes=%d/%d stored=%d", got, counting.addBatchCalls, counting.addEdgeCalls, g.EdgeCount())
		}
	})
}

func TestMacroExpansionUsesConstantBatchReadsAndOneWrite(t *testing.T) {
	g := graph.New()
	macro := &graph.Node{ID: "repo::CALL", Kind: graph.KindMacro, Name: "CALL", RepoPrefix: "repo", FilePath: "macro.h", Meta: map[string]any{"macro_kind": macroFunctionKindMeta}}
	callee := &graph.Node{ID: "repo::target", Kind: graph.KindFunction, Name: "target", RepoPrefix: "repo", FilePath: "target.c"}
	nodes := []*graph.Node{macro, callee}
	edges := []*graph.Edge{{From: macro.ID, To: callee.ID, Kind: graph.EdgeCalls, FilePath: "macro.h", Line: 2}}
	for i := 0; i < 100; i++ {
		caller := &graph.Node{ID: fmt.Sprintf("repo::caller-%03d", i), Kind: graph.KindFunction, Name: fmt.Sprintf("caller_%03d", i), RepoPrefix: "repo", FilePath: fmt.Sprintf("caller_%03d.c", i)}
		nodes = append(nodes, caller)
		edges = append(edges, &graph.Edge{From: caller.ID, To: graph.UnresolvedMarker + macro.Name, Kind: graph.EdgeCalls, FilePath: caller.FilePath, Line: 10})
	}
	g.AddBatch(nodes, edges)

	counting := &frameworkBatchTestStore{Store: g}
	if got := runLegacyFrameworkSynth(counting, ResolveMacroExpansionCalls); got != 100 {
		t.Fatalf("macro edges = %d, want 100", got)
	}
	if counting.getNodeCalls != 0 || counting.getOutEdgesCalls != 0 || counting.getInEdgesCalls != 0 {
		t.Fatalf("point reads leaked: node=%d out=%d in=%d", counting.getNodeCalls, counting.getOutEdgesCalls, counting.getInEdgesCalls)
	}
	if counting.getNodesByIDsCalls != 1 || counting.getOutEdgesByNodeIDsCalls != 2 {
		t.Fatalf("batch reads = nodes:%d out:%d, want 1/2", counting.getNodesByIDsCalls, counting.getOutEdgesByNodeIDsCalls)
	}
	if counting.addEdgeCalls != 0 || counting.addBatchCalls != 1 || len(counting.batchEdges[0]) != 100 {
		t.Fatalf("writes = AddEdge:%d AddBatch:%d batch-size:%d, want 0/1/100", counting.addEdgeCalls, counting.addBatchCalls, len(counting.batchEdges[0]))
	}
	if g.EdgeCount() != 201 {
		t.Fatalf("stored edges = %d, want 201", g.EdgeCount())
	}
}

func TestFrameworkIdentityMapKeysDoNotCollideOnNUL(t *testing.T) {
	left := &graph.Edge{From: "a", To: "b\x00c", Kind: graph.EdgeCalls, FilePath: "x.go", Line: 7}
	right := &graph.Edge{From: "a\x00b", To: "c", Kind: graph.EdgeCalls, FilePath: "x.go", Line: 7}
	if graph.EdgeIdentityFor(left) == graph.EdgeIdentityFor(right) {
		t.Fatal("distinct edge identities collided")
	}

	g := graph.New()
	batch := newFrameworkEdgeBatchStore(g)
	batch.AddEdge(left)
	batch.AddEdge(right)
	if len(batch.staged) != 2 || len(batch.order) != 2 {
		t.Fatalf("batch identity entries = staged:%d ordered:%d, want 2/2", len(batch.staged), len(batch.order))
	}
	batch.flush()
	if g.EdgeCount() != 2 {
		t.Fatalf("stored edges = %d, want 2", g.EdgeCount())
	}

	view := &frameworkScopedStore{
		incidentSeen:   make(map[graph.EdgeIdentity]struct{}),
		incidentByKind: make(map[graph.EdgeKind][]*graph.Edge),
	}
	if !view.rememberEdge(left) || !view.rememberEdge(right) {
		t.Fatal("scoped store rejected identity collision fixtures")
	}
	if view.retainedRows != 2 || len(view.incidentSeen) != 2 {
		t.Fatalf("scoped retained identities = rows:%d seen:%d, want 2/2", view.retainedRows, len(view.incidentSeen))
	}
}

var frameworkIdentityBenchmarkSink bool

func BenchmarkFrameworkEdgeIdentityMapLookup(b *testing.B) {
	edge := &graph.Edge{
		From: "repo::internal/resolver.(*frameworkScopedStore).rememberEdge",
		To:   "repo::internal/graph.EdgeIdentityFor",
		Kind: graph.EdgeCalls, FilePath: "internal/resolver/framework_scoped_store.go", Line: 316,
	}
	index := map[graph.EdgeIdentity]struct{}{graph.EdgeIdentityFor(edge): {}}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_, frameworkIdentityBenchmarkSink = index[graph.EdgeIdentityFor(edge)]
	}
}

func captureFrameworkPanic(fn func()) (recovered any) {
	defer func() { recovered = recover() }()
	fn()
	return nil
}
