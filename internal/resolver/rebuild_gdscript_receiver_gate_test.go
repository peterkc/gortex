package resolver

import (
	"maps"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/zzet/gortex/internal/graph"
)

func TestRebuildGDScriptReceiverGateSQLiteOracle(t *testing.T) {
	caller := rebuildEventNode("caller.go", "invoke", "go", "")
	target := rebuildEventNode("owner.gd", "apply", "gdscript", "")
	target.Kind = graph.KindMethod
	target.Meta = map[string]any{"receiver": "Parent"}
	class := func(file, name, parent string, opaque bool) *graph.Node {
		n := rebuildEventNode(file, name, "gdscript", "")
		n.Kind = graph.KindType
		n.Meta = map[string]any{"gd_kind": "class_name", "gd_extends": parent, "gd_extends_opaque": opaque}
		return n
	}
	variable := rebuildEventNode("local.gd", "local", "gdscript", "")
	variable.Kind = graph.KindVariable
	preload := rebuildEventNode("local.gd", "Persist", "gdscript", "")
	preload.Kind = graph.KindVariable
	preload.Meta = map[string]any{"gd_preload": "res://owner.gd"}
	file := &graph.Node{ID: target.FilePath, Kind: graph.KindFile, FilePath: target.FilePath, Language: "gdscript"}
	manifest := rebuildEventNode("project.godot", "Singleton", "godot_project", "")
	manifest.Kind = graph.KindType
	manifest.Meta = map[string]any{"gd_kind": "autoload", "gd_script": "owner.gd"}
	makeEdge := func(from *graph.Node, kind graph.EdgeKind, line int, recv string) *graph.Edge {
		return &graph.Edge{From: from.ID, To: target.ID, Kind: kind, FilePath: from.FilePath, Line: line,
			Origin: graph.OriginASTResolved, Confidence: 0.9, ConfidenceLabel: graph.ConfidenceLabelFor(kind, 0.9),
			Tier: graph.ResolvedBy(graph.OriginASTResolved),
			Meta: map[string]any{"receiver_type": recv, "payload": map[string]any{"keep": true}},
		}
	}
	call := makeEdge(caller, graph.EdgeCalls, 1, "Input")
	ref := makeEdge(caller, graph.EdgeReferences, 1, "Input")
	localCaller := rebuildEventNode("local.gd", "invoke", "gdscript", "")
	b, sqlite := openRebuildEventStores(t,
		rebuildEventFacts(caller.FilePath, caller.Language, []*graph.Node{caller}, call, ref,
			makeEdge(caller, graph.EdgeCalls, 2, "Child"), makeEdge(caller, graph.EdgeCalls, 3, "Opaque"),
			makeEdge(caller, graph.EdgeCalls, 4, "Singleton"), makeEdge(caller, graph.EdgeReads, 1, "Input")),
		rebuildEventFacts(localCaller.FilePath, localCaller.Language, []*graph.Node{localCaller, variable, preload},
			makeEdge(localCaller, graph.EdgeCalls, 1, "local"), makeEdge(localCaller, graph.EdgeReferences, 2, "Persist")),
		rebuildEventFacts(target.FilePath, target.Language, []*graph.Node{target, file, class(target.FilePath, "Parent", "", false)}),
		rebuildEventFacts("child.gd", "gdscript", []*graph.Node{class("child.gd", "Child", "Parent", false)}),
		rebuildEventFacts("opaque.gd", "gdscript", []*graph.Node{class("opaque.gd", "Opaque", "", true)}),
		rebuildEventFacts(manifest.FilePath, manifest.Language, []*graph.Node{manifest}),
	)
	before := rebuildEventSQLiteRows(sqlite)
	require.Len(t, before.edges, 8)
	require.Equal(t, before, rebuildEventSavedRows(t, b))

	disabled, err := NewFrameworkSynthesizerSelection(nil)
	require.NoError(t, err)
	active, err := FreezeRebuildGDScriptReceiverGate(t.Context(), b.RebuildView(), disabled)
	require.NoError(t, err)
	require.False(t, active)
	count, err := RunRebuildGDScriptReceiverGate(t.Context(), b, active)
	require.NoError(t, err)
	require.Zero(t, count)
	require.Zero(t, RunFrameworkSynthesizersWithSelection(sqlite, disabled).GDScriptReceiverGated)
	require.Equal(t, before, rebuildEventSavedRows(t, b))
	require.Equal(t, before, rebuildEventSQLiteRows(sqlite))

	// No gRPC candidates: even though the registry filters it out, the raw
	// nonempty selection still admits the native pre-census tail.
	requested, err := NewFrameworkSynthesizerSelection([]string{SynthGRPCStub})
	require.NoError(t, err)
	frozen, err := FreezeRebuildFrameworkSelection(t.Context(), b.RebuildView(), requested)
	require.NoError(t, err)
	require.True(t, frozen.none())
	active, err = FreezeRebuildGDScriptReceiverGate(t.Context(), b.RebuildView(), requested)
	require.NoError(t, err)
	require.True(t, active)
	native := RunFrameworkSynthesizersWithSelection(sqlite, requested)
	require.NoError(t, native.Err)
	require.Equal(t, 2, native.GDScriptReceiverGated)
	count, err = RunRebuildGDScriptReceiverGate(t.Context(), b, active)
	require.NoError(t, err)
	require.EqualValues(t, 2, count)

	want := map[graph.EdgeIdentity]*graph.Edge{}
	for _, e := range before.edges {
		id := graph.EdgeIdentityFor(e)
		copy := *e
		if id == graph.EdgeIdentityFor(call) || id == graph.EdgeIdentityFor(ref) {
			copy.Origin = graph.OriginSpeculative
			copy.Meta = maps.Clone(e.Meta)
			copy.Meta[graph.MetaSpeculative] = true
			copy.Meta["demoted"] = "receiver_type_mismatch"
		}
		want[id] = &copy
	}
	after := rebuildEventSavedRows(t, b)
	require.Equal(t, before.nodes, after.nodes)
	require.Equal(t, rebuildEventSQLiteRows(sqlite), after)
	require.Len(t, after.edges, len(want))
	for _, e := range after.edges {
		require.Equal(t, want[graph.EdgeIdentityFor(e)], e)
	}
	count, err = RunRebuildGDScriptReceiverGate(t.Context(), b, active)
	require.NoError(t, err)
	require.Zero(t, count)
	require.Zero(t, RunFrameworkSynthesizersWithSelection(sqlite, requested).GDScriptReceiverGated)
	require.Equal(t, after, rebuildEventSavedRows(t, b))
	require.Equal(t, after, rebuildEventSQLiteRows(sqlite))
}
