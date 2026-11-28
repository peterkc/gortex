package indexer

import (
	"fmt"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/zzet/gortex/internal/contracts"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/graph/store_pebble"
	"github.com/zzet/gortex/internal/parser/languages"
)

func TestRebuildDIContractRecordsUseNativeOrder(t *testing.T) {
	ctx := t.Context()
	native := newFTSStore(t)
	idx := newTestIndexer(native)
	idx.repoPrefix, idx.workspaceID, idx.projectID = "r", "w", "p"
	t.Cleanup(idx.Close)
	b, err := store_pebble.OpenBuild(t.TempDir(), store_pebble.FullProfile)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, b.Close()) })
	nodes := []*graph.Node{
		{ID: "S", Kind: graph.KindType, RepoPrefix: idx.repoPrefix},
		{ID: "foreign", Kind: graph.KindType, RepoPrefix: "other"},
	}
	for _, id := range []string{"A", "B", "C", "D"} {
		nodes = append(nodes, &graph.Node{ID: id, Kind: graph.KindType, RepoPrefix: idx.repoPrefix})
	}
	const file, token = "app.ts", "Service"
	provider := func(from, to, binding string, line int) *graph.Edge {
		return &graph.Edge{From: from, To: to, Kind: graph.EdgeProvides, FilePath: file, Line: line,
			Meta: map[string]any{graph.MetaDIBinding: binding, graph.MetaDIToken: token}}
	}
	edges := []*graph.Edge{
		provider("S", "D", graph.DIBindingUseFactory, 10),
		{From: "S", To: "B", Kind: graph.EdgeConsumes, FilePath: file, Line: 12, Meta: map[string]any{"via": "@Inject", graph.MetaDIToken: token}},
		provider("S", "A", graph.DIBindingUseValue, 8),
		provider("S", "C", "not-DI", 17),
		provider("foreign", "A", graph.DIBindingUseValue, 4),
	}
	native.AddBatch(nodes, edges)
	b.AddBatch(nodes, edges)
	const id = "di::" + token
	prior := contracts.Contract{ID: id, Type: contracts.ContractDI, Role: contracts.RoleProvider, RepoPrefix: "r", FilePath: file, SymbolID: "parse", Line: 2}
	reg := contracts.NewRegistry()
	reg.Add(prior)
	idx.extractDIContracts(reg)
	require.NoError(t, b.WithRebuildContractRecords(ctx, func(records *store_pebble.RebuildContractRecords) error {
		require.NoError(t, records.Add(store_pebble.RebuildContractRecord{Contract: prior}))
		require.NoError(t, idx.appendRebuildDIContracts(records))
		var actual []contracts.Contract
		require.NoError(t, records.VisitByID(id, func(item store_pebble.RebuildContractRecord) error {
			actual = append(actual, item.Contract)
			return nil
		}))
		require.Equal(t, reg.ByID(id), actual)
		require.Len(t, actual, 4)
		require.Equal(t, 8, actual[1].Line)
		require.Equal(t, 12, actual[2].Line)
		require.Equal(t, 10, actual[3].Line)
		require.Equal(t, "w", actual[3].WorkspaceID)
		require.Equal(t, "p", actual[3].ProjectID)
		return nil
	}))
}

func TestRebuildDIContractDuplicateProvidesKeepsNativeFirst(t *testing.T) {
	for _, tokens := range [][2]string{{"FIRST", "SECOND"}, {"SECOND", "FIRST"}} {
		t.Run(tokens[0], func(t *testing.T) {
			const path = "module.ts"
			ctx := t.Context()
			native := newFTSStore(t)
			idx := newTestIndexer(native)
			t.Cleanup(idx.Close)
			ext := languages.NewTypeScriptExtractor()
			idx.registry.Register(ext)
			src := fmt.Appendf(nil, `import { Module } from "@nestjs/common";
class Impl {}
@Module({providers: [{provide: "%s", useClass: Impl}, {provide: "%s", useClass: Impl}]})
export class AppModule {}
`, tokens[0], tokens[1])
			parsed, _, err := idx.extractPreparedRebuildFileWithContractFacts(ctx, path, ext, src, src)
			require.NoError(t, err)
			var providers []*graph.Edge
			for _, e := range parsed.Edges {
				if e.Kind == graph.EdgeProvides {
					providers = append(providers, e)
				}
			}
			require.Len(t, providers, 2)
			require.Equal(t, providers[0].From, providers[1].From)
			require.Equal(t, providers[0].To, providers[1].To)
			require.Equal(t, providers[0].FilePath, providers[1].FilePath)
			require.Equal(t, providers[0].Line, providers[1].Line)
			require.Equal(t, tokens[0], providers[0].Meta[graph.MetaDIProvidesFor])
			require.Equal(t, tokens[1], providers[1].Meta[graph.MetaDIProvidesFor])

			require.NoError(t, native.AddBatchChecked(parsed.Nodes, parsed.Edges))
			want := contracts.NewRegistry()
			idx.extractDIContracts(want)
			id := "di::" + tokens[0]
			require.Equal(t, []string{id}, want.AllIDs())

			b, err := store_pebble.OpenBuild(t.TempDir(), store_pebble.FullProfile)
			require.NoError(t, err)
			t.Cleanup(func() { require.NoError(t, b.Close()) })
			require.NoError(t, b.AddParsedFile("", parsed))
			var early []string
			require.NoError(t, b.RebuildView().VisitDIContractEdges(ctx, "", func(e *graph.Edge) error {
				if c, ok := diContractFromEdge(e); ok {
					early = append(early, c.ID)
				}
				return nil
			}))
			require.Equal(t, want.AllIDs(), early)
			require.NoError(t, b.WithRebuildContractRecords(ctx, func(records *store_pebble.RebuildContractRecords) error {
				require.NoError(t, idx.appendRebuildDIContracts(records))
				var late []contracts.Contract
				require.NoError(t, records.VisitByID(id, func(item store_pebble.RebuildContractRecord) error {
					late = append(late, item.Contract)
					return nil
				}))
				require.Equal(t, want.ByID(id), late)
				var ids []string
				require.NoError(t, records.VisitIDs(func(id string) error {
					ids = append(ids, id)
					return nil
				}))
				require.Equal(t, want.AllIDs(), ids)
				return nil
			}))
		})
	}
}
