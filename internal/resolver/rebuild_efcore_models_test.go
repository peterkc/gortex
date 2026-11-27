package resolver

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/graph/store_pebble"
)

func efRebuildSelection(t *testing.T, build *store_pebble.BuildStore) FrameworkSynthesizerSelection {
	t.Helper()
	sel, err := NewFrameworkSynthesizerSelection([]string{SynthCSharpEFCoreModels})
	require.NoError(t, err)
	frozen, err := FreezeRebuildFrameworkSelection(t.Context(), build.RebuildView(), sel)
	require.NoError(t, err)
	return frozen
}

func TestRebuildEFCoreModelsSQLiteIncumbent(t *testing.T) {
	const cs, goFile = "Widget.cs", "schema.go"
	entity := &graph.Node{ID: cs + "::Widget", Name: "Widget", FilePath: cs, Kind: graph.KindType, Language: "csharp", StartLine: 2,
		Meta: map[string]any{"ef_attribute_table": "old_widgets"}}
	file := &graph.Node{ID: cs, Name: cs, FilePath: cs, Kind: graph.KindFile, Language: "csharp",
		Meta: map[string]any{"ef_fluent": []map[string]any{efMappingAction("Ctx", "Widget", "widgets", "", "table", 0, 10)}}}
	old := &graph.Edge{From: entity.ID, To: "db::orm::old_widgets", Kind: graph.EdgeModelsTable, FilePath: cs, Line: 2,
		Meta: map[string]any{"orm": "efcore", "binding": "attribute", "table_name": "old_widgets"}}
	oldTable := csharpEFTableNodeForEdge(entity, old)
	incumbent := &graph.Node{ID: "db::orm::widgets", Name: "widgets", FilePath: goFile, Kind: graph.KindTable, Language: "go",
		Meta: map[string]any{"dialect": "orm", "source": "go-orm"}}
	other := &graph.Edge{From: entity.ID, To: incumbent.ID, Kind: graph.EdgeModelsTable, FilePath: cs, Line: 9,
		Meta: map[string]any{"orm": "foreign"}}
	facts := []store_pebble.ParseFacts{
		rebuildEventFacts(cs, "csharp", []*graph.Node{entity, file, oldTable}, old, other),
		rebuildEventFacts(goFile, "go", []*graph.Node{incumbent}),
	}
	build, native := openRebuildEventStores(t, facts...)
	selection := efRebuildSelection(t, build)
	require.True(t, selection.allows(SynthCSharpEFCoreModels))
	receipts := rebuildGRPCReceipts(facts...)
	for _, expected := range []uint64{1, 0} {
		require.Equal(t, int(expected), efResolveModels(t, native))
		changed, err := RunRebuildEFCoreModels(t.Context(), build, receipts, selection)
		require.NoError(t, err)
		require.Equal(t, expected, changed)
		require.Equal(t, rebuildEventSQLiteRows(native), rebuildEventSavedRows(t, build))
	}
	require.Equal(t, incumbent, native.GetNode(incumbent.ID))
	require.Contains(t, native.AllEdges(), other)
	require.NotContains(t, native.AllEdges(), old)
}

