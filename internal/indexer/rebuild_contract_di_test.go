package indexer

import (
	"errors"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/zzet/gortex/internal/contracts"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/graph/store_pebble"
)

func TestRebuildSpringBeanCallsNativePairs(t *testing.T) {
	const repo, java = "repo", "java"
	ctx := t.Context()
	native := graph.New()
	idx := newTestIndexer(native)
	idx.repoPrefix = repo
	t.Cleanup(idx.Close)
	b, err := store_pebble.OpenBuild(t.TempDir(), store_pebble.FullProfile)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, b.Close()) })
	method := func(path, cls, name, params, scope, language string) *graph.Node {
		return &graph.Node{
			ID: path + "::" + cls + "." + name, Name: name, Kind: graph.KindMethod,
			FilePath: path, RepoPrefix: scope, Language: language,
			Meta: map[string]any{"params_src": params, "receiver": cls},
		}
	}
	first := &graph.Node{ID: "repo/A.java::A", Name: "A", Kind: graph.KindType, RepoPrefix: repo, Language: java, FilePath: "repo/A.java"}
	last := &graph.Node{ID: "repo/Z.java::Z", Name: "Z", Kind: graph.KindType, RepoPrefix: repo, Language: java, FilePath: "repo/Z.java"}
	owner := &graph.Node{ID: "repo/Client.java::Client", Name: "Client", Kind: graph.KindType, RepoPrefix: repo, Language: java, FilePath: "repo/Client.java"}
	factory := method(first.FilePath, first.Name, "make", "Service upstream", repo, java)
	nodes := []*graph.Node{
		first, last, owner, factory,
		method(owner.FilePath, owner.Name, "a", "List<Service> services", repo, java),
		method(owner.FilePath, owner.Name, "b", "Service service", repo, java),
		method("repo/Near.java", "Near", "ctor", "ServiceImpl service", repo, java),
		method("other/Foreign.java", "Foreign", "ctor", "Service service", "other", java),
		method("repo/Other.kt", "Other", "ctor", "Service service", repo, "kotlin"),
	}
	bean := func(from *graph.Node, path string, line int) *graph.Edge {
		return &graph.Edge{From: from.ID, To: factory.ID, Kind: graph.EdgeProvides, FilePath: path, Line: line,
			Meta: map[string]any{graph.MetaDIBinding: graph.DIBindingBean, graph.MetaDIProvidesFor: "Service"}}
	}
	native.AddBatch(nodes, nil)
	b.AddBatch(nodes, nil)
	unexpected := func(*graph.Edge) error {
		t.Fatal("a graph without bean declarations returned a Call")
		return nil
	}
	require.NoError(t, visitRebuildSpringBeanCalls(ctx, b, repo, unexpected))
	edges := []*graph.Edge{bean(last, last.FilePath, 3), bean(first, first.FilePath, 12), bean(first, first.FilePath, 5),
		{From: owner.ID, To: factory.ID, Kind: graph.EdgeConsumes, FilePath: owner.FilePath, Line: 2,
			Meta: map[string]any{"via": "@Inject", graph.MetaDIToken: "Service"}},
	}
	native.AddBatch(nil, edges)
	b.AddBatch(nil, edges)
	idx.extractDIContracts(contracts.NewRegistry())
	var want []*graph.Edge
	for e := range native.EdgesByKind(graph.EdgeCalls) {
		want = append(want, e)
	}
	require.Len(t, want, 1)
	require.Equal(t, owner.ID, want[0].From)
	require.Equal(t, first.FilePath, want[0].FilePath)
	require.Equal(t, 5, want[0].Line)
	failure := errors.New("test sink failed")
	require.ErrorIs(t, visitRebuildSpringBeanCalls(ctx, b, repo, func(*graph.Edge) error { return failure }), failure)
	var got []*graph.Edge
	require.NoError(t, visitRebuildSpringBeanCalls(ctx, b, repo, func(e *graph.Edge) error {
		got = append(got, e)
		return nil
	}))
	require.Equal(t, want, got)
	require.NoError(t, b.RebuildView().VisitEdges(ctx, func(e *graph.Edge) error {
		require.NotEqual(t, graph.EdgeCalls, e.Kind, "Calls stay private to the candidate stream")
		return nil
	}))
}
