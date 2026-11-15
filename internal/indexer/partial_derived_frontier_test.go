package indexer

import (
	"context"
	"fmt"
	"iter"
	"testing"

	"github.com/zzet/gortex/internal/graph"
)

type partialDerivedCountingStore struct {
	graph.Store
	nodesInFilesCalls   int
	nodesInFilesRows    int
	fileBatchCalls      int
	fileBatchRows       int
	outBatchCalls       int
	inBatchCalls        int
	nodeBatchCalls      int
	nodeBatchRows       int
	nameBatchCalls      int
	nodesByKindCalls    int
	edgesByKindCalls    int
	scopedEvictionCalls int
}

func (c *partialDerivedCountingStore) NodesInFilesByKind(files []string, kinds []graph.NodeKind) []*graph.Node {
	c.nodesInFilesCalls++
	rows := c.Store.(graph.NodesInFilesByKindFinder).NodesInFilesByKind(files, kinds)
	c.nodesInFilesRows += len(rows)
	return rows
}

func (c *partialDerivedCountingStore) GetFileNodesByPaths(files []string) map[string][]*graph.Node {
	c.fileBatchCalls++
	rows := c.Store.GetFileNodesByPaths(files)
	for _, nodes := range rows {
		c.fileBatchRows += len(nodes)
	}
	return rows
}

func (c *partialDerivedCountingStore) GetOutEdgesByNodeIDs(ids []string) map[string][]*graph.Edge {
	c.outBatchCalls++
	return c.Store.GetOutEdgesByNodeIDs(ids)
}

func (c *partialDerivedCountingStore) GetInEdgesByNodeIDs(ids []string) map[string][]*graph.Edge {
	c.inBatchCalls++
	return c.Store.GetInEdgesByNodeIDs(ids)
}

func (c *partialDerivedCountingStore) GetNodesByIDs(ids []string) map[string]*graph.Node {
	c.nodeBatchCalls++
	rows := c.Store.GetNodesByIDs(ids)
	c.nodeBatchRows += len(rows)
	return rows
}

func (c *partialDerivedCountingStore) FindNodesByNames(names []string) map[string][]*graph.Node {
	c.nameBatchCalls++
	return c.Store.FindNodesByNames(names)
}

func (c *partialDerivedCountingStore) NodesByKind(kind graph.NodeKind) iter.Seq[*graph.Node] {
	c.nodesByKindCalls++
	return c.Store.NodesByKind(kind)
}

func (c *partialDerivedCountingStore) EdgesByKind(kind graph.EdgeKind) iter.Seq[*graph.Edge] {
	c.edgesByKindCalls++
	return c.Store.EdgesByKind(kind)
}

func (c *partialDerivedCountingStore) EvictEdgesFromSourcesByKinds(
	ctx context.Context,
	sources []string,
	kinds []graph.EdgeKind,
) (int, error) {
	c.scopedEvictionCalls++
	return c.Store.(graph.ScopedEdgeKindEvicter).EvictEdgesFromSourcesByKinds(ctx, sources, kinds)
}

func addUnrelatedPartialScale(g *graph.Graph, count int) {
	nodes := make([]*graph.Node, 0, count*2)
	for i := 0; i < count; i++ {
		file := fmt.Sprintf("other::f%05d.go", i)
		nodes = append(nodes,
			&graph.Node{ID: file + "::M", Kind: graph.KindMethod, Name: "M", RepoPrefix: "other", FilePath: file, Meta: map[string]any{"receiver": "Noise"}},
			&graph.Node{ID: file + "::v", Kind: graph.KindField, Name: "v", RepoPrefix: "other", FilePath: file, Meta: map[string]any{"receiver": "Noise"}},
		)
	}
	g.AddBatch(nodes, nil)
}

