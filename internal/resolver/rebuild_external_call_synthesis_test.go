package resolver

import (
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/graph/store_pebble"
)

func TestRebuildExternalCallSynthesisSQLiteOracle(t *testing.T) {
	const pkg = "acme.tools"
	for _, mode := range []string{"Python before Go", "Python after Go", "Python different package", "Dart before Go", "plugin connection", "disabled"} {
		t.Run(mode, func(t *testing.T) {
			goNode := rebuildEventNode("m.go", "invoke", "go", "")
			old := &graph.Edge{From: goNode.ID, To: "dep::" + pkg + "::Send", Kind: graph.EdgeCalls, FilePath: goNode.FilePath, Line: 7}
			file, lang, path := "a.py", "python", pkg
			switch mode {
			case "Python after Go":
				file = "z.py"
			case "Python different package":
				path = "other.tools"
			case "Dart before Go":
				file, lang = "a.dart", "dart"
			case "plugin connection":
				file, lang = "a.custom", "custom"
			}
			caller := rebuildEventNode(file, "call", lang, "")
			other := &graph.Edge{From: caller.ID, To: "unresolved::extern::" + path + "::fetch", Kind: graph.EdgeCalls, FilePath: file, Line: 9}
			facts := []store_pebble.ParseFacts{rebuildEventFacts(goNode.FilePath, goNode.Language, []*graph.Node{goNode}, old)}
			var sibling *graph.Edge
			if mode == "plugin connection" {
				const script = "handler.gd"
				other.Kind = graph.EdgeReferences
				other.To = "dep::" + path + "::fetch"
				other.Meta = map[string]any{"godot_connection": true, "godot_method": "handle"}
				sibling = &graph.Edge{From: caller.ID, To: script, Kind: graph.EdgeReferences, FilePath: file, Line: 10}
				method := rebuildEventNode(script, "handle", "gdscript", "")
				method.Kind = graph.KindMethod
				scriptNode := &graph.Node{ID: script, FilePath: script, Language: "gdscript", Kind: graph.KindFile}
				facts = append(facts, rebuildEventFacts(script, "gdscript", []*graph.Node{scriptNode, method}))
			}
			otherFacts := rebuildEventFacts(file, lang, []*graph.Node{caller}, other)
			if sibling != nil {
				otherFacts.Edges = append(otherFacts.Edges, sibling)
			}
			facts = append(facts, otherFacts)
			b, sqlite := openRebuildEventStores(t, facts...)
			before := rebuildEventSavedRows(t, b)
			count, err := RunRebuildExternalCallSynthesis(t.Context(), b, rebuildGRPCReceipts(facts...), mode != "disabled", nil)
			if mode == "disabled" {
				require.NoError(t, err)
				require.Zero(t, count)
				require.Equal(t, before, rebuildEventSavedRows(t, b))
				return
			}

			// Seed the known native extern result to isolate the post-framework
			// synthesis oracle from the full ResolveAll pipeline.
			if mode == "plugin connection" {
				require.Equal(t, 1, ResolveGodotSceneConnections(sqlite))
			} else {
				native := *other
				native.To = "dep::" + path + "::fetch"
				sqlite.ReindexEdges([]graph.EdgeReindex{{Edge: &native, OldTo: other.To}})
			}
			SynthesizeExternalCalls(sqlite, true)
			want := rebuildRetainedExternalCallRows(t, rebuildEventSQLiteRows(sqlite), goNode.ID)
			switch mode {
			case "Python before Go", "Dart before Go", "plugin connection":
				require.ErrorIs(t, err, store_pebble.ErrRebuildExternalCallSynthesis)
				require.Zero(t, count)
				require.Equal(t, before, rebuildEventSavedRows(t, b))
				if mode == "plugin connection" {
					require.Equal(t, "go", want.nodes[0].Language)
				} else {
					require.Equal(t, lang, want.nodes[0].Language)
				}
			default:
				require.NoError(t, err)
				require.EqualValues(t, 1, count)
				after := rebuildEventSavedRows(t, b)
				require.Equal(t, want, rebuildRetainedExternalCallRows(t, after, goNode.ID))
				_, err := RunRebuildExternalCallSynthesis(t.Context(), b, rebuildGRPCReceipts(facts...), true, nil)
				require.NoError(t, err)
				require.Equal(t, after, rebuildEventSavedRows(t, b))
			}
		})
	}
}

func rebuildRetainedExternalCallRows(t *testing.T, rows rebuildEventRows, caller string) rebuildEventRows {
	t.Helper()
	var kept rebuildEventRows
	for _, e := range rows.edges {
		if e.From == caller {
			kept.edges = append(kept.edges, e)
		}
	}
	require.Len(t, kept.edges, 1)
	for _, n := range rows.nodes {
		if n.ID == kept.edges[0].To {
			kept.nodes = append(kept.nodes, n)
		}
	}
	require.Len(t, kept.nodes, 1)
	return kept
}

