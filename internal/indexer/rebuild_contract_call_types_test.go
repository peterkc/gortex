package indexer

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/graph/store_pebble"
)

func TestRebuildCallReturnNamesMatchNative(t *testing.T) {
	ctx := t.Context()
	g := graph.New()
	idx := newTestIndexer(g)
	t.Cleanup(idx.Close)
	b, err := store_pebble.OpenBuild(t.TempDir(), store_pebble.FullProfile)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, b.Close()) })
	add := func(id, name, repo string, kind graph.NodeKind, signature string) {
		n := &graph.Node{ID: id, Name: name, Kind: kind, RepoPrefix: repo, Language: "go",
			Meta: map[string]any{"signature": signature}}
		g.AddNode(n)
		b.AddNode(n)
	}
	add("r/z.go::ItemStore.Fetch", "Fetch", "r", graph.KindMethod, "func Fetch() ([]*api.Item, error)")
	add("r/a.go::ItemCache.Fetch", "Fetch", "r", graph.KindMethod, "func Fetch() ([]*api.Item, error)")
	add("r/b.go::ItemDB.Fetch", "Fetch", "r", graph.KindMethod, "")
	add("r/c.go::UserStore.Fetch", "Fetch", "r", graph.KindMethod, "func Fetch() User")
	add("other/d.go::ItemStore.Fetch", "Fetch", "other", graph.KindMethod, "func Fetch() Foreign")
	add("r/e.go::Fetch", "Fetch", "r", graph.KindType, "func Fetch() NotACallable")
	add("r/a.go::ItemStore.Load", "Load", "r", graph.KindMethod, "func Load() api.Item")
	add("r/z.go::ItemStore.Load", "Load", "r", graph.KindMethod, "func Load() other.Item")
	add("r/a.go::Do", "Do", "r", graph.KindFunction, "func Do() error")
	check := func(expr string, want rawContractCallType) {
		t.Helper()
		key := contractCallKey{callExpr: expr, repoPrefix: "r"}
		require.Equal(t, want, idx.resolveCallExprTypeNames([]contractCallKey{key})[key])
		got, err := rebuildCallReturnName(ctx, b.RebuildView(), key)
		require.NoError(t, err)
		require.Equal(t, want, got)
	}
	check("s.items.Fetch", rawContractCallType{typeName: "Item", repeated: true, pointer: true})
	check("s.items.Load", rawContractCallType{}) // same bare name, different raw return names
	check("Do", rawContractCallType{})
	check("s.items.", rawContractCallType{})
	check("absent", rawContractCallType{})
	stopped, cancel := context.WithCancel(ctx)
	cancel()
	_, err = rebuildCallReturnName(stopped, b.RebuildView(), contractCallKey{callExpr: "s.items.Fetch", repoPrefix: "r"})
	require.ErrorIs(t, err, context.Canceled)
}