func testFrontierFixture(scale int) *graph.Graph {
	g := graph.New()
	changedFile := "repoA::changed_test.go"
	callerFile := "repoB::caller_test.go"
	targetTestFile := "repoB::target_test.go"
	prodFile := "repoB::prod.go"
	g.AddBatch([]*graph.Node{
		{ID: changedFile, Kind: graph.KindFile, RepoPrefix: "repoA", FilePath: changedFile, Language: "go"},
		{ID: changedFile + "::TestChanged", Kind: graph.KindFunction, Name: "TestChanged", RepoPrefix: "repoA", FilePath: changedFile, Language: "go"},
		{ID: callerFile, Kind: graph.KindFile, RepoPrefix: "repoB", FilePath: callerFile, Language: "go"},
		{ID: callerFile + "::TestCaller", Kind: graph.KindFunction, Name: "TestCaller", RepoPrefix: "repoB", FilePath: callerFile, Language: "go", Meta: map[string]any{"is_test": true, "test_role": "test"}},
		{ID: targetTestFile, Kind: graph.KindFile, RepoPrefix: "repoB", FilePath: targetTestFile, Language: "go"},
		{ID: targetTestFile + "::TestTarget", Kind: graph.KindFunction, Name: "TestTarget", RepoPrefix: "repoB", FilePath: targetTestFile, Language: "go", Meta: map[string]any{"is_test": true, "test_role": "test"}},
		{ID: prodFile, Kind: graph.KindFile, RepoPrefix: "repoB", FilePath: prodFile, Language: "go"},
		{ID: prodFile + "::Production", Kind: graph.KindFunction, Name: "Production", RepoPrefix: "repoB", FilePath: prodFile, Language: "go"},
	}, []*graph.Edge{
		{From: changedFile + "::TestChanged", To: prodFile + "::Production", Kind: graph.EdgeCalls, FilePath: changedFile, Line: 10},
		{From: changedFile + "::TestChanged", To: targetTestFile + "::TestTarget", Kind: graph.EdgeCalls, FilePath: changedFile, Line: 11},
		{From: callerFile + "::TestCaller", To: changedFile + "::TestChanged", Kind: graph.EdgeCalls, FilePath: callerFile, Line: 20},
		{From: callerFile + "::TestCaller", To: prodFile + "::Production", Kind: graph.EdgeCalls, FilePath: callerFile, Line: 21},
		// This stale edge proves the incoming caller neighborhood is reconciled.
		{From: callerFile + "::TestCaller", To: changedFile + "::TestChanged", Kind: graph.EdgeTests, FilePath: callerFile, Line: 20},
	})
	addUnrelatedPartialScale(g, scale)
	return g
}

func testEdgesForSources(g graph.Store, sources ...string) map[string]bool {
	rows := g.GetOutEdgesByNodeIDs(sources)
	out := make(map[string]bool)
	for _, source := range sources {
		for _, edge := range rows[source] {
			if edge != nil && edge.Kind == graph.EdgeTests {
				out[edge.From+"->"+edge.To] = true
			}
		}
	}
	return out
}

func TestPartialTestEdgesUseExactFileAndIncomingCallerFrontier(t *testing.T) {
	const changedFile = "repoA::changed_test.go"
	const changedTest = changedFile + "::TestChanged"
	const caller = "repoB::caller_test.go::TestCaller"
	full := testFrontierFixture(0)
	graph.EvictEdgesByKinds(full, []graph.EdgeKind{graph.EdgeTests})
	markTestSymbolsAndEmitEdges(full)
	want := testEdgesForSources(full, changedTest, caller)

	counted := &partialDerivedCountingStore{Store: testFrontierFixture(1500)}
	marked, emitted := markTestSymbolsAndEmitEdgesScoped(
		counted, map[string]bool{"repoA": true}, changedFile,
	)
	got := testEdgesForSources(counted.Store, changedTest, caller)
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("partial test edges = %v, want full-pass parity %v", got, want)
	}
	if marked != 1 || emitted != 2 {
		t.Fatalf("partial counts = (%d marked, %d emitted), want (1, 2)", marked, emitted)
	}
	if got[caller+"->"+changedTest] {
		t.Fatalf("test-to-test caller edge survived reconciliation: %v", got)
	}
	if counted.nodesByKindCalls != 0 || counted.edgesByKindCalls != 0 {
		t.Fatalf("partial pass used global scans: nodes=%d edges=%d", counted.nodesByKindCalls, counted.edgesByKindCalls)
	}
	if counted.nodesInFilesCalls != 1 || counted.nodesInFilesRows > 3 {
		t.Fatalf("file projection = %d calls/%d rows, want one tiny frontier", counted.nodesInFilesCalls, counted.nodesInFilesRows)
	}
	if counted.inBatchCalls != 1 || counted.scopedEvictionCalls != 1 {
		t.Fatalf("incoming/eviction batches = %d/%d, want 1/1", counted.inBatchCalls, counted.scopedEvictionCalls)
	}
	if counted.nodeBatchRows > 12 {
		t.Fatalf("partial endpoint materialisation grew beyond frontier: %d rows", counted.nodeBatchRows)
	}
}

