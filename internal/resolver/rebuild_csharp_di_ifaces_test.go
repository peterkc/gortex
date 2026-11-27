package resolver

import (
	"slices"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/graph/store_pebble"
)

func diRebuildSelection(t *testing.T, b *store_pebble.BuildStore) FrameworkSynthesizerSelection {
	t.Helper()
	selection, err := NewFrameworkSynthesizerSelection([]string{SynthCSharpDIIfaces})
	require.NoError(t, err)
	frozen, err := FreezeRebuildFrameworkSelection(t.Context(), b.RebuildView(), selection)
	require.NoError(t, err)
	return frozen
}

func TestRebuildCSharpDIIfacesSQLiteFolded(t *testing.T) {
	const a, b, reg = "A.cs", "B.cs", "Module.cs"
	file := &graph.Node{ID: reg, Kind: graph.KindFile, FilePath: reg, Language: "csharp"}
	canonical := ifaceRebuildNode(a, "Svc", "csharp", graph.KindType)
	husk := ifaceRebuildNode(b, "Svc", "csharp", graph.KindType)
	first := ifaceRebuildNode(a, "IRunner", "csharp", graph.KindInterface)
	second := ifaceRebuildNode(b, "IRunner", "csharp", graph.KindInterface)
	first.StartLine, second.StartLine = 3, 5
	suppressed := ifaceRebuildNode(b, "IAlready", "csharp", graph.KindInterface)
	inferred := ifaceRebuildNode(b, "IInferred", "csharp", graph.KindInterface)
	marker := diIfacesMarker(reg, "Svc")
	otherMarker := *marker
	otherMarker.Line++
	existing := csharpDIProvideEdge(reg, "Svc", suppressed)
	existing.FilePath = reg
	late := ifaceRebuildEdge(husk, first.ID, graph.EdgeImplements, 20)
	early := ifaceRebuildEdge(husk, second.ID, graph.EdgeImplements, 10)
	methodSet := ifaceRebuildEdge(husk, inferred.ID, graph.EdgeImplements, 7)
	methodSet.Meta = map[string]any{"via": MetaViaMethodSetInference}
	facts := []store_pebble.ParseFacts{
		rebuildEventFacts(a, "csharp", []*graph.Node{canonical, first}),
		rebuildEventFacts(b, "csharp", []*graph.Node{husk, second, suppressed, inferred}, late, early, methodSet,
			ifaceRebuildEdge(husk, suppressed.ID, graph.EdgeImplements, 8)),
		rebuildEventFacts(reg, "csharp", []*graph.Node{file}, marker, &otherMarker, existing),
	}
	build, native := openRebuildEventStores(t, facts...)
	ctx := t.Context()
	require.NoError(t, runRebuildCSharpPartial(ctx, build, rebuildGRPCReceipts(facts...)))
	New(native).mergeCSharpPartialTypes()
	require.Equal(t, 1, ResolveCSharpDIImplementedInterfaces(native))
	want := rebuildEventSQLiteRows(native)
	require.Contains(t, want.edges, csharpDIProvideEdge(reg, "Svc", second), "SQLite line order takes precedence over receipt order")
	require.Contains(t, want.edges, existing)
	// The two markers produce the same expansion; one exact marker suffices.
	facts[2].Edges = []*graph.Edge{&otherMarker, existing}
	selection := diRebuildSelection(t, build)
	require.True(t, selection.allows(SynthCSharpDIIfaces))
	for _, count := range []uint64{1, 0} {
		adds, err := RunRebuildCSharpDIIfaces(ctx, build, rebuildGRPCReceipts(facts...), selection)
		require.NoError(t, err)
		require.Equal(t, count, adds)
		require.Equal(t, want, rebuildEventSavedRows(t, build))
	}
}

func TestRebuildCSharpDIIfacesSQLiteTieAndProof(t *testing.T) {
	const file = "Module.cs"
	impl := ifaceRebuildNode(file, "Svc", "csharp", graph.KindType)
	reg := &graph.Node{ID: file, Kind: graph.KindFile, FilePath: file, Language: "csharp"}
	z := ifaceRebuildNode(file, "z", "csharp", graph.KindInterface)
	a := ifaceRebuildNode(file, "a", "csharp", graph.KindInterface)
	z.Name, a.Name = "IThing", "IThing"
	z.StartLine, a.StartLine = 11, 12
	marker := diIfacesMarker(file, "Svc")
	zArc := ifaceRebuildEdge(impl, z.ID, graph.EdgeImplements, 3)
	aArc := ifaceRebuildEdge(impl, a.ID, graph.EdgeImplements, 3)
	facts := rebuildEventFacts(file, "csharp", []*graph.Node{reg, impl, z, a}, marker, zArc, aArc)
	for _, omit := range []bool{false, true} {
		build, native := openRebuildEventStores(t, facts)
		require.Equal(t, 1, ResolveCSharpDIImplementedInterfaces(native))
		want := rebuildEventSQLiteRows(native)
		require.Contains(t, want.edges, csharpDIProvideEdge(file, "Svc", z))
		before := rebuildEventSavedRows(t, build)
		receipt := facts
		if omit {
			receipt.Edges = slices.DeleteFunc(slices.Clone(facts.Edges), func(e *graph.Edge) bool { return e == zArc })
		}
		adds, err := RunRebuildCSharpDIIfaces(t.Context(), build, rebuildGRPCReceipts(receipt), diRebuildSelection(t, build))
		if omit {
			require.ErrorIs(t, err, store_pebble.ErrRebuildCSharpDIIfaces)
			var refusal *store_pebble.RebuildUnknownProvenanceError
			require.ErrorAs(t, err, &refusal)
			require.Equal(t, graph.EdgeIdentityFor(csharpDIProvideEdge(file, "Svc", z)), refusal.First)
			require.Zero(t, adds)
			require.Equal(t, before, rebuildEventSavedRows(t, build))
		} else {
			require.NoError(t, err)
			require.Equal(t, uint64(1), adds)
			require.Equal(t, want, rebuildEventSavedRows(t, build))
		}
	}
}

func TestRebuildCSharpDIIfacesFrozenSelection(t *testing.T) {
	build := openRebuildGRPCBuild(t)
	frozen := diRebuildSelection(t, build)
	require.False(t, frozen.allows(SynthCSharpDIIfaces))
	build.AddNode(ifaceRebuildNode("Module.cs", "Svc", "csharp", graph.KindType))
	require.True(t, diRebuildSelection(t, build).allows(SynthCSharpDIIfaces))
	adds, err := RunRebuildCSharpDIIfaces(t.Context(), build, nil, frozen)
	require.NoError(t, err)
	require.Zero(t, adds)
}
