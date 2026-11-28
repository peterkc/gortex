package indexer

import (
	"iter"
	"slices"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/zzet/gortex/internal/contracts"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/graph/store_pebble"
)

func TestRebuildContractGraphRowsNativeOwners(t *testing.T) {
	const a, z, envID, routeID = "a.go", "z.go", "env::TOKEN", "http::GET::/x"
	ctx := t.Context()
	b, err := store_pebble.OpenBuild(t.TempDir(), store_pebble.FullProfile)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, b.Close()) })
	head, err := store_pebble.OpenHead(t.TempDir(), rebuildTestIncarnation, store_pebble.FullProfile)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, head.Close()) })
	manifest, err := store_pebble.PrepareManifest(ctx, t.TempDir(), []store_pebble.ManifestFile{
		{Path: a, Kind: 'f', Language: "go"}, {Path: z, Kind: 'f', Language: "go"},
	}, store_pebble.Witness{
		Input: store_pebble.HeadDigest{1}, Recipe: store_pebble.HeadDigest{2}, Stage: store_pebble.StageManifest,
		Profile: store_pebble.FullProfile, PlatformCatalogRevision: graph.PlatformCatalogRevision,
	}, rebuildTestIncarnation)
	require.NoError(t, err)
	_, err = head.PublishStage(ctx, manifest)
	require.NoError(t, err)

	native := graph.New()
	owner := &graph.Node{ID: z + "::Handler", Name: "Handler", Kind: graph.KindFunction, Language: "go", FilePath: z}
	for _, path := range []string{a, z} {
		nodes := []*graph.Node{{ID: path, Name: path, FilePath: path, Kind: graph.KindFile, Language: "go"}}
		if path == z {
			nodes = append(nodes, owner)
		}
		native.AddBatch(nodes, nil)
		require.NoError(t, b.AddParsedFile("", store_pebble.ParseFacts{
			File: graph.FileMetaRow{FilePath: path, NodeCount: len(nodes)}, EffectiveLanguage: "go", Nodes: nodes,
		}))
	}
	registry := contracts.NewRegistry()
	registry.Add(contracts.Contract{ID: envID, Type: contracts.ContractEnv, Role: contracts.RoleConsumer,
		FilePath: a, SymbolID: a + "::Missing", Line: 2, Meta: map[string]any{"var": "TOKEN"}})
	registry.Add(contracts.Contract{ID: envID, Type: contracts.ContractEnv, Role: contracts.RoleProvider,
		FilePath: z, Line: 1, Meta: map[string]any{"var": "TOKEN"}})
	registry.Add(contracts.Contract{ID: routeID, Type: contracts.ContractHTTP, Role: contracts.RoleProvider,
		FilePath: z, SymbolID: owner.ID, Line: 5, Meta: map[string]any{"method": "GET", "path": "/x"}})
	records := registry.All()
	wantNodes, wantEdges, missing := contractGraphRows(native, records, true)
	require.Equal(t, 1, missing)
	require.Len(t, wantNodes, 2)
	require.Len(t, wantEdges, 3)
	require.Equal(t, a, wantNodes[0].FilePath)
	require.Equal(t, false, wantNodes[0].Meta["contract_owner_record"])
	slices.SortStableFunc(records, func(a, b contracts.Contract) int {
		if contractNodeRecordLess(a, b) {
			return -1
		}
		if contractNodeRecordLess(b, a) {
			return 1
		}
		return 0
	})
	stream := func(records []contracts.Contract) iter.Seq2[contracts.Contract, error] {
		return func(yield func(contracts.Contract, error) bool) {
			for _, c := range records {
				if !yield(c, nil) {
					return
				}
			}
		}
	}
	outOfOrder := slices.Clone(records)
	slices.Reverse(outOfOrder)
	err = visitRebuildContractGraphRows(ctx, b.RebuildView(), stream(outOfOrder), func(contracts.Contract, *graph.Node, []*graph.Edge) error {
		return nil
	})
	require.ErrorIs(t, err, graph.ErrContractProjectionStale)

	files, err := b.PrepareFileGraph(ctx, t.TempDir(), manifest.Witness, rebuildTestIncarnation)
	require.NoError(t, err)
	fileBase, err := b.RetainFileGraph(ctx, files)
	require.NoError(t, err)
	_, err = head.PublishStage(ctx, files)
	require.NoError(t, err)
	relations, err := b.PrepareRelations(ctx, t.TempDir(), fileBase, nil)
	require.NoError(t, err)
	retained, err := b.RetainRelations(ctx, fileBase, relations)
	require.NoError(t, err)
	_, err = head.PublishStage(ctx, relations)
	require.NoError(t, err)
	var batches []store_pebble.RebuildContractBatch
	var seenNodes []*graph.Node
	var seenEdges []*graph.Edge
	observed := make(rebuildEarlyContractIDs)
	require.NoError(t, visitRebuildContractGraphRows(ctx, b.RebuildView(), stream(records),
		func(_ contracts.Contract, node *graph.Node, edges []*graph.Edge) error {
			batch := store_pebble.RebuildContractBatch{Edges: edges}
			if node != nil {
				observed[node.ID] = node.FilePath
				seenNodes = append(seenNodes, node)
				batch.Nodes = []*graph.Node{node}
			}
			seenEdges = append(seenEdges, edges...)
			batches = append(batches, batch)
			return nil
		}))
	require.Equal(t, wantNodes, seenNodes)
	require.Equal(t, wantEdges, seenEdges)
	tables, err := prepareRebuildContracts(ctx, b, t.TempDir(), retained, rebuildEarlyContractIDs{envID: a, routeID: z},
		orderedEarlyContractIDs(observed), orderedEarlyContractIDs(rebuildEarlyContractIDs{envID: a, routeID: z}),
		func(yield func(store_pebble.RebuildContractBatch, error) bool) {
			for _, batch := range batches {
				if !yield(batch, nil) {
					return
				}
			}
		}, store_pebble.ContractCoverage{}, nil, nil)
	require.NoError(t, err)
	_, err = head.PublishStage(ctx, tables)
	require.NoError(t, err)
	snapshot := head.OpenSnapshot()
	defer snapshot.Close()
	for _, node := range wantNodes {
		require.Equal(t, node, snapshot.GetNode(node.ID))
	}
	var actual []*graph.Edge
	for _, kind := range []graph.EdgeKind{graph.EdgeProvides, graph.EdgeConsumes, graph.EdgeHandlesRoute} {
		for e := range snapshot.EdgesByKind(kind) {
			actual = append(actual, e)
		}
	}
	require.ElementsMatch(t, wantEdges, actual)
}

