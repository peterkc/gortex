package resolver

import (
	"slices"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/graph/store_pebble"
	"github.com/zzet/gortex/internal/parser"
	"github.com/zzet/gortex/internal/parser/languages"
)

func TestRebuildGodotPreloadAliasSQLiteOracle(t *testing.T) {
	const (
		a        = "game/A.gd"
		b        = "game/B.gd"
		left     = "game/left/Util.gd"
		right    = "game/right/Util.gd"
		runTask  = "unresolved::*.run_task"
		leftOnly = "unresolved::*.left_only"
	)
	gd := languages.NewGDScriptExtractor()
	extract := func(x parser.Extractor, file, source string) store_pebble.ParseFacts {
		parsed, err := x.Extract(file, []byte(source))
		require.NoError(t, err)
		defer parsed.ReleaseTree()
		facts := rebuildEventFacts(file, x.Language(), parsed.Nodes, parsed.Edges...)
		facts.File.Size = len(source)
		return roundTripRebuildRNBridgeFacts(t, facts)
	}
	// The right script doubles as the autoload target. Both scripts also
	// match res://Util.gd, but only the left script ends with eft/Util.gd.
	facts := []store_pebble.ParseFacts{
		extract(languages.NewGodotProjectExtractor(), "game/project.godot", `[autoload]
Clock="*res://right/Util.gd"
`),
		extract(gd, left, `static func run_task():
    pass
static func overlap():
    pass
static func left_only():
    pass
`),
		extract(gd, right, `static func run_task():
    pass
static func overlap():
    pass
static func right_only():
    pass
`),
		extract(gd, a, `const Tools = preload("res://left/Util.gd")
const Clock = preload("res://left/Util.gd")
const Ambiguous = preload("res://Util.gd")
const Wrong = preload("res://eft/Util.gd")
var result = Tools.run_task()
func probe():
    Clock.overlap()
    Clock.left_only()
    Tools.right_only()
    Ambiguous.run_task()
    Wrong.run_task()
`),
		extract(gd, b, `const Tools = preload("res://right/Util.gd")
func probe():
    Tools.run_task()
`),
	}
	selected, err := NewFrameworkSynthesizerSelection([]string{SynthGodotAutoload, SynthGodotPreloadAlias})
	require.NoError(t, err)
	for _, name := range []string{"complete", "missing_winning_alias"} {
		t.Run(name, func(t *testing.T) {
			build, native := openRebuildEventStores(t, facts...)
			selection, err := FreezeRebuildFrameworkSelection(t.Context(), build.RebuildView(), selected)
			require.NoError(t, err)
			require.True(t, selection.allows(SynthGodotAutoload))
			require.True(t, selection.allows(SynthGodotPreloadAlias))

			receipts := slices.Clone(facts)
			if name == "missing_winning_alias" {
				// Omit only B's live alias, not any input deciding autoload.
				input := &receipts[len(receipts)-1]
				input.Nodes = slices.DeleteFunc(slices.Clone(input.Nodes), func(n *graph.Node) bool {
					return n.ID == b+"::Tools"
				})
				require.Len(t, input.Nodes, input.File.NodeCount-1)
				input.File.NodeCount = len(input.Nodes)
			}
			autoload, err := RunRebuildGodotAutoload(t.Context(), build, rebuildGRPCReceipts(receipts...), selection)
			require.NoError(t, err)
			require.EqualValues(t, 1, autoload)
			require.Equal(t, 1, ResolveGDScriptAutoloads(native))
			before := rebuildEventSavedRows(t, build)
			require.Equal(t, rebuildEventSQLiteRows(native), before, "after autoload")
			require.Equal(t, leftOnly, rebuildGodotPreloadCallAt(t, before, a+"::probe", 8).To)

			changed, err := RunRebuildGodotPreloadAlias(t.Context(), build, rebuildGRPCReceipts(receipts...), selection)
			after := rebuildEventSavedRows(t, build)
			if name == "missing_winning_alias" {
				require.Zero(t, changed)
				var refusal *store_pebble.RebuildUnknownProvenanceError
				require.ErrorAs(t, err, &refusal)
				require.EqualValues(t, 1, refusal.Count)
				require.Equal(t, graph.EdgeIdentity{
					From: b + "::probe", To: runTask, Kind: graph.EdgeCalls, FilePath: b, Line: 3,
				}, refusal.First)
				require.Equal(t, before, after, "alias proof refusal must leave all post-autoload rows unchanged")
				return
			}
			require.NoError(t, err)
			require.EqualValues(t, 3, changed)
			require.Equal(t, 3, ResolveGDScriptPreloadAliases(native))
			require.Equal(t, rebuildEventSQLiteRows(native), after, "after autoload then preload")
			for _, want := range []struct {
				from  string
				line  int
				to    string
				stamp string
			}{
				{a, 5, left + "::run_task", SynthGodotPreloadAlias},
				{b + "::probe", 3, right + "::run_task", SynthGodotPreloadAlias},
				{a + "::probe", 7, right + "::overlap", SynthGodotAutoload},
				{a + "::probe", 8, left + "::left_only", SynthGodotPreloadAlias},
				{a + "::probe", 9, "unresolved::*.right_only", ""},
				{a + "::probe", 10, runTask, ""},
				{a + "::probe", 11, runTask, ""},
			} {
				call := rebuildGodotPreloadCallAt(t, after, want.from, want.line)
				require.Equal(t, want.to, call.To, "%s:%d", want.from, want.line)
				if want.stamp != "" {
					require.Equal(t, want.stamp, call.Meta[MetaSynthesizedBy], "%s:%d", want.from, want.line)
				}
				if want.stamp != SynthGodotPreloadAlias {
					require.Equal(t, rebuildGodotPreloadCallAt(t, before, want.from, want.line), call)
				}
			}
		})
	}
}

