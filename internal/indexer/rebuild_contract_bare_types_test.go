package indexer

import (
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/zzet/gortex/internal/contracts"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/graph/store_pebble"
)

func TestRebuildContractBareTypeRefsMatchNativeBuckets(t *testing.T) {
	ctx := t.Context()
	b, err := store_pebble.OpenBuild(t.TempDir(), store_pebble.FullProfile)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, b.Close()) })
	native := graph.New()
	for _, item := range []struct {
		id, name string
		kind     graph.NodeKind
	}{
		{"r/a.go::Payload", "Payload", graph.KindType},
		{"s/b.go::Payload", "Payload", graph.KindType},
		{"r/a.go::Ambig", "Ambig", graph.KindType},
		{"r/b.go::Ambig", "Ambig", graph.KindType},
		{"s/b.go::Ambig", "Ambig", graph.KindType},
		{"r/a.go::Interface", "Interface", graph.KindInterface},
		{"r/a.go::Hidden", "Hidden", graph.KindType},
		{"Link", "Chain", graph.KindType},
		{"r/a.go::Final", "Link", graph.KindType},
	} {
		n := &graph.Node{ID: item.id, Name: item.name, Kind: item.kind, Language: "go"}
		native.AddNode(n)
		b.AddNode(n)
	}
	idx := &Indexer{graph: native}
	const id = "http::route"
	reg := contracts.NewRegistry()
	require.NoError(t, b.WithRebuildContractRecords(ctx, func(records *store_pebble.RebuildContractRecords) error {
		for i, item := range []struct{ symbol, request, response string }{
			{"S", "Payload", "Ambig"},
			{"S", "Hidden", "Payload"},
			{"T", "Interface", "r/a.go::Bound"},
			{"U", "Missing", "Chain"},
			{"V", "Link", "Payload"},
		} {
			c := contracts.Contract{ID: id, Type: contracts.ContractHTTP, Role: contracts.RoleProvider,
				RepoPrefix: "r", FilePath: "r/router.go", SymbolID: item.symbol, Line: i + 1,
				Meta: map[string]any{"request_type": item.request, "response_type": item.response}}
			reg.Add(cloneContractRecords([]contracts.Contract{c})[0])
			require.NoError(t, records.Add(store_pebble.RebuildContractRecord{Contract: c}))
		}
		reg.ReplaceByID("empty", nil)
		require.NoError(t, records.ClearID("empty"))
		idx.upgradeContractBareTypeRefs(reg)
		require.NoError(t, records.UpgradeBareTypeRefs(nil))
		var actual []contracts.Contract
		require.NoError(t, records.VisitByID(id, func(item store_pebble.RebuildContractRecord) error {
			actual = append(actual, item.Contract)
			return nil
		}))
		require.Equal(t, reg.ByID(id), actual)
		require.Equal(t, "r/a.go::Payload", actual[0].Meta["request_type"])
		require.Equal(t, "Ambig", actual[0].Meta["response_type"])
		require.Equal(t, "Hidden", actual[1].Meta["request_type"])
		require.Equal(t, "r/a.go::Payload", actual[1].Meta["response_type"])
		require.Equal(t, "Interface", actual[2].Meta["request_type"])
		require.Equal(t, "r/a.go::Bound", actual[2].Meta["response_type"])
		require.Equal(t, "Missing", actual[3].Meta["request_type"])
		require.Equal(t, "r/a.go::Final", actual[3].Meta["response_type"])
		var ids []string
		require.NoError(t, records.VisitIDs(func(id string) error {
			ids = append(ids, id)
			return nil
		}))
		require.ElementsMatch(t, reg.AllIDs(), ids)
		return nil
	}))
}

func TestRebuildContractBareTypeUnprovedRepresentatives(t *testing.T) {
	ctx := t.Context()
	b, err := store_pebble.OpenBuild(t.TempDir(), store_pebble.FullProfile)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, b.Close()) })
	for _, name := range []string{"Stale", "Proved"} {
		b.AddNode(&graph.Node{ID: "r/type.go::" + name, Name: name, Kind: graph.KindType})
	}
	require.NoError(t, b.WithRebuildContractRecords(ctx, func(records *store_pebble.RebuildContractRecords) error {
		for i, item := range []struct {
			symbol, name string
			unproved     bool
		}{
			{"S", "Stale", true},
			{"S", "Stale", false},
			{"S", "Proved", false},
			{"T", "Proved", false},
		} {
			c := contracts.Contract{ID: "http::GET::/route", Role: contracts.RoleProvider, Type: contracts.ContractHTTP,
				RepoPrefix: "r", FilePath: "r/route.go", SymbolID: item.symbol, Line: i + 1,
				Meta: map[string]any{"request_type": item.name}}
			require.NoError(t, records.Add(store_pebble.RebuildContractRecord{Contract: c, BodyUnproved: item.unproved}))
		}
		require.NoError(t, records.UpgradeBareTypeRefs(nil))
		var got []store_pebble.RebuildContractRecord
		require.NoError(t, records.VisitByID("http::GET::/route", func(item store_pebble.RebuildContractRecord) error {
			got = append(got, item)
			return nil
		}))
		require.Len(t, got, 4)
		for i := range 2 {
			require.True(t, got[i].BodyUnproved)
			require.Equal(t, "Stale", got[i].Meta["request_type"])
		}
		for i := 2; i < 4; i++ {
			require.False(t, got[i].BodyUnproved)
			require.Equal(t, "r/type.go::Proved", got[i].Meta["request_type"])
		}
		return nil
	}))
}