func TestRebuildContractGraphRowsNativeRepoOwners(t *testing.T) {
	const path, first, last, foreign, symbol = "shared.go", "b-file", "z-file", "a-file", "shared.go::Handle"
	ctx := t.Context()
	file := func(id, repo string) *graph.Node {
		return &graph.Node{ID: id, FilePath: path, RepoPrefix: repo, Kind: graph.KindFile, Language: "go"}
	}
	nodes := []*graph.Node{
		file(last, "one"), file(foreign, "two"), file(first, "one"),
		{ID: symbol, FilePath: path, RepoPrefix: "two", Kind: graph.KindFunction, Language: "go"},
	}
	native := newFTSStore(t)
	require.NoError(t, native.AddBatchChecked(nodes, nil))
	b, err := store_pebble.OpenBuild(t.TempDir(), store_pebble.FullProfile)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, b.Close()) })
	b.AddBatch(nodes, nil)
	record := func(id, repo, source string) contracts.Contract {
		return contracts.Contract{ID: "env::" + id, Type: contracts.ContractEnv, Role: contracts.RoleConsumer,
			FilePath: path, RepoPrefix: repo, SymbolID: source}
	}
	records := []contracts.Contract{
		record("A", "one", symbol), // the foreign symbol must not fall back to the local file
		record("B", "one", ""),
		record("C", "two", ""),
		record("D", "two", symbol),
		record("E", "absent", ""),
	}
	wantNodes, wantEdges, missing := contractGraphRows(native, records, true)
	require.Equal(t, 2, missing)
	require.Len(t, wantEdges, 3)
	require.Equal(t, first, wantEdges[0].From)
	require.Equal(t, foreign, wantEdges[1].From)
	require.Equal(t, symbol, wantEdges[2].From)
	stream := func(yield func(contracts.Contract, error) bool) {
		for _, c := range records {
			if !yield(c, nil) {
				return
			}
		}
	}
	var gotNodes []*graph.Node
	var gotEdges []*graph.Edge
	visit := func(_ contracts.Contract, n *graph.Node, edges []*graph.Edge) error {
		gotNodes = append(gotNodes, n)
		gotEdges = append(gotEdges, edges...)
		return nil
	}
	view := b.RebuildView()
	require.ErrorIs(t, visitRebuildContractGraphRows(ctx, view, nil, visit), graph.ErrContractProjectionStale)
	require.Empty(t, gotNodes)
	require.Empty(t, gotEdges)
	require.NoError(t, visitRebuildContractGraphRows(ctx, view, stream, visit))
	require.Equal(t, wantNodes, gotNodes)
	require.Equal(t, wantEdges, gotEdges)
}
