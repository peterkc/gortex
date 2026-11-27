package resolver

import (
	"context"
	"iter"
	"maps"
	"strings"
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

func TestRebuildGodotAutoloadMemberIDOrder(t *testing.T) {
	facts, _, method := rebuildGodotAutoloadFacts()
	first := *method
	first.ID = facts[1].File.FilePath + "::a-method"
	facts[1] = rebuildEventFacts(facts[1].File.FilePath, "gdscript", append(facts[1].Nodes, &first))
	for _, omit := range []string{"none", "higher ID", "lowest ID"} {
		t.Run(omit, func(t *testing.T) {
			build, native := openRebuildEventStores(t, facts...)
			before := rebuildEventSavedRows(t, build)
			receipts := append([]store_pebble.ParseFacts(nil), facts...)
			fileNode, function := facts[1].Nodes[0], facts[1].Nodes[1]
			switch omit {
			case "higher ID":
				receipts[1] = rebuildEventFacts(facts[1].File.FilePath, "gdscript", []*graph.Node{fileNode, function, &first})
			case "lowest ID":
				receipts[1] = rebuildEventFacts(facts[1].File.FilePath, "gdscript", []*graph.Node{fileNode, function, method})
			}
			changed, err := RunRebuildGodotAutoload(t.Context(), build, rebuildGRPCReceipts(receipts...), rebuildGodotAutoloadSelection(t, build))
			if omit == "lowest ID" {
				require.ErrorIs(t, err, store_pebble.ErrRebuildUnknownProvenance)
				require.Zero(t, changed)
				require.Equal(t, before, rebuildEventSavedRows(t, build))
				return
			}
			require.NoError(t, err)
			require.EqualValues(t, 1, changed)
			require.Equal(t, 1, ResolveGDScriptAutoloads(native))
			want := rebuildEventSQLiteRows(native)
			require.Len(t, want.edges, 1)
			require.Equal(t, first.ID, want.edges[0].To, "the smallest Method ID wins over receipt order and the Function")
			require.Equal(t, want, rebuildEventSavedRows(t, build))
		})
	}
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

func TestRebuildGodotScriptNodeRowCaps(t *testing.T) {
	autoloadFacts, autoloadCall, target := rebuildGodotAutoloadFacts()
	const source = "game/start.gd"
	alias := &graph.Node{ID: source + "::Clock", Name: "Clock", Kind: graph.KindVariable, FilePath: source, Language: "gdscript",
		Meta: map[string]any{"gd_preload": "res://src/Clock.gd"}}
	caller := &graph.Node{ID: source + "::probe", Kind: graph.KindFunction, FilePath: source, Language: "gdscript"}
	preloadCall := *autoloadCall
	preloadCall.From, preloadCall.FilePath = caller.ID, source
	preloadFacts := []store_pebble.ParseFacts{
		rebuildEventFacts(source, "gdscript", []*graph.Node{alias, caller}, &preloadCall),
		autoloadFacts[1],
	}
	padding := strings.Repeat("x", 16<<20)
	for _, stage := range []struct {
		name  string
		err   error
		facts []store_pebble.ParseFacts
		run   func(context.Context, *store_pebble.BuildStore, iter.Seq2[store_pebble.ParseFacts, error], FrameworkSynthesizerSelection) (uint64, error)
	}{
		{SynthGodotAutoload, store_pebble.ErrRebuildGodotAutoload, autoloadFacts, RunRebuildGodotAutoload},
		{SynthGodotPreloadAlias, store_pebble.ErrRebuildGodotPreloadAlias, preloadFacts, RunRebuildGodotPreloadAlias},
	} {
		t.Run(stage.name, func(t *testing.T) {
			selected, err := NewFrameworkSynthesizerSelection([]string{stage.name})
			require.NoError(t, err)
			unrelated := *stage.facts[0].Nodes[0]
			unrelated.ID += "-unused"
			unrelated.Meta = nil // Same candidate kind, but no valid declaration.
			for _, fixture := range []struct {
				name  string
				node  *graph.Node
				fails bool
			}{
				{"unrelated", &unrelated, false},
				{"declaration", stage.facts[0].Nodes[0], true},
				{"script file", stage.facts[1].Nodes[0], true},
				{"member", target, true},
			} {
				t.Run(fixture.name, func(t *testing.T) {
					build := openRebuildGRPCBuild(t, stage.facts...)
					frozen, err := FreezeRebuildFrameworkSelection(t.Context(), build.RebuildView(), selected)
					require.NoError(t, err)
					require.True(t, frozen.allows(stage.name))
					large := *fixture.node
					large.Meta = maps.Clone(large.Meta)
					if large.Meta == nil {
						large.Meta = make(map[string]any)
					}
					large.Meta["unrelated"] = padding
					build.AddNode(&large)
					before := rebuildEventSavedRows(t, build).edges
					moves, err := stage.run(t.Context(), build, rebuildGRPCReceipts(stage.facts...), frozen)
					after := rebuildEventSavedRows(t, build).edges
					if fixture.fails {
						require.ErrorIs(t, err, stage.err)
						require.ErrorIs(t, err, store_pebble.ErrRebuildLinkInputSize)
						require.Zero(t, moves)
						require.Equal(t, before, after)
						return
					}
					require.NoError(t, err)
					require.EqualValues(t, 1, moves)
					require.Len(t, after, 1)
					require.Equal(t, target.ID, after[0].To)
				})
			}
		})
	}
}

func TestRebuildGodotAutoloadStationaryReceipt(t *testing.T) {
	facts, call, method := rebuildGodotAutoloadFacts()
	stationary := *call
	stationary.To, stationary.Line = method.ID, call.Line+1
	stationary.Context = strings.Repeat("x", 16<<20)
	facts[2].Edges = []*graph.Edge{call, &stationary}
	build, native := openRebuildEventStores(t, facts...)
	receipt := stationary
	receipt.Context = "different unused context"
	receipt.Confidence = 0.5
	facts[2].Edges = []*graph.Edge{call, &receipt}
	changed, err := RunRebuildGodotAutoload(t.Context(), build, rebuildGRPCReceipts(facts...), rebuildGodotAutoloadSelection(t, build))
	require.NoError(t, err)
	require.EqualValues(t, 1, changed)
	require.Equal(t, 1, ResolveGDScriptAutoloads(native))
	saved := rebuildEventSavedRows(t, build)
	require.Equal(t, rebuildEventSQLiteRows(native), saved)
	require.Contains(t, saved.edges, &stationary)
}