func TestRebuildEFCoreModelsSQLiteConflictAndProof(t *testing.T) {
	const cs = "Widget.cs"
	entity := &graph.Node{ID: cs + "::Widget", Name: "Widget", FilePath: cs, Kind: graph.KindType, Language: "csharp", StartLine: 2,
		Meta: map[string]any{"ef_attribute_table": "old_widgets"}}
	file := &graph.Node{ID: cs, Name: cs, FilePath: cs, Kind: graph.KindFile, Language: "csharp",
		Meta: map[string]any{"ef_fluent": []map[string]any{
			efMappingAction("First", "Widget", "first_widgets", "", "table", 0, 8),
			efMappingAction("Second", "Widget", "second_widgets", "", "table", 0, 12),
		}}}
	old := &graph.Edge{From: entity.ID, To: "db::orm::old_widgets", Kind: graph.EdgeModelsTable, FilePath: cs, Line: 2,
		Meta: map[string]any{"orm": "efcore", "binding": "attribute", "table_name": "old_widgets"}}
	oldTable := csharpEFTableNodeForEdge(entity, old)
	facts := rebuildEventFacts(cs, "csharp", []*graph.Node{entity, file, oldTable}, old)
	for _, omit := range []bool{true, false} {
		build, native := openRebuildEventStores(t, facts)
		before := rebuildEventSavedRows(t, build)
		receipt := facts
		if omit {
			receipt.Nodes = []*graph.Node{entity, oldTable}
		}
		changed, err := RunRebuildEFCoreModels(t.Context(), build, rebuildGRPCReceipts(receipt), efRebuildSelection(t, build))
		if omit {
			require.ErrorIs(t, err, store_pebble.ErrRebuildEFCoreModels)
			var refused *store_pebble.RebuildUnknownProvenanceError
			require.ErrorAs(t, err, &refused)
			require.Equal(t, graph.EdgeIdentityFor(old), refused.First)
			require.Zero(t, changed)
			require.Equal(t, before, rebuildEventSavedRows(t, build))
			continue
		}
		require.NoError(t, err)
		require.Equal(t, uint64(1), changed)
		require.Equal(t, 1, efResolveModels(t, native))
		require.Empty(t, native.AllEdges(), "a fluent conflict blocks the attribute fallback")
		require.Equal(t, rebuildEventSQLiteRows(native), rebuildEventSavedRows(t, build))
	}
}

func TestRebuildEFCoreModelsSQLiteSharedNewTable(t *testing.T) {
	a := &graph.Node{ID: "A.cs::Widget", Name: "Widget", FilePath: "A.cs", Kind: graph.KindType, Language: "csharp", WorkspaceID: "z",
		StartLine: 2, Meta: map[string]any{"ef_attribute_table": "widgets"}}
	z := &graph.Node{ID: "Z.cs::Widget", Name: "Widget", FilePath: "Z.cs", Kind: graph.KindType, Language: "csharp", WorkspaceID: "a",
		StartLine: 4, Meta: map[string]any{"ef_attribute_table": "widgets"}}
	facts := []store_pebble.ParseFacts{
		rebuildEventFacts(a.FilePath, "csharp", []*graph.Node{a}),
		rebuildEventFacts(z.FilePath, "csharp", []*graph.Node{z}),
	}
	build, native := openRebuildEventStores(t, facts...)
	require.Equal(t, 2, efResolveModels(t, native))
	require.Equal(t, z.FilePath, native.GetNode("db::orm::widgets").FilePath, "native chooses the last affected entity by ID")
	changed, err := RunRebuildEFCoreModels(t.Context(), build, rebuildGRPCReceipts(facts...), efRebuildSelection(t, build))
	require.NoError(t, err)
	require.Equal(t, uint64(2), changed)
	require.Equal(t, rebuildEventSQLiteRows(native), rebuildEventSavedRows(t, build))
}

func TestRebuildEFCoreModelsFrozenSelection(t *testing.T) {
	build := openRebuildGRPCBuild(t)
	frozen := efRebuildSelection(t, build)
	require.False(t, frozen.allows(SynthCSharpEFCoreModels))
	build.AddNode(&graph.Node{ID: "Widget.cs::Widget", Name: "Widget", FilePath: "Widget.cs", Kind: graph.KindType, Language: "csharp"})
	require.True(t, efRebuildSelection(t, build).allows(SynthCSharpEFCoreModels))
	changed, err := RunRebuildEFCoreModels(t.Context(), build, nil, frozen)
	require.NoError(t, err)
	require.Zero(t, changed)
}

