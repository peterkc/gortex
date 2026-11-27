package resolver

import (
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/graph/store_pebble"
)

func rebuildGodotAutoloadSelection(t *testing.T, build *store_pebble.BuildStore) FrameworkSynthesizerSelection {
	t.Helper()
	sel, err := NewFrameworkSynthesizerSelection([]string{SynthGodotAutoload})
	require.NoError(t, err)
	frozen, err := FreezeRebuildFrameworkSelection(t.Context(), build.RebuildView(), sel)
	require.NoError(t, err)
	require.True(t, frozen.allows(SynthGodotAutoload))
	return frozen
}

func rebuildGodotAutoloadFacts() (facts []store_pebble.ParseFacts, call *graph.Edge, method *graph.Node) {
	const manifest, script, source = "game/project.godot", "game/src/Clock.gd", "app.go"
	autoload := &graph.Node{ID: manifest + "::Clock", Name: "Clock", Kind: graph.KindType, Language: "godot_project", FilePath: manifest,
		Meta: map[string]any{"gd_kind": "autoload", "gd_script": "src/Clock.gd"}}
	file := &graph.Node{ID: script, Name: script, Kind: graph.KindFile, Language: "gdscript", FilePath: script}
	fn := &graph.Node{ID: script + "::a-tick", Name: "tick", Kind: graph.KindFunction, Language: "gdscript", FilePath: script}
	method = &graph.Node{ID: script + "::z-tick", Name: "tick", Kind: graph.KindMethod, Language: "gdscript", FilePath: script}
	caller := &graph.Node{ID: source + "::caller", Name: "caller", Kind: graph.KindFunction, Language: "go", FilePath: source}
	call = &graph.Edge{From: caller.ID, To: "unresolved::*.tick", Kind: graph.EdgeCalls, FilePath: source, Line: 4,
		Meta: map[string]any{"receiver_type": autoload.Name}}
	return []store_pebble.ParseFacts{
		rebuildEventFacts(manifest, "godot_project", []*graph.Node{autoload}),
		rebuildEventFacts(script, "gdscript", []*graph.Node{file, fn, method}),
		rebuildEventFacts(source, "go", []*graph.Node{caller}, call),
	}, call, method
}

func TestRebuildGodotAutoloadSQLiteOracle(t *testing.T) {
	facts, call, method := rebuildGodotAutoloadFacts()
	const otherFile = "other/src/Clock.gd"
	ambiguous := &graph.Node{ID: otherFile, Name: otherFile, Kind: graph.KindFile, Language: "gdscript", FilePath: otherFile}
	const otherManifest = "other/project.godot"
	conflict := &graph.Node{ID: otherManifest + "::Clock", Name: "Clock", Kind: graph.KindType, Language: "godot_project", FilePath: otherManifest,
		Meta: map[string]any{"gd_kind": "autoload", "gd_script": "another/Clock.gd"}}
	for _, tc := range []struct {
		name  string
		extra *graph.Node
	}{
		{name: "unique"},
		{name: "ambiguous script", extra: ambiguous},
		{name: "conflicting declaration", extra: conflict},
	} {
		t.Run(tc.name, func(t *testing.T) {
			inputs := facts
			if tc.extra != nil {
				inputs = append(inputs, rebuildEventFacts(tc.extra.FilePath, tc.extra.Language, []*graph.Node{tc.extra}))
			}
			build, native := openRebuildEventStores(t, inputs...)
			selection := rebuildGodotAutoloadSelection(t, build)
			want := 0
			if tc.extra == nil {
				want = 1
			}
			changed, err := RunRebuildGodotAutoload(t.Context(), build, rebuildGRPCReceipts(inputs...), selection)
			require.NoError(t, err)
			require.EqualValues(t, want, changed)
			require.Equal(t, want, ResolveGDScriptAutoloads(native))
			saved := rebuildEventSavedRows(t, build)
			require.Equal(t, rebuildEventSQLiteRows(native), saved)
			require.Len(t, saved.edges, 1)
			if want == 1 {
				require.Equal(t, method.ID, saved.edges[0].To, "Method wins even with an earlier Function receipt")
			} else {
				require.Equal(t, call, saved.edges[0])
			}
		})
	}
}

func TestRebuildGodotAutoloadFrozenMemberOrder(t *testing.T) {
	facts, _, method := rebuildGodotAutoloadFacts()
	first := *method
	first.ID = facts[1].File.FilePath + "::a-method"
	facts[1] = rebuildEventFacts(facts[1].File.FilePath, "gdscript", append(facts[1].Nodes, &first))
	build := openRebuildGRPCBuild(t, facts...)
	changed, err := RunRebuildGodotAutoload(t.Context(), build, rebuildGRPCReceipts(facts...), rebuildGodotAutoloadSelection(t, build))
	require.NoError(t, err)
	require.EqualValues(t, 1, changed)
	rows := rebuildEventSavedRows(t, build).edges
	require.Len(t, rows, 1)
	require.Equal(t, method.ID, rows[0].To, "receipt order, not ID order, breaks ties within a kind")
}

func TestRebuildGodotAutoloadExactProof(t *testing.T) {
	facts, call, _ := rebuildGodotAutoloadFacts()
	const otherFile = "other/src/Clock.gd"
	other := &graph.Node{ID: otherFile, Name: otherFile, Kind: graph.KindFile, Language: "gdscript", FilePath: otherFile}
	for _, which := range []string{"manifest", "method", "call", "competing file", "lower-priority function"} {
		t.Run(which, func(t *testing.T) {
			inputs := facts
			if which == "competing file" {
				inputs = append(inputs, rebuildEventFacts(otherFile, "gdscript", []*graph.Node{other}))
			}
			build := openRebuildGRPCBuild(t, inputs...)
			before := rebuildEventSavedRows(t, build)
			receipts := append([]store_pebble.ParseFacts(nil), facts...)
			switch which {
			case "manifest":
				receipts = receipts[1:]
			case "method":
				receipts[1] = rebuildEventFacts(facts[1].File.FilePath, "gdscript", facts[1].Nodes[:2])
			case "call":
				receipts[2].Edges = nil
			case "lower-priority function":
				receipts[1] = rebuildEventFacts(facts[1].File.FilePath, "gdscript", []*graph.Node{facts[1].Nodes[0], facts[1].Nodes[2]})
			}
			changed, err := RunRebuildGodotAutoload(t.Context(), build, rebuildGRPCReceipts(receipts...), rebuildGodotAutoloadSelection(t, build))
			if which == "lower-priority function" {
				require.NoError(t, err, "an unproved Function cannot displace a Method")
				require.EqualValues(t, 1, changed)
				return
			}
			require.Zero(t, changed)
			require.ErrorIs(t, err, store_pebble.ErrRebuildGodotAutoload)
			var refused *store_pebble.RebuildUnknownProvenanceError
			require.ErrorAs(t, err, &refused)
			require.EqualValues(t, 1, refused.Count)
			require.Equal(t, graph.EdgeIdentityFor(call), refused.First)
			require.Equal(t, before, rebuildEventSavedRows(t, build))
		})
	}
}