func rebuildGodotPreloadCallAt(t *testing.T, rows rebuildEventRows, from string, line int) *graph.Edge {
	t.Helper()
	var calls []*graph.Edge
	for _, e := range rows.edges {
		if e.Kind == graph.EdgeCalls && e.From == from && e.Line == line {
			calls = append(calls, e)
		}
	}
	require.Len(t, calls, 1, "Call from %s:%d", from, line)
	return calls[0]
}

func TestRebuildGodotPreloadAliasDeclarationOrder(t *testing.T) {
	const source = "game/start.gd"
	first := &graph.Node{ID: source + "::z-alias", Name: "Clock", Kind: graph.KindVariable, Language: "gdscript", FilePath: source,
		Meta: map[string]any{"gd_preload": "res://Early.gd"}}
	last := &graph.Node{ID: source + "::a-alias", Name: first.Name, Kind: first.Kind, Language: first.Language, FilePath: source,
		Meta: map[string]any{"gd_preload": "res://Late.gd"}}
	caller := &graph.Node{ID: source + "::caller", Name: "caller", Kind: graph.KindFunction, Language: "gdscript", FilePath: source}
	call := &graph.Edge{From: caller.ID, To: "unresolved::*.tick", Kind: graph.EdgeCalls, FilePath: source, Line: 4,
		Meta: map[string]any{"receiver_type": first.Name}}
	script := func(path string) store_pebble.ParseFacts {
		file := &graph.Node{ID: path, Name: path, Kind: graph.KindFile, Language: "gdscript", FilePath: path}
		member := &graph.Node{ID: path + "::tick", Name: "tick", Kind: graph.KindMethod, Language: "gdscript", FilePath: path}
		return rebuildEventFacts(path, "gdscript", []*graph.Node{file, member})
	}
	facts := []store_pebble.ParseFacts{
		rebuildEventFacts(source, "gdscript", []*graph.Node{caller, first, last}, call),
		script("game/Early.gd"), script("game/Late.gd"),
	}
	sel, err := NewFrameworkSynthesizerSelection([]string{SynthGodotPreloadAlias})
	require.NoError(t, err)
	for _, tc := range []struct {
		name string
		omit *graph.Node
	}{
		{name: "complete"},
		{name: "missing lower ID", omit: last},
		{name: "missing greatest ID", omit: first},
	} {
		t.Run(tc.name, func(t *testing.T) {
			build, native := openRebuildEventStores(t, facts...)
			before := rebuildEventSavedRows(t, build)
			frozen, err := FreezeRebuildFrameworkSelection(t.Context(), build.RebuildView(), sel)
			require.NoError(t, err)
			receipts := slices.Clone(facts)
			if tc.omit != nil {
				nodes := slices.DeleteFunc(slices.Clone(facts[0].Nodes), func(n *graph.Node) bool {
					return n.ID == tc.omit.ID
				})
				receipts[0] = rebuildEventFacts(source, "gdscript", nodes, call)
			}
			changed, err := RunRebuildGodotPreloadAlias(t.Context(), build, rebuildGRPCReceipts(receipts...), frozen)
			saved := rebuildEventSavedRows(t, build)
			if tc.omit == first {
				require.ErrorIs(t, err, store_pebble.ErrRebuildUnknownProvenance)
				require.Zero(t, changed)
				require.Equal(t, before, saved)
				return
			}
			require.NoError(t, err)
			require.EqualValues(t, 1, changed)
			require.Equal(t, 1, ResolveGDScriptPreloadAliases(native))
			want := rebuildEventSQLiteRows(native)
			require.Len(t, want.edges, 1)
			// KindVariable scans in ID order. The z-alias wins even
			// though its receipt appears before the a-alias receipt.
			require.Equal(t, "game/Early.gd::tick", want.edges[0].To)
			require.Equal(t, want, saved)
		})
	}
}