func capabilityFrontierFixture(withWrite bool, scale int) *graph.Graph {
	g := graph.New()
	changedFile := "repoA::changed.go"
	callerFile := "repoA::caller.go"
	fieldFile := "repoA::state.go"
	helper := changedFile + "::State.helper"
	caller := callerFile + "::State.run"
	field := fieldFile + "::State.value"
	nodes := []*graph.Node{
		{ID: changedFile, Kind: graph.KindFile, RepoPrefix: "repoA", FilePath: changedFile, Language: "go"},
		{ID: helper, Kind: graph.KindMethod, Name: "helper", RepoPrefix: "repoA", FilePath: changedFile, Language: "go", Meta: map[string]any{"receiver": "State"}},
		{ID: callerFile, Kind: graph.KindFile, RepoPrefix: "repoA", FilePath: callerFile, Language: "go"},
		{ID: caller, Kind: graph.KindMethod, Name: "run", RepoPrefix: "repoA", FilePath: callerFile, Language: "go", Meta: map[string]any{"receiver": "State"}},
		{ID: field, Kind: graph.KindField, Name: "value", RepoPrefix: "repoA", FilePath: fieldFile, Language: "go", Meta: map[string]any{"receiver": "State"}},
		{ID: "cfg::env::TOKEN", Kind: graph.KindConfigKey, Name: "TOKEN"},
	}
	edges := []*graph.Edge{
		{From: caller, To: helper, Kind: graph.EdgeCalls, FilePath: callerFile, Line: 10, Meta: map[string]any{"recv_self": true}},
		{From: helper, To: "cfg::env::TOKEN", Kind: graph.EdgeReadsConfig, FilePath: changedFile, Line: 20},
		{From: helper, To: "unresolved::exec.Command", Kind: graph.EdgeCalls, FilePath: changedFile, Line: 21},
	}
	if withWrite {
		edges = append(edges, &graph.Edge{From: helper, To: field, Kind: graph.EdgeWrites, FilePath: changedFile, Line: 22})
	}
	g.AddBatch(nodes, edges)
	addUnrelatedPartialScale(g, scale)
	return g
}

func capabilityEdgesForSources(g graph.Store, sources ...string) map[string]bool {
	rows := g.GetOutEdgesByNodeIDs(sources)
	out := make(map[string]bool)
	for _, source := range sources {
		for _, edge := range rows[source] {
			if edge == nil || (edge.Kind != graph.EdgeReadsEnv && edge.Kind != graph.EdgeExecutesProcess && edge.Kind != graph.EdgeAccessesField) {
				continue
			}
			via := ""
			if edge.Meta != nil {
				via, _ = edge.Meta["via"].(string)
			}
			out[string(edge.Kind)+":"+edge.From+"->"+edge.To+":"+via] = true
		}
	}
	return out
}

func TestPartialCapabilityEdgesReconcileBoundedReceiverFrontier(t *testing.T) {
	const changedFile = "repoA::changed.go"
	const helper = changedFile + "::State.helper"
	const caller = "repoA::caller.go::State.run"
	oracle := capabilityFrontierFixture(false, 0)
	if _, _, _, err := synthesizeCapabilityEdges(oracle); err != nil {
		t.Fatal(err)
	}
	want := capabilityEdgesForSources(oracle, helper, caller)

	fixture := capabilityFrontierFixture(true, 1500)
	if _, _, _, err := synthesizeCapabilityEdges(fixture); err != nil {
		t.Fatal(err)
	}
	if before := capabilityEdgesForSources(fixture, helper, caller); len(before) <= len(want) {
		t.Fatalf("fixture did not create stale direct/transitive capabilities: before=%v want=%v", before, want)
	}
	fixture.RemoveEdge(helper, "repoA::state.go::State.value", graph.EdgeWrites)
	counted := &partialDerivedCountingStore{Store: fixture}
	if _, _, _, err := synthesizeCapabilityEdgesScoped(counted, map[string]bool{"repoA": true}, changedFile); err != nil {
		t.Fatal(err)
	}
	got := capabilityEdgesForSources(counted.Store, helper, caller)
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("partial capabilities = %v, want full-pass parity %v", got, want)
	}
	if counted.nodesByKindCalls != 0 || counted.edgesByKindCalls != 0 {
		t.Fatalf("partial capability used global scans: nodes=%d edges=%d", counted.nodesByKindCalls, counted.edgesByKindCalls)
	}
	if counted.fileBatchCalls != 1 || counted.fileBatchRows > 3 {
		t.Fatalf("changed-file read = %d calls/%d rows, want one tiny frontier", counted.fileBatchCalls, counted.fileBatchRows)
	}
	if counted.scopedEvictionCalls != 1 {
		t.Fatalf("scoped evictions = %d, want 1", counted.scopedEvictionCalls)
	}
	if counted.nodeBatchRows > 20 {
		t.Fatalf("partial endpoint materialisation grew beyond receiver frontier: %d rows", counted.nodeBatchRows)
	}
}
