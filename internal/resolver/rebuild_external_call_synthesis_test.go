package resolver

import (
	"context"
	"fmt"
	"iter"
	"slices"
	"strings"
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
			count, err := runRebuildExternalCallSynthesisTest(t.Context(), b, rebuildGRPCReceipts(facts...), mode != "disabled", nil)
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
				_, err := runRebuildExternalCallSynthesisTest(t.Context(), b, rebuildGRPCReceipts(facts...), true, nil)
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
	count, err := runRebuildExternalCallSynthesisTest(t.Context(), b, rebuildGRPCReceipts(facts...), true, nil)
	require.NoError(t, err)
	require.EqualValues(t, 516, count)
	require.EqualValues(t, count, SynthesizeExternalCalls(sqlite, true))
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
	count, err = runRebuildExternalCallSynthesisTest(t.Context(), b, rebuildGRPCReceipts(facts...), true, nil)
	require.NoError(t, err)
	require.EqualValues(t, 515, count)
	require.EqualValues(t, count, SynthesizeExternalCalls(sqlite, true))
	require.Equal(t, want, rebuildEventSavedRows(t, b))
}

func TestRebuildExternalCallSynthesisFrozenAlias(t *testing.T) {
	const shared = "acme-tools.example"
	goNode := rebuildEventNode("z.go", "invoke", "go", "")
	plugin := rebuildEventNode("a.custom", "invoke", "custom", "")
	retained := &graph.Edge{From: goNode.ID, Kind: graph.EdgeCalls, To: "external::" + shared, FilePath: goNode.FilePath}
	uncertain := &graph.Edge{From: plugin.ID, Kind: graph.EdgeReferences, To: "unresolved::import::tool-alias", FilePath: "recording.ts"}
	for _, real := range []string{shared, "other-tools.example"} {
		t.Run(real, func(t *testing.T) {
			facts := []store_pebble.ParseFacts{
				rebuildEventFacts(goNode.FilePath, goNode.Language, []*graph.Node{goNode}, retained),
				rebuildEventFacts(plugin.FilePath, plugin.Language, []*graph.Node{plugin}, uncertain),
			}
			b, native := openRebuildEventStores(t, facts...)
			before := rebuildEventSavedRows(t, b)
			calls := 0
			count, err := runRebuildExternalCallSynthesisTest(t.Context(), b, rebuildGRPCReceipts(facts...), true,
				func(file, spec string) (string, error) {
					calls++
					require.Equal(t, uncertain.FilePath, file)
					require.Equal(t, "tool-alias", spec)
					return real, nil
				})
			require.Equal(t, 1, calls)
			if real == shared {
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

func runRebuildExternalCallSynthesisTest(ctx context.Context, b *store_pebble.BuildStore,
	receipts iter.Seq2[store_pebble.ParseFacts, error], enabled bool, rewrite func(string, string) (string, error),
) (uint64, error) {
	requested := AllFrameworkSynthesizers()
	frozen, err := FreezeRebuildFrameworkSelection(ctx, b.RebuildView(), requested)
	if err != nil {
		return 0, err
	}
	return RunRebuildExternalCallSynthesis(ctx, b, receipts, enabled, requested, frozen, rewrite)
}

func TestRebuildExternalCallStableDeferredTerminal(t *testing.T) {
	for _, tagged := range []bool{false, true} {
		for _, selected := range []bool{false, true} {
			name := fmt.Sprintf("tagged=%v/selected=%v", tagged, selected)
			t.Run(name, func(t *testing.T) {
				const target = "dep::acme.tools::run"
				goCaller := rebuildEventNode("z.go", "Run", "go", "")
				plugin := rebuildEventNode("a.custom", "run", "custom", "")
				retained := &graph.Edge{From: goCaller.ID, To: target, Kind: graph.EdgeCalls, FilePath: goCaller.FilePath}
				stable := &graph.Edge{From: plugin.ID, To: target, Kind: graph.EdgeReferences, FilePath: plugin.FilePath}
				if tagged {
					stable.Meta = map[string]any{"godot_connection": true, "godot_method": "handle"}
				}
				facts := []store_pebble.ParseFacts{
					rebuildEventFacts(goCaller.FilePath, goCaller.Language, []*graph.Node{goCaller}, retained),
					rebuildEventFacts(plugin.FilePath, plugin.Language, []*graph.Node{plugin}, stable),
				}
				b, sqlite := openRebuildEventStores(t, facts...)
				var names []string
				if selected {
					names = append(names, SynthGodotConnection)
				}
				requested, err := NewFrameworkSynthesizerSelection(names)
				require.NoError(t, err)
				frozen, err := FreezeRebuildFrameworkSelection(t.Context(), b.RebuildView(), requested)
				require.NoError(t, err)
				count, err := RunRebuildExternalCallSynthesis(t.Context(), b, rebuildGRPCReceipts(facts...), true, requested, frozen, nil)
				require.NoError(t, err)
				require.EqualValues(t, 2, count)
				if selected {
					require.Zero(t, ResolveGodotSceneConnections(sqlite))
				}
				require.EqualValues(t, count, SynthesizeExternalCalls(sqlite, true))
				require.Equal(t, rebuildEventSQLiteRows(sqlite), rebuildEventSavedRows(t, b))
			})
		}
	}
}

func TestRebuildExternalCallDeferredIndexedTerminal(t *testing.T) {
	for _, match := range []bool{true, false} {
		t.Run(fmt.Sprint(match), func(t *testing.T) {
			const target = "external::acme.tools"
			goCaller := rebuildEventNode("z.go", "Run", "go", "")
			caller := rebuildEventNode("a.custom", "run", "custom", "")
			decl := rebuildEventNode(caller.FilePath, "fetch", caller.Language, "")
			decl.ID = target
			name := "fetch"
			if !match {
				name = "other"
			}
			retained := &graph.Edge{From: goCaller.ID, To: target, Kind: graph.EdgeCalls, FilePath: goCaller.FilePath}
			uncertain := &graph.Edge{From: caller.ID, To: "unresolved::" + name, Kind: graph.EdgeCalls, FilePath: caller.FilePath}
			facts := []store_pebble.ParseFacts{
				rebuildEventFacts(goCaller.FilePath, goCaller.Language, []*graph.Node{goCaller}, retained),
				rebuildEventFacts(caller.FilePath, caller.Language, []*graph.Node{caller, decl}, uncertain),
			}
			b, sqlite := openRebuildEventStores(t, facts...)
			before := rebuildEventSavedRows(t, b)
			count, err := runRebuildExternalCallSynthesisTest(t.Context(), b, rebuildGRPCReceipts(facts...), true, nil)
			native := *uncertain
			oldTo, changed := New(sqlite).resolveEdge(&native, &ResolveStats{})
			require.Equal(t, match, changed)
			if changed {
				sqlite.ReindexEdges([]graph.EdgeReindex{{Edge: &native, OldTo: oldTo}})
			}
			if match {
				require.ErrorIs(t, err, store_pebble.ErrRebuildExternalCallSynthesis)
				require.Zero(t, count)
				require.Equal(t, before, rebuildEventSavedRows(t, b))
				var bound bool
				for _, e := range sqlite.GetOutEdges(caller.ID) {
					bound = bound || e.To == decl.ID
				}
				require.True(t, bound)
				SynthesizeExternalCalls(sqlite, true)
				require.Equal(t, caller.Language, rebuildRetainedExternalCallRows(t, rebuildEventSQLiteRows(sqlite), goCaller.ID).nodes[0].Language)
				return
			}
			require.NoError(t, err)
			require.EqualValues(t, 1, count)
			SynthesizeExternalCalls(sqlite, true)
			require.Equal(t, rebuildRetainedExternalCallRows(t, rebuildEventSQLiteRows(sqlite), goCaller.ID),
				rebuildRetainedExternalCallRows(t, rebuildEventSavedRows(t, b), goCaller.ID))
		})
	}
}

func TestRebuildExternalCallSkippedSelection(t *testing.T) {
	for _, pass := range []string{SynthRustScope, SynthPascalFormName, SynthGodotConnection} {
		for _, enabled := range []bool{true, false} {
			t.Run(fmt.Sprintf("%s/%v", pass, enabled), func(t *testing.T) {
				const terminal = "external::acme.tools"
				goCaller := rebuildEventNode("z.go", "Run", "go", "")
				retained := &graph.Edge{From: goCaller.ID, To: terminal, Kind: graph.EdgeCalls, FilePath: goCaller.FilePath}
				facts := []store_pebble.ParseFacts{rebuildEventFacts(goCaller.FilePath, goCaller.Language, []*graph.Node{goCaller}, retained)}
				var nativeTurn func(graph.Store) int
				switch pass {
				case SynthRustScope:
					caller := rebuildEventNode("a.rs", "run", "rust", "")
					fn := rebuildEventNode("a.rs", "fetch", "rust", "")
					fn.ID = terminal
					e := &graph.Edge{From: caller.ID, To: "unresolved::fetch", Kind: graph.EdgeCalls, FilePath: caller.FilePath,
						Meta: map[string]any{"rust_path": "crate::fetch"}}
					facts = append(facts, rebuildEventFacts(caller.FilePath, caller.Language, []*graph.Node{caller, fn}, e))
					nativeTurn = ResolveRustScopeCalls
				case SynthPascalFormName:
					unit := &graph.Node{ID: "a.pas", FilePath: "a.pas", Kind: graph.KindFile, Language: "pascal"}
					form := &graph.Node{ID: terminal, FilePath: "a.dfm", Kind: graph.KindFile, Language: "custom"}
					facts = append(facts, rebuildEventFacts(unit.FilePath, unit.Language, []*graph.Node{unit}),
						rebuildEventFacts(form.FilePath, form.Language, []*graph.Node{form}))
					nativeTurn = ResolvePascalForms
				case SynthGodotConnection:
					caller := rebuildEventNode("a.custom", "run", "custom", "")
					const script = "handler.gd"
					n := &graph.Node{ID: script, FilePath: script, Kind: graph.KindFile, Language: "gdscript"}
					method := rebuildEventNode(script, "handle", "gdscript", "")
					method.Kind = graph.KindMethod
					e := &graph.Edge{From: caller.ID, To: terminal, Kind: graph.EdgeReferences, FilePath: caller.FilePath,
						Meta: map[string]any{"godot_connection": true, "godot_method": "handle"}}
					sibling := &graph.Edge{From: caller.ID, To: script, Kind: graph.EdgeReferences, FilePath: caller.FilePath, Line: 1}
					facts = append(facts, rebuildEventFacts(caller.FilePath, caller.Language, []*graph.Node{caller}, e, sibling),
						rebuildEventFacts(script, "gdscript", []*graph.Node{n, method}))
					nativeTurn = ResolveGodotSceneConnections
				}
				b, sqlite := openRebuildEventStores(t, facts...)
				before := rebuildEventSavedRows(t, b)
				var names []string
				if enabled {
					names = append(names, pass)
				}
				requested, err := NewFrameworkSynthesizerSelection(names)
				require.NoError(t, err)
				frozen, err := FreezeRebuildFrameworkSelection(t.Context(), b.RebuildView(), requested)
				require.NoError(t, err)
				require.Equal(t, enabled, frozen.allows(pass))
				count, err := RunRebuildExternalCallSynthesis(t.Context(), b, rebuildGRPCReceipts(facts...), true, requested, frozen, nil)
				if enabled {
					require.Positive(t, nativeTurn(sqlite))
					require.ErrorIs(t, err, store_pebble.ErrRebuildExternalCallSynthesis)
					require.Zero(t, count)
					require.Equal(t, before, rebuildEventSavedRows(t, b))
					return
				}
				require.NoError(t, err)
				require.EqualValues(t, count, SynthesizeExternalCalls(sqlite, true))
				require.Equal(t, rebuildEventSQLiteRows(sqlite), rebuildEventSavedRows(t, b))
			})
		}
	}
}

func TestRebuildExternalCallNpmImporterEligibility(t *testing.T) {
	for _, file := range []string{"host.vue", "recording.TS"} {
		t.Run(file, func(t *testing.T) {
			goCaller := rebuildEventNode("z.go", "Run", "go", "")
			caller := rebuildEventNode("a.custom", "run", "custom", "")
			retained := &graph.Edge{From: goCaller.ID, To: "external::acme.tools", Kind: graph.EdgeCalls, FilePath: goCaller.FilePath}
			uncertain := &graph.Edge{From: caller.ID, To: "unresolved::import::unrelated", Kind: graph.EdgeReferences, FilePath: file}
			facts := []store_pebble.ParseFacts{
				rebuildEventFacts(goCaller.FilePath, goCaller.Language, []*graph.Node{goCaller}, retained),
				rebuildEventFacts(caller.FilePath, caller.Language, []*graph.Node{caller}, uncertain),
			}
			b, _ := openRebuildEventStores(t, facts...)
			count, err := runRebuildExternalCallSynthesisTest(t.Context(), b, rebuildGRPCReceipts(facts...), true,
				func(string, string) (string, error) {
					t.Fatal("native npm lookup does not accept this path")
					return "", nil
				})
			require.NoError(t, err)
			require.EqualValues(t, 1, count)
		})
	}
}

func TestRebuildExternalCallStubRepoPrefix(t *testing.T) {
	caller := rebuildEventNode("src/a.py", "run", "python", "")
	e := &graph.Edge{From: caller.ID, To: "unresolved::extern::thirdparty::run", Kind: graph.EdgeCalls, FilePath: caller.FilePath}
	for _, repo := range []string{"", "repo"} {
		caller.RepoPrefix = repo
		votes := maybeRebuildExternalCall(e, caller, "python")
		require.Len(t, votes, 1)
		require.Equal(t, graph.StubID(repo, graph.StubKindStdlib, "thirdparty", "run"), votes[0].OldTo)
	}
}

func TestRebuildExternalCallCSharpReceiverProof(t *testing.T) {
	for _, related := range []bool{false, true} {
		t.Run(fmt.Sprint(related), func(t *testing.T) {
			const terminal = "external::acme.tools"
			caller := rebuildEventNode("a.cs", "run", "csharp", "")
			method := rebuildEventNode("a.cs", "fetch", "csharp", "")
			method.ID, method.Kind = terminal, graph.KindMethod
			method.Meta = map[string]any{"receiver": "Owner"}
			receiver := rebuildEventNode("a.cs", "Receiver", "csharp", "")
			owner := rebuildEventNode("a.cs", "Owner", "csharp", "")
			receiver.Kind, owner.Kind = graph.KindType, graph.KindType
			cs := &graph.Edge{From: caller.ID, To: terminal, Kind: graph.EdgeCalls, FilePath: caller.FilePath,
				Origin: graph.OriginASTInferred, Meta: map[string]any{"receiver_type": receiver.Name}}
			goCaller := rebuildEventNode("z.go", "Run", "go", "")
			goEdge := &graph.Edge{From: goCaller.ID, To: terminal, Kind: graph.EdgeCalls, FilePath: goCaller.FilePath}
			csFacts := rebuildEventFacts(caller.FilePath, caller.Language, []*graph.Node{caller, method, receiver, owner}, cs)
			if related {
				csFacts.Edges = append(csFacts.Edges, &graph.Edge{From: receiver.ID, To: owner.ID, Kind: graph.EdgeExtends, FilePath: caller.FilePath})
			}
			facts := []store_pebble.ParseFacts{csFacts, rebuildEventFacts(goCaller.FilePath, goCaller.Language, []*graph.Node{goCaller}, goEdge)}
			b, sqlite := openRebuildEventStores(t, facts...)
			changed, err := rebuildExternalCSharpDemotion(t.Context(), b.RebuildView(), cs, caller)
			require.NoError(t, err)
			require.Equal(t, !related, changed)
			want := 0
			if !related {
				want = 1
			}
			require.Equal(t, want, demoteCSharpMisattributedMemberCalls(sqlite))
		})
	}
}

func TestRebuildExternalCallIndexedReceiptReplay(t *testing.T) {
	for _, deferred := range []bool{false, true} {
		t.Run(fmt.Sprint(deferred), func(t *testing.T) {
			caller := rebuildEventNode("z.go", "run", "go", "")
			nodes := []*graph.Node{caller}
			for i := 0; i < 8; i++ {
				n := rebuildEventNode(caller.FilePath, fmt.Sprint(i), "go", "")
				n.ID = fmt.Sprintf("dep::acme.tools::fn%d", i)
				nodes = append(nodes, n)
			}
			facts := []store_pebble.ParseFacts{rebuildEventFacts(caller.FilePath, caller.Language, nodes)}
			if deferred {
				n := rebuildEventNode("a.custom", "run", "custom", "")
				e := &graph.Edge{From: n.ID, To: "unresolved::none", Kind: graph.EdgeCalls, FilePath: n.FilePath}
				facts = append(facts, rebuildEventFacts(n.FilePath, n.Language, []*graph.Node{n}, e))
			}
			b, _ := openRebuildEventStores(t, facts...)
			passes := 0
			receipts := func(yield func(store_pebble.ParseFacts, error) bool) {
				passes++
				rebuildGRPCReceipts(facts...)(yield)
			}
			count, err := runRebuildExternalCallSynthesisTest(t.Context(), b, receipts, true, nil)
			require.NoError(t, err)
			require.Zero(t, count)
			require.Equal(t, 1, passes, "indexed lookup shares the receipt proof traversal")
		})
	}
}

func TestRebuildExternalCallPythonImportedName(t *testing.T) {
	for _, file := range []string{"acme.py", "other.py"} {
		t.Run(file, func(t *testing.T) {
			const terminal = "external::shared.tools"
			goCaller := rebuildEventNode("z.go", "run", "go", "")
			caller := rebuildEventNode("a.py", "run", "python", "")
			decl := rebuildEventNode(file, "tools", "python", "")
			decl.ID = terminal
			goEdge := &graph.Edge{From: goCaller.ID, To: terminal, Kind: graph.EdgeCalls, FilePath: goCaller.FilePath}
			pyEdge := &graph.Edge{From: caller.ID, To: "unresolved::extern::acme.tools::fetch", Kind: graph.EdgeCalls, FilePath: caller.FilePath}
			facts := []store_pebble.ParseFacts{
				rebuildEventFacts(goCaller.FilePath, goCaller.Language, []*graph.Node{goCaller}, goEdge),
				rebuildEventFacts(caller.FilePath, caller.Language, []*graph.Node{caller}, pyEdge),
				rebuildEventFacts(decl.FilePath, decl.Language, []*graph.Node{decl}),
			}
			b, native := openRebuildEventStores(t, facts...)
			before := rebuildEventSavedRows(t, b)
			count, err := runRebuildExternalCallSynthesisTest(t.Context(), b, rebuildGRPCReceipts(facts...), true, nil)
			bound := *pyEdge
			old, changed := New(native).resolveEdge(&bound, &ResolveStats{})
			require.True(t, changed)
			native.ReindexEdges([]graph.EdgeReindex{{Edge: &bound, OldTo: old}})
			SynthesizeExternalCalls(native, true)
			want := rebuildRetainedExternalCallRows(t, rebuildEventSQLiteRows(native), goCaller.ID)
			if file == "acme.py" {
				require.Equal(t, terminal, bound.To)
				require.Equal(t, "python", want.nodes[0].Language)
				require.ErrorIs(t, err, store_pebble.ErrRebuildExternalCallSynthesis)
				require.Zero(t, count)
				require.Equal(t, before, rebuildEventSavedRows(t, b))
				return
			}
			require.NoError(t, err)
			require.EqualValues(t, 1, count)
			require.Equal(t, want, rebuildRetainedExternalCallRows(t, rebuildEventSavedRows(t, b), goCaller.ID))
		})
	}
}

// A lookup that can match must survive both projection and the store's
// name, qualified-name or file-directory probe.
func requireRebuildExternalIndexedCoverage(t *testing.T, e *graph.Edge, n *graph.Node) {
	t.Helper()
	if !rebuildExternalIndexedLookup(e, n) {
		return
	}
	projected, names, quals, directory := rebuildExternalIndexedEdge(e)
	require.NotNil(t, projected)
	require.True(t, rebuildExternalIndexedLookup(projected, n), "projection lost lookup evidence")
	require.True(t, slices.Contains(names, n.Name) || slices.Contains(quals, n.QualName) || directory && n.Kind == graph.KindFile,
		"lookup match missing from indexed probes: edge %s, node %s", e.To, n.ID)
}

func TestRebuildExternalCallReadPromotion(t *testing.T) {
	for _, tc := range []struct {
		kind graph.NodeKind
		to   string
		want bool
	}{
		{graph.KindFunction, "unresolved::fetch", true},
		{graph.KindMethod, "unresolved::*.fetch", true},
		{graph.KindVariable, "unresolved::import::acme.tools", false},
		{graph.KindConstant, "unresolved::import::acme.tools", false},
		{graph.KindType, "unresolved::extern::acme.tools::fetch", false},
		{graph.KindFunction, "unresolved::extern::acme.tools::fetch", false},
	} {
		t.Run(string(tc.kind)+tc.to, func(t *testing.T) {
			n := &graph.Node{ID: "external::acme.tools", Kind: tc.kind, Name: "fetch", QualName: "acme.tools"}
			e := &graph.Edge{From: "a.custom::run", Kind: graph.EdgeReads, To: tc.to, FilePath: "a.custom"}
			require.Equal(t, tc.want, rebuildExternalIndexedLookup(e, n))
			requireRebuildExternalIndexedCoverage(t, e, n)
		})
	}
}

type rebuildTestRoleString string

func TestRebuildExternalCallSkippedInputCaps(t *testing.T) {
	padding := strings.Repeat("x", 16<<20)
	for _, tc := range []struct {
		name  string
		lang  string
		kind  graph.EdgeKind
		role  map[string]any
		voter bool
	}{
		{name: "non-Rust Call", lang: "go", kind: graph.EdgeCalls, role: map[string]any{"rust_path": "crate::fetch"}},
		{name: "untagged Rust Call", lang: "rust", kind: graph.EdgeCalls},
		{name: "Rust voter", lang: "rust", kind: graph.EdgeCalls, role: map[string]any{"rust_path": "crate::fetch"}, voter: true},
		{name: "Rust JSON role", lang: "rust", kind: graph.EdgeCalls, role: map[string]any{"rust_path": rebuildTestRoleString("crate::fetch")}, voter: true},
		{name: "ordinary Reference", lang: "custom", kind: graph.EdgeReferences},
		{name: "Godot voter", lang: "custom", kind: graph.EdgeReferences, role: map[string]any{"godot_connection": true, "godot_method": "handle"}, voter: true},
		{name: "Godot empty method", lang: "custom", kind: graph.EdgeReferences, role: map[string]any{"godot_connection": true, "godot_method": ""}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			caller := rebuildEventNode("a.source", "run", tc.lang, "")
			rust := rebuildEventNode("a.rs", "fetch", "rust", "")
			rust.ID = "external::rust.tools"
			godot := rebuildEventNode("script.gd", "handle", "gdscript", "")
			godot.ID = "external::godot.tools"
			facts := []store_pebble.ParseFacts{
				rebuildEventFacts(caller.FilePath, caller.Language, []*graph.Node{caller}),
				rebuildEventFacts(rust.FilePath, rust.Language, []*graph.Node{rust}),
				rebuildEventFacts(godot.FilePath, godot.Language, []*graph.Node{godot}),
			}
			b := openRebuildGRPCBuild(t, facts...)
			meta := map[string]any{"padding": padding}
			for k, v := range tc.role {
				meta[k] = v
			}
			b.AddEdge(&graph.Edge{From: caller.ID, To: "unresolved::fetch", Kind: tc.kind, FilePath: caller.FilePath, Meta: meta})
			selection, err := NewFrameworkSynthesizerSelection([]string{SynthGodotConnection, SynthRustScope})
			require.NoError(t, err)
			votes := 0
			err = visitRebuildSkippedExternalCalls(t.Context(), b.RebuildView(), selection,
				func(*graph.Edge, store_pebble.RebuildExternalCallCandidate) error {
					votes++
					return nil
				})
			if tc.voter {
				require.ErrorIs(t, err, store_pebble.ErrRebuildLinkInputSize)
			} else {
				require.NoError(t, err)
			}
			require.Zero(t, votes)
		})
	}
}

func TestRebuildExternalCallReadNonCallable(t *testing.T) {
	for _, kind := range []graph.NodeKind{graph.KindVariable, graph.KindConstant} {
		t.Run(string(kind), func(t *testing.T) {
			const file = "external::acme.tools"
			n := &graph.Node{ID: file + "::fetch", Kind: kind, Name: "fetch", FilePath: file}
			e := &graph.Edge{From: "a.ts::run", Kind: graph.EdgeReads, To: "unresolved::fetch", FilePath: "a.ts"}
			require.False(t, rebuildExternalIndexedLookup(e, n))
			e.Kind = graph.EdgeCalls
			require.True(t, rebuildExternalIndexedLookup(e, n), "the value-callee role is otherwise eligible")
			requireRebuildExternalIndexedCoverage(t, e, n)
		})
	}
	t.Run("PHP type", func(t *testing.T) {
		n := &graph.Node{ID: "external::acme.tools", Kind: graph.KindType, Language: "php", Name: "Model"}
		e := &graph.Edge{From: "a.php::run", Kind: graph.EdgeReads, To: "unresolved::Alias", FilePath: "a.php",
			Meta: map[string]any{"fqn": `App\Model`}}
		require.False(t, rebuildExternalIndexedLookup(e, n))
		e.Kind = graph.EdgeReferences
		require.True(t, rebuildExternalIndexedLookup(e, n), "the PHP name role is otherwise eligible")
		requireRebuildExternalIndexedCoverage(t, e, n)
	})
}

func TestRebuildExternalCallPHPIndexedMetadata(t *testing.T) {
	for _, key := range []string{"fqn", "target_fqn", "eloquent_method"} {
		for _, match := range []bool{true, false} {
			t.Run(fmt.Sprintf("%s/%v", key, match), func(t *testing.T) {
				const terminal = "external::acme.tools"
				goCaller := rebuildEventNode("z.go", "run", "go", "")
				caller := rebuildEventNode("a.php", "run", "php", "")
				decl := rebuildEventNode(caller.FilePath, "Model", "php", "")
				decl.ID, decl.Kind = terminal, graph.KindType
				decl.Meta = map[string]any{MetaScopeNamespace: "App"}
				claim := &graph.Edge{From: caller.ID, Kind: graph.EdgeReferences, To: "unresolved::Alias", FilePath: caller.FilePath}
				leaf := "Model"
				if !match {
					leaf = "Other"
				}
				claim.Meta = map[string]any{key: `App\` + leaf}
				nodes := []*graph.Node{caller, decl}
				edges := []*graph.Edge{claim}
				switch key {
				case "fqn":
					claim.To = "unresolved::import::Alias"
				case "eloquent_method":
					owner := rebuildEventNode(caller.FilePath, "Owner", "php", "")
					owner.Kind = graph.KindType
					owner.Meta = map[string]any{MetaScopeNamespace: "App"}
					decl.Kind = graph.KindMethod
					decl.Meta = map[string]any{"receiver": "Owner"}
					claim.Kind = graph.EdgeCalls
					claim.Meta = map[string]any{"eloquent_method": leaf, "eloquent_model": `App\Owner`}
					companion := &graph.Edge{From: caller.ID, To: owner.ID, Kind: graph.EdgeReferences, FilePath: caller.FilePath,
						Meta: map[string]any{"ref_context": graph.RefContextStaticAccess, "target_fqn": `App\Owner`}}
					nodes = append(nodes, owner)
					edges = append(edges, companion)
				}
				require.Equal(t, match, rebuildExternalIndexedLookup(claim, decl))
				requireRebuildExternalIndexedCoverage(t, claim, decl)
				retained := &graph.Edge{From: goCaller.ID, Kind: graph.EdgeCalls, To: terminal, FilePath: goCaller.FilePath}
				facts := []store_pebble.ParseFacts{
					rebuildEventFacts(goCaller.FilePath, goCaller.Language, []*graph.Node{goCaller}, retained),
					rebuildEventFacts(caller.FilePath, caller.Language, nodes, edges...),
				}
				b, sqlite := openRebuildEventStores(t, facts...)
				before := rebuildEventSavedRows(t, b)
				count, err := runRebuildExternalCallSynthesisTest(t.Context(), b, rebuildGRPCReceipts(facts...), true, nil)
				native := *claim
				oldTo, changed := New(sqlite).resolveEdge(&native, &ResolveStats{})
				if changed {
					sqlite.ReindexEdges([]graph.EdgeReindex{{Edge: &native, OldTo: oldTo}})
				}
				require.Equal(t, match, native.To == terminal)
				SynthesizeExternalCalls(sqlite, true)
				want := rebuildRetainedExternalCallRows(t, rebuildEventSQLiteRows(sqlite), goCaller.ID)
				if match {
					require.Equal(t, "php", want.nodes[0].Language)
					require.ErrorIs(t, err, store_pebble.ErrRebuildExternalCallSynthesis)
					require.Zero(t, count)
					require.Equal(t, before, rebuildEventSavedRows(t, b))
					return
				}
				require.NoError(t, err)
				require.EqualValues(t, 1, count)
				require.Equal(t, want, rebuildRetainedExternalCallRows(t, rebuildEventSavedRows(t, b), goCaller.ID))
			})
		}
	}
}

func TestRebuildExternalCallIndexedKeys(t *testing.T) {
	for _, tc := range []struct {
		name      string
		file      string
		target    string
		names     []string
		quals     []string
		directory bool
	}{
		{name: "bare", target: "fetch", names: []string{"fetch"}},
		{name: "selector", target: "*.fetch", names: []string{"fetch"}},
		{name: "explicit symbol", target: "extern::acme.tools::fetch", names: []string{"fetch"}},
		{name: "Python imported attribute", file: "a.py", target: "extern::acme.tools::fetch", names: []string{"fetch", "tools"}},
		{name: "import declaration and directory", target: "import::acme.tools::fetch", names: []string{"fetch"},
			quals: []string{"acme.tools::fetch", "acme.tools"}, directory: true},
		{name: "empty import spec", target: "import::", quals: []string{"", ""}, directory: true},
		{name: "empty extern leaf", target: "extern::acme.tools::", names: []string{""}},
		{name: "Lua-owned import", file: "a.lua", target: "import::acme.tools::fetch"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			raw := &graph.Edge{From: "source", Kind: graph.EdgeCalls, To: "unresolved::" + tc.target, FilePath: tc.file}
			edge, names, quals, directory := rebuildExternalIndexedEdge(raw)
			require.Equal(t, raw, edge)
			require.Equal(t, tc.names, names)
			require.Equal(t, tc.quals, quals)
			require.Equal(t, tc.directory, directory)
		})
	}
}

func TestRebuildExternalCallIndexedCoverage(t *testing.T) {
	for _, tc := range []struct {
		name, file, target string
		node               graph.Node
	}{
		{"bare", "a.py", "fetch", graph.Node{Kind: graph.KindFunction, Name: "fetch"}},
		{"selector", "a.py", "*.fetch", graph.Node{Kind: graph.KindMethod, Name: "fetch"}},
		{"extern leaf", "a.py", "extern::acme.tools::fetch", graph.Node{Kind: graph.KindType, Name: "fetch"}},
		{"empty extern leaf", "a.py", "extern::acme.tools::", graph.Node{Kind: graph.KindFunction}},
		{"Python imported attribute", "a.py", "extern::acme.tools::alias", graph.Node{Kind: graph.KindFunction, Name: "tools", FilePath: "acme.py"}},
		{"import full qualname", "a.py", "import::acme.tools::fetch", graph.Node{Kind: graph.KindType, Name: "other", QualName: "acme.tools::fetch"}},
		{"import path qualname", "a.py", "import::acme.tools::fetch", graph.Node{Kind: graph.KindType, Name: "other", QualName: "acme.tools"}},
		{"import leaf", "a.py", "import::acme.tools::fetch", graph.Node{Kind: graph.KindType, Name: "fetch"}},
		{"import directory", "a.py", "import::acme/tools::fetch", graph.Node{Kind: graph.KindFile, Name: "other.py", FilePath: "acme/tools/other.py"}},
		{"empty import qualname", "a.py", "import::", graph.Node{Kind: graph.KindType, Name: "other"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tc.node.ID = "external::fixture.tools"
			e := &graph.Edge{From: "a.py::run", Kind: graph.EdgeCalls, To: "unresolved::" + tc.target, FilePath: tc.file}
			require.True(t, rebuildExternalIndexedLookup(e, &tc.node), "fixture must exercise a lookup match")
			requireRebuildExternalIndexedCoverage(t, e, &tc.node)
		})
	}
}
