package indexer

import (
	"errors"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/zzet/gortex/internal/contracts"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/graph/store_pebble"
)

func TestRebuildSpringBeanCallsNativePairs(t *testing.T) {
	const repo, java = "repo", "java"
	ctx := t.Context()
	native := newFTSStore(t)
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
	other := &graph.Node{ID: "repo/Second.java::Second", Name: "Second", Kind: graph.KindType, RepoPrefix: repo, Language: java, FilePath: "repo/Second.java"}
	factory := method(first.FilePath, first.Name, "make", "Service upstream", repo, java)
	nodes := []*graph.Node{
		first, last, owner, other, factory,
		method(other.FilePath, other.Name, "ctor", "Service service", repo, java),
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
	require.NoError(t, native.AddBatchChecked(nodes, nil))
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
	require.NoError(t, native.AddBatchChecked(nil, edges))
	b.AddBatch(nil, edges)
	idx.extractDIContracts(contracts.NewRegistry())
	var want []*graph.Edge
	for _, row := range graph.ReadRepoEdgesByKinds(native, []string{repo}, []graph.EdgeKind{graph.EdgeCalls}) {
		want = append(want, row.Edge)
	}
	require.Len(t, want, 2)
	require.Equal(t, owner.ID, want[0].From)
	require.Equal(t, other.ID, want[1].From)
	for _, e := range want {
		require.Equal(t, first.FilePath, e.FilePath)
		require.Equal(t, 5, e.Line)
	}
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

func TestRebuildSpringBeanCallInputBounds(t *testing.T) {
	const repo = "repo"
	padding := strings.Repeat("x", 16<<20)
	for _, mode := range []string{"irrelevant_method", "non_bean_Provides", "method", "bean_Provides"} {
		t.Run(mode, func(t *testing.T) {
			b, err := store_pebble.OpenBuild(t.TempDir(), store_pebble.FullProfile)
			require.NoError(t, err)
			t.Cleanup(func() { require.NoError(t, b.Close()) })
			const path = "repo/Source.java"
			source := &graph.Node{ID: path + "::Source", FilePath: path, Kind: graph.KindType, Language: "java", RepoPrefix: repo}
			method := &graph.Node{ID: path + "::Client.make", FilePath: path, Kind: graph.KindMethod, Language: "java", RepoPrefix: repo,
				Meta: map[string]any{"receiver": "Client", "params_src": "Service input"}}
			bean := &graph.Edge{From: source.ID, To: path + "::Source.bean", Kind: graph.EdgeProvides, FilePath: path, Line: 1,
				Meta: map[string]any{graph.MetaDIBinding: graph.DIBindingBean, graph.MetaDIProvidesFor: "Service"}}
			switch mode {
			case "irrelevant_method":
				other := *method
				other.ID += "Kotlin"
				other.Language = "kotlin"
				other.Meta = map[string]any{"padding": padding}
				b.AddNode(&other)
			case "non_bean_Provides":
				other := *bean
				other.Line++
				other.Meta = map[string]any{graph.MetaDIBinding: graph.DIBindingUseClass, graph.MetaDIProvidesFor: "Service", "padding": padding}
				b.AddEdge(&other)
			case "method":
				method.Meta["padding"] = padding
			case "bean_Provides":
				bean.Meta["padding"] = padding
			}
			b.AddBatch([]*graph.Node{source, method}, []*graph.Edge{bean})
			var got []*graph.Edge
			err = visitRebuildSpringBeanCalls(t.Context(), b, repo, func(e *graph.Edge) error {
				got = append(got, e)
				return nil
			})
			if mode == "method" || mode == "bean_Provides" {
				require.ErrorIs(t, err, store_pebble.ErrRebuildLinkInputSize)
				require.Empty(t, got)
				return
			}
			require.NoError(t, err)
			require.Len(t, got, 1)
			require.Equal(t, path+"::Client", got[0].From)
			require.Equal(t, "Service", got[0].Meta["bean_of"])
		})
	}
}
