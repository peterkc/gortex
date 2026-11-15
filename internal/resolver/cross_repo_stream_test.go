package resolver

import (
	"testing"

	"github.com/zzet/gortex/internal/graph"
)

func crossRepoStreamFixture(edgeCount int) (*graph.Graph, []*graph.Edge) {
	store := graph.New()
	callerID := "repoA/pkg/a.go::Caller"
	targetID := "repoB/lib/b.go::Helper"
	store.AddBatch([]*graph.Node{
		{ID: callerID, Kind: graph.KindFunction, Name: "Caller", FilePath: "repoA/pkg/a.go", Language: "go", RepoPrefix: "repoA"},
		{ID: targetID, Kind: graph.KindFunction, Name: "Helper", FilePath: "repoB/lib/b.go", Language: "go", RepoPrefix: "repoB"},
	}, nil)
	wireImport(store, "repoA/pkg/a.go", "repoB", "repoB/lib/b.go")
	edges := make([]*graph.Edge, 0, edgeCount)
	for i := 0; i < edgeCount; i++ {
		edges = append(edges, &graph.Edge{
			From: callerID, To: "unresolved::Helper", Kind: graph.EdgeCalls,
			FilePath: "repoA/pkg/a.go", Line: i + 10,
		})
	}
	store.AddBatch(nil, edges)
	return store, edges
}

func TestCrossRepoResolveAllStreamsWithScopedParityAndBoundedPeak(t *testing.T) {
	const edgeCount = resolvePendingPageRows*2 + 37
	streamedStore, streamedEdges := crossRepoStreamFixture(edgeCount)
	oracleStore, oracleEdges := crossRepoStreamFixture(edgeCount)

	streamed := NewCrossRepo(streamedStore).ResolveAll()
	oracle := NewCrossRepo(oracleStore).ResolveForRepo("repoA")

	if streamed.Resolved != oracle.Resolved || streamed.Unresolved != oracle.Unresolved ||
		streamed.CrossRepoEdges != oracle.CrossRepoEdges {
		t.Fatalf("streamed stats=%+v, scoped oracle=%+v", streamed, oracle)
	}
	if streamed.Resolved != edgeCount || streamed.CrossRepoEdges != edgeCount {
		t.Fatalf("resolved=%d cross_repo=%d, want %d each", streamed.Resolved, streamed.CrossRepoEdges, edgeCount)
	}
	for i := range streamedEdges {
		if streamedEdges[i].To != oracleEdges[i].To || streamedEdges[i].CrossRepo != oracleEdges[i].CrossRepo {
			t.Fatalf("edge %d diverged: streamed=%#v oracle=%#v", i, streamedEdges[i], oracleEdges[i])
		}
	}
	if streamed.peakPendingPage > resolvePendingPageRows {
		t.Fatalf("peak pending page=%d, bound=%d", streamed.peakPendingPage, resolvePendingPageRows)
	}
	if streamed.peakLookupKeys >= edgeCount {
		t.Fatalf("lookup cache retained %d keys for %d repeated-name edges", streamed.peakLookupKeys, edgeCount)
	}

	crossKind, _ := graph.CrossRepoKindFor(graph.EdgeCalls)
	parallel := 0
	for range streamedStore.EdgesByKind(crossKind) {
		parallel++
	}
	if parallel != edgeCount {
		t.Fatalf("parallel cross-repo edges=%d, want %d", parallel, edgeCount)
	}
}

func TestDetectCrossRepoEdgesForReindexesScopesToBatch(t *testing.T) {
	store := graph.New()
	store.AddBatch([]*graph.Node{
		{ID: "a::caller", Kind: graph.KindFunction, Name: "caller", RepoPrefix: "a"},
		{ID: "b::target", Kind: graph.KindFunction, Name: "target", RepoPrefix: "b"},
		{ID: "c::untouched", Kind: graph.KindFunction, Name: "untouched", RepoPrefix: "c"},
	}, nil)
	changed := &graph.Edge{From: "a::caller", To: "b::target", Kind: graph.EdgeCalls, FilePath: "a.go", Line: 1}
	untouched := &graph.Edge{From: "a::caller", To: "c::untouched", Kind: graph.EdgeCalls, FilePath: "a.go", Line: 2}
	store.AddBatch(nil, []*graph.Edge{changed, untouched})

	if got, err := DetectCrossRepoEdgesForReindexes(store, []graph.EdgeReindex{{Edge: changed, OldTo: "unresolved::target"}}); err != nil || got != 1 {
		t.Fatalf("materialized=%d, want 1", got)
	}
	crossKind, _ := graph.CrossRepoKindFor(graph.EdgeCalls)
	var targets []string
	for edge := range store.EdgesByKind(crossKind) {
		targets = append(targets, edge.To)
	}
	if len(targets) != 1 || targets[0] != "b::target" {
		t.Fatalf("scoped parallel targets=%v, want only b::target", targets)
	}
}