func TestRebuildEFCoreModelsSQLiteAssembly(t *testing.T) {
	const cs = "Entities.cs"
	entity := func(name string) *graph.Node {
		return &graph.Node{ID: cs + "::" + name, Name: name, FilePath: cs, Kind: graph.KindType, Language: "csharp", StartLine: 2}
	}
	config := func(id, target, table string) *graph.Node {
		return &graph.Node{ID: cs + "::" + id, Name: "SharedConfig", FilePath: cs, Kind: graph.KindType, Language: "csharp", StartLine: 6,
			Meta: map[string]any{"ef_config_entity": target, "ef_config_table": table, "ef_config_relation": "table"}}
	}
	a, b := entity("Alpha"), entity("Beta")
	file := &graph.Node{ID: cs, Name: cs, FilePath: cs, Kind: graph.KindFile, Language: "csharp",
		Meta: map[string]any{"ef_fluent": []map[string]any{
			efApplyAssemblyAction("Ctx", 0, 10),
			efMappingAction("Ctx", a.Name, "alpha_override", "", "table", 1, 11),
			efApplyConfigAction("Ctx", "SharedConfig", 2, 12),
		}}}
	facts := rebuildEventFacts(cs, "csharp", []*graph.Node{a, b, file, config("cfg1", a.Name, "alpha"), config("cfg2", b.Name, "beta")})
	build, native := openRebuildEventStores(t, facts)
	require.Equal(t, 2, efResolveModels(t, native))
	require.Equal(t, "db::orm::alpha_override", efOnlyModelEdgeFrom(t, native, a.ID).To, "explicit activation is ambiguous across entities")
	require.Equal(t, "db::orm::beta", efOnlyModelEdgeFrom(t, native, b.ID).To)
	changed, err := RunRebuildEFCoreModels(t.Context(), build, rebuildGRPCReceipts(facts), efRebuildSelection(t, build))
	require.NoError(t, err)
	require.Equal(t, uint64(2), changed)
	require.Equal(t, rebuildEventSQLiteRows(native), rebuildEventSavedRows(t, build))
}

func TestRebuildEFCoreModelsReceiptFactAndCohort(t *testing.T) {
	const cs = "Models.cs"
	entity := &graph.Node{ID: "widget", Name: "Widget", FilePath: cs, Kind: graph.KindType, Language: "csharp"}
	file := &graph.Node{ID: cs, Name: cs, FilePath: cs, Kind: graph.KindFile, Language: "csharp"}
	fluent := *file
	fluent.Meta = map[string]any{"ef_fluent": []map[string]any{efMappingAction("Ctx", "Widget", "widgets", "", "table", 0, 10)}}
	other := &graph.Node{ID: "other", Name: "Widget", FilePath: cs, Kind: graph.KindType, Language: "csharp", WorkspaceID: "elsewhere"}
	entrant := *other
	entrant.WorkspaceID = ""
	for _, fixture := range []struct {
		name string
		live []*graph.Node
		raw  []*graph.Node
	}{
		{"receipt-only fact", []*graph.Node{entity, file}, []*graph.Node{entity, &fluent}},
		{"receipt-only cohort entrant", []*graph.Node{entity, &fluent, other}, []*graph.Node{entity, &fluent, &entrant}},
		{"unproved stationary cohort member", []*graph.Node{entity, &fluent, &entrant}, []*graph.Node{entity, &fluent}},
	} {
		t.Run(fixture.name, func(t *testing.T) {
			live := rebuildEventFacts(cs, "csharp", fixture.live)
			raw := rebuildEventFacts(cs, "csharp", fixture.raw)
			build := openRebuildGRPCBuild(t, live)
			before := rebuildEventSavedRows(t, build)
			changed, err := RunRebuildEFCoreModels(t.Context(), build, rebuildGRPCReceipts(raw), efRebuildSelection(t, build))
			var refused *store_pebble.RebuildUnknownProvenanceError
			require.ErrorAs(t, err, &refused)
			require.Equal(t, graph.EdgeIdentity{From: entity.ID, To: "db::orm::widgets", Kind: graph.EdgeModelsTable, FilePath: cs, Line: 10}, refused.First)
			require.Zero(t, changed)
			require.Equal(t, before, rebuildEventSavedRows(t, build))
		})
	}
}