func TestRebuildContractBareTypeTurnCount(t *testing.T) {
	ctx := t.Context()
	b, err := store_pebble.OpenBuild(t.TempDir(), store_pebble.FullProfile)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, b.Close()) })
	native := graph.New()
	for _, pair := range [][2]string{{"A", "B"}, {"B", "C"}, {"C", "D"}, {"D", "file.go::Final"}} {
		n := &graph.Node{Name: pair[0], ID: pair[1], Kind: graph.KindType}
		b.AddNode(n)
		native.AddNode(n)
	}
	idx := &Indexer{graph: native}
	reg := contracts.NewRegistry()
	require.NoError(t, b.WithRebuildContractRecords(ctx, func(records *store_pebble.RebuildContractRecords) error {
		for i, row := range []struct{ id, symbol, name string }{
			{"route", "handler", "A"}, {"route", "", "A"}, {"openB", "", "B"}, {"openC", "", "C"}, {"openD", "", "D"},
		} {
			c := contracts.Contract{ID: row.id, SymbolID: row.symbol, FilePath: "route.go", Line: i + 1, Meta: map[string]any{"request_type": row.name}}
			reg.Add(cloneContractRecords([]contracts.Contract{c})[0])
			require.NoError(t, records.Add(store_pebble.RebuildContractRecord{Contract: c}))
		}
		idx.upgradeContractBareTypeRefs(reg)
		require.NoError(t, records.UpgradeBareTypeRefs(nil))
		var got []contracts.Contract
		require.NoError(t, records.VisitByID("route", func(item store_pebble.RebuildContractRecord) error {
			got = append(got, item.Contract)
			return nil
		}))
		require.Equal(t, reg.ByID("route"), got)
		require.Equal(t, "file.go::Final", got[0].Meta["request_type"])
		require.Equal(t, "D", got[1].Meta["request_type"])
		return nil
	}))
}

func TestRebuildContractBareTypeOnlyUnprovedRepresentative(t *testing.T) {
	b, err := store_pebble.OpenBuild(t.TempDir(), store_pebble.FullProfile)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, b.Close()) })
	for _, n := range []*graph.Node{
		{ID: "r/type.go::Unique", Name: "Unique", Kind: graph.KindType},
		{ID: "r/a.go::Ambiguous", Name: "Ambiguous", Kind: graph.KindType},
		{ID: "r/b.go::Ambiguous", Name: "Ambiguous", Kind: graph.KindType},
		{ID: "Same", Name: "Same", Kind: graph.KindType},
	} {
		b.AddNode(n)
	}
	require.NoError(t, b.WithRebuildContractRecords(t.Context(), func(records *store_pebble.RebuildContractRecords) error {
		const id = "http::GET::/route"
		names := []string{"Stale", "Missing", "Ambiguous", "Same", "Unique"}
		for i, name := range names {
			c := contracts.Contract{ID: id, SymbolID: "S", FilePath: "r/route.go", RepoPrefix: "r", Line: i + 1,
				Meta: map[string]any{"request_type": name}}
			require.NoError(t, records.Add(store_pebble.RebuildContractRecord{Contract: c, BodyUnproved: i == 0}))
		}
		require.NoError(t, records.UpgradeBareTypeRefs(nil))
		var got []store_pebble.RebuildContractRecord
		require.NoError(t, records.VisitByID(id, func(item store_pebble.RebuildContractRecord) error {
			got = append(got, item)
			return nil
		}))
		require.Len(t, got, len(names))
		for i, name := range names {
			require.Equal(t, name, got[i].Meta["request_type"])
			require.Equal(t, i == 0 || name == "Unique", got[i].BodyUnproved, name)
		}
		return nil
	}))
}