func TestRebuildExternalCallConvergenceSQLiteOracle(t *testing.T) {
	const file, lang, pkg = "m.go", "go", "acme.tools"
	caller := rebuildEventNode(file, "invoke", lang, "")
	first := &graph.Edge{From: caller.ID, To: "dep::" + pkg + "::A", Kind: graph.EdgeCalls, FilePath: file, Line: 513,
		Meta: map[string]any{"marker": "first", "go_import": "old"}}
	last := *first
	last.To, last.Meta = "dep::"+pkg+"::Z", map[string]any{"marker": "last"}
	occupied := *first
	occupied.Line++
	incumbent := occupied
	incumbent.To, incumbent.Meta = externalCallNodeID("dep", pkg), map[string]any{"marker": "incumbent"}
	ref := last
	ref.Kind = graph.EdgeReferences
	edges := []*graph.Edge{&last, first, &incumbent, &occupied, &ref}
	for line := 1; line <= 511; line++ {
		copy := *first
		copy.Line = line
		edges = append(edges, &copy)
	}
	other := rebuildEventNode("z.ts", "fetch", "typescript", "")
	ts := &graph.Edge{From: other.ID, To: "dep::" + pkg + "::A", Kind: graph.EdgeCalls, FilePath: other.FilePath, Line: 1}
	facts := []store_pebble.ParseFacts{
		rebuildEventFacts(other.FilePath, other.Language, []*graph.Node{other}, ts),
		rebuildEventFacts(file, lang, []*graph.Node{caller}, edges...),
	}
	b, sqlite := openRebuildEventStores(t, facts...)
	count, err := RunRebuildExternalCallSynthesis(t.Context(), b, rebuildGRPCReceipts(facts...), true, nil)
	require.NoError(t, err)
	require.EqualValues(t, 515, count)
	SynthesizeExternalCalls(sqlite, true)
	want := rebuildEventSQLiteRows(sqlite)
	require.Equal(t, want, rebuildEventSavedRows(t, b))
	for _, e := range want.edges {
		if e.From != caller.ID || e.Kind != graph.EdgeCalls {
			continue
		}
		switch e.Line {
		case first.Line:
			require.Equal(t, "first", e.Meta["marker"])
			require.Equal(t, first.To, e.Meta["go_extern_target"])
			require.NotContains(t, e.Meta, "go_import")
		case incumbent.Line:
			require.Equal(t, "incumbent", e.Meta["marker"])
		}
	}
	count, err = RunRebuildExternalCallSynthesis(t.Context(), b, rebuildGRPCReceipts(facts...), true, nil)
	require.NoError(t, err)
	require.Zero(t, count)
	require.Equal(t, want, rebuildEventSavedRows(t, b))
}

func TestRebuildExternalCallSynthesisFrozenAlias(t *testing.T) {
	goNode := rebuildEventNode("z.go", "invoke", "go", "")
	plugin := rebuildEventNode("a.custom", "invoke", "custom", "")
	retained := &graph.Edge{From: goNode.ID, Kind: graph.EdgeCalls, To: "external::acme-tools", FilePath: goNode.FilePath}
	uncertain := &graph.Edge{From: plugin.ID, Kind: graph.EdgeReferences, To: "unresolved::import::tool-alias", FilePath: "recording.ts"}
	for _, real := range []string{"acme-tools", "other-tools"} {
		t.Run(real, func(t *testing.T) {
			facts := []store_pebble.ParseFacts{
				rebuildEventFacts(goNode.FilePath, goNode.Language, []*graph.Node{goNode}, retained),
				rebuildEventFacts(plugin.FilePath, plugin.Language, []*graph.Node{plugin}, uncertain),
			}
			b, native := openRebuildEventStores(t, facts...)
			before := rebuildEventSavedRows(t, b)
			calls := 0
			count, err := RunRebuildExternalCallSynthesis(t.Context(), b, rebuildGRPCReceipts(facts...), true,
				func(file, spec string) (string, error) {
					calls++
					require.Equal(t, uncertain.FilePath, file)
					require.Equal(t, "tool-alias", spec)
					return real, nil
				})
			require.Equal(t, 1, calls)
			if real == "acme-tools" {
				require.ErrorIs(t, err, store_pebble.ErrRebuildExternalCallSynthesis)
				require.Zero(t, count)
				require.Equal(t, before, rebuildEventSavedRows(t, b))
				return
			}
			require.NoError(t, err)
			require.EqualValues(t, 1, count)
			nativeEdge := *uncertain
			nativeEdge.To = "external::" + real
			native.ReindexEdges([]graph.EdgeReindex{{Edge: &nativeEdge, OldTo: uncertain.To}})
			SynthesizeExternalCalls(native, true)
			require.Equal(t, rebuildRetainedExternalCallRows(t, rebuildEventSQLiteRows(native), goNode.ID),
				rebuildRetainedExternalCallRows(t, rebuildEventSavedRows(t, b), goNode.ID))
		})
	}
}