func TestRebuildEFCoreModelsOwnedReceiptPayload(t *testing.T) {
	const cs = "Models.cs"
	entity := &graph.Node{ID: "widget", Name: "Widget", FilePath: cs, Kind: graph.KindType, Language: "csharp", StartLine: 2,
		Meta: map[string]any{"ef_attribute_table": "old_widgets"}}
	file := &graph.Node{ID: cs, Name: cs, FilePath: cs, Kind: graph.KindFile, Language: "csharp",
		Meta: map[string]any{"ef_fluent": []map[string]any{efMappingAction("Ctx", "Widget", "widgets", "", "table", 0, 10)}}}
	old := &graph.Edge{From: entity.ID, To: "db::orm::old_widgets", Kind: graph.EdgeModelsTable, FilePath: cs, Line: 2,
		Origin: graph.OriginASTInferred, Confidence: ConfidenceTyped,
		Meta: map[string]any{"orm": "efcore", "binding": "attribute", "table_name": "old_widgets"}}
	live := rebuildEventFacts(cs, "csharp", []*graph.Node{entity, file}, old)
	for _, drift := range []string{"binding", "origin", "confidence"} {
		t.Run(drift, func(t *testing.T) {
			build := openRebuildGRPCBuild(t, live)
			before := rebuildEventSavedRows(t, build)
			wrong := *old
			switch drift {
			case "binding":
				wrong.Meta = map[string]any{"orm": "efcore", "binding": "dbset", "table_name": "old_widgets"}
			case "origin":
				wrong.Origin = graph.OriginTextMatched
			case "confidence":
				wrong.Confidence = 0
			}
			receipt := rebuildEventFacts(cs, "csharp", live.Nodes, &wrong)
			changed, err := RunRebuildEFCoreModels(t.Context(), build, rebuildGRPCReceipts(receipt), efRebuildSelection(t, build))
			var refused *store_pebble.RebuildUnknownProvenanceError
			require.ErrorAs(t, err, &refused)
			require.Equal(t, graph.EdgeIdentityFor(old), refused.First)
			require.Zero(t, changed)
			require.Equal(t, before, rebuildEventSavedRows(t, build))
		})
	}
}

func TestRebuildEFCoreModelsBudgetOnlyNamedTypes(t *testing.T) {
	const cs = "Models.cs"
	entity := &graph.Node{ID: "widget", Name: "Widget", FilePath: cs, Kind: graph.KindType, Language: "csharp"}
	file := &graph.Node{ID: cs, Name: cs, FilePath: cs, Kind: graph.KindFile, Language: "csharp",
		Meta: map[string]any{"ef_fluent": []map[string]any{efMappingAction("Ctx", "Widget", "widgets", "", "table", 0, 10)}}}
	large := &graph.Node{ID: "unused", Name: "Unused", FilePath: cs, Kind: graph.KindType, Language: "csharp",
		Meta: map[string]any{"unrelated": strings.Repeat("x", 16<<20)}}
	facts := rebuildEventFacts(cs, "csharp", []*graph.Node{entity, file, large})
	build := openRebuildGRPCBuild(t, facts)
	changed, err := RunRebuildEFCoreModels(t.Context(), build, rebuildGRPCReceipts(facts), efRebuildSelection(t, build))
	require.NoError(t, err)
	require.EqualValues(t, 1, changed)
	for _, e := range rebuildEventSavedRows(t, build).edges {
		if e.Kind == graph.EdgeModelsTable {
			require.Equal(t, entity.ID, e.From)
			require.Equal(t, "db::orm::widgets", e.To)
			return
		}
	}
	t.Fatal("missing EF model projection")
}
