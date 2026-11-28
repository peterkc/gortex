package resolver

import (
	"iter"
	"maps"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/graph/store_pebble"
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

func TestRebuildGDScriptReceiverGateWithoutClasses(t *testing.T) {
	const file = "project.godot"
	manifest := &graph.Node{ID: file + "::Singleton", Name: "Singleton", Kind: graph.KindType, FilePath: file, Language: "godot_project",
		Meta: map[string]any{"gd_kind": "autoload", "gd_script": "owner.gd"}}
	script := &graph.Node{ID: "owner.gd", Kind: graph.KindFile, FilePath: "owner.gd", Language: "gdscript"}
	for _, hasClass := range []bool{false, true} {
		b := openRebuildGRPCBuild(t, rebuildEventFacts(file, "godot_project", []*graph.Node{manifest}),
			rebuildEventFacts(script.FilePath, script.Language, []*graph.Node{script}))
		class := &graph.Node{ID: "owner.gd::Owner", Name: "Owner", Kind: graph.KindType, FilePath: script.FilePath, Language: script.Language,
			Meta: map[string]any{"gd_kind": "class_name"}}
		if hasClass {
			b.AddNode(class)
		}
		large := *manifest
		large.Meta = maps.Clone(manifest.Meta)
		large.Meta["gd_script"] = strings.Repeat("x", rebuildGDScriptGateEvidenceBytes)
		b.AddNode(&large)
		count, err := RunRebuildGDScriptReceiverGate(t.Context(), b, true)
		require.Zero(t, count)
		if hasClass {
			require.ErrorIs(t, err, store_pebble.ErrRebuildLinkInputSize)
		} else {
			require.NoError(t, err)
		}
	}
}

func TestRebuildGDScriptReceiverGateSkipsUnusedEvidenceBudget(t *testing.T) {
	class := &graph.Node{ID: "owner.gd::Owner", Name: "Owner", Kind: graph.KindType, Language: "gdscript",
		Meta: map[string]any{"gd_kind": "class_name"}}
	manifest := &graph.Node{ID: "project.godot::Singleton", Name: "Singleton", Kind: graph.KindType, Language: "godot_project",
		Meta: map[string]any{"gd_kind": "autoload", "gd_script": strings.Repeat("x", rebuildGDScriptGateEvidenceBytes/2)}}
	alias := &graph.Node{ID: "owner.gd::Alias", Name: "Alias", Kind: graph.KindVariable, Language: "gdscript",
		Meta: map[string]any{"gd_preload": "res://" + strings.Repeat("x", rebuildGDScriptGateEvidenceBytes/2) + ".gd"}}
	for _, hasClass := range []bool{false, true} {
		classes := iter.Seq2[*graph.Node, error](func(yield func(*graph.Node, error) bool) {
			if hasClass {
				yield(class, nil)
			}
		})
		scripts := iter.Seq2[*graph.Node, error](func(yield func(*graph.Node, error) bool) {
			if yield(manifest, nil) {
				yield(alias, nil)
			}
		})
		demote, err := prepareRebuildGDScriptReceiverGate(classes, scripts)
		require.Nil(t, demote)
		if hasClass {
			require.ErrorIs(t, err, store_pebble.ErrRebuildLinkInputSize)
		} else {
			require.NoError(t, err)
		}
	}
}

func TestRebuildGDScriptGateCandidate(t *testing.T) {
	for _, tc := range []struct {
		kind     graph.NodeKind
		language string
		classes  bool
		full     bool
		want     bool
	}{
		{graph.KindType, "gdscript", true, true, true},
		{graph.KindType, "go", true, true, false},
		{graph.KindFile, "gdscript", false, false, true},
		{graph.KindType, "godot_project", false, true, true},
		{graph.KindVariable, "gdscript", false, true, true},
		{graph.KindType, "gdscript", false, true, false},
		{graph.KindFunction, "gdscript", false, true, false},
		{graph.KindVariable, "go", false, true, false},
	} {
		full, ok := rebuildGDScriptGateCandidate(&graph.Node{Kind: tc.kind, Language: tc.language}, tc.classes)
		require.Equal(t, tc.want, ok)
		if ok {
			require.Equal(t, tc.full, full)
		}
	}
}
