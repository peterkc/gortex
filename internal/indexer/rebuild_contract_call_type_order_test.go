package indexer

import (
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/graph/store_pebble"
	"go.uber.org/zap"
	"go.uber.org/zap/zaptest/observer"
)

func TestRebuildCallReturnTypeOrderUsesDecidingTier(t *testing.T) {
	b, err := store_pebble.OpenBuild(t.TempDir(), store_pebble.FullProfile)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, b.Close()) })
	add := func(id, name, repo, lang string, kind graph.NodeKind) {
		b.AddNode(&graph.Node{ID: id, Name: name, RepoPrefix: repo, Language: lang, Kind: kind})
	}
	add("r/z.go::Item", "Item", "r", "go", graph.KindType)
	add("r/a.ts::Item", "Item", "r", "typescript", graph.KindType)
	add("r/0.go::Item", "Item", "r", "go", graph.KindInterface)
	add("a/a.go::Item", "Item", "a", "go", graph.KindType)
	add("r/z.go::Unique", "Unique", "r", "go", graph.KindType)
	add("a/a.go::Unique", "Unique", "a", "go", graph.KindType)
	add("a/b.go::Unique", "Unique", "a", "go", graph.KindType)
	add("r/0.go::Interface", "Interface", "r", "go", graph.KindInterface)
	core, observed := observer.New(zap.DebugLevel)
	require.NoError(t, b.WithRebuildContractRecords(t.Context(), func(records *store_pebble.RebuildContractRecords) error {
		check := func(repo, name, want string) {
			t.Helper()
			got, err := records.ResolveCallReturnTypeName(repo, name)
			require.NoError(t, err)
			require.Equal(t, want, got)
		}
		check("r", "Item", "r/a.ts::Item")
		check("r", "Item", "r/a.ts::Item") // One native lookup case, not two.
		check("r", "Unique", "r/z.go::Unique")
		check("r", "Interface", "Interface")
		check("r", "absent", "absent")
		check("r", "other.go::Item", "other.go::Item")
		check("absent", "Item", "a/a.go::Item")
		return logRebuildCallReturnTypeOrder(records, zap.New(core))
	}))
	counts := observed.FilterMessage("rebuild call return type native order differences").All()
	require.Len(t, counts, 1)
	require.EqualValues(t, 2, counts[0].ContextMap()["cases"])
	details := observed.FilterMessage("rebuild call return type native order difference").All()
	require.Len(t, details, 2)
	require.Equal(t, "absent", details[0].ContextMap()["repo"])
	require.EqualValues(t, 3, details[0].ContextMap()["candidates"])
	require.Equal(t, "r", details[1].ContextMap()["repo"])
	require.Equal(t, "r/a.ts::Item", details[1].ContextMap()["selected_id"])
	require.EqualValues(t, 2, details[1].ContextMap()["candidates"])
}
