package resolver

import (
	"fmt"
	"slices"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/graph/store_pebble"
	"github.com/zzet/gortex/internal/parser/languages"
)

func TestRebuildMacroExpansionSQLiteOracle(t *testing.T) {
	const file = "main.c"
	const caller, handler = file + "::run", file + "::handler"
	const first, occupied, second, saved = 6, 7, 8, 9
	ctx := t.Context()
	facts := []store_pebble.ParseFacts{
		rebuildMacroExpansionFacts(t, file, `int handler(int x) { return x + 1; }
int left(int x) { return x + 2; }
#define APPLY(x) handler(x)
#define CHOOSE(x) left(x)
int run(int x) {
    x = APPLY(x);
    x = APPLY(x) + handler(x);
    x = APPLY(x);
    x = APPLY(x);
    return CHOOSE(x);
}
`),
		rebuildMacroExpansionFacts(t, "other.c", `int right(int x) { return x + 3; }
#define CHOOSE(x) right(x)
`),
	}
	b, native := openRebuildEventStores(t, facts...)
	// Model the preceding call linker on both stores. The private move
	// keeps the original extractor receipt for the body and direct call.
	for _, site := range []struct {
		from string
		line int
	}{
		{file + "::APPLY", 3},
		{caller, occupied},
	} {
		i := slices.IndexFunc(facts[0].Edges, func(e *graph.Edge) bool {
			return e.Kind == graph.EdgeCalls && e.From == site.from && e.To == "unresolved::handler" && e.Line == site.line
		})
		require.GreaterOrEqual(t, i, 0, "missing extracted handler call from %s", site.from)
		linked := *facts[0].Edges[i]
		linked.To = handler
		if site.line == occupied {
			linked.Origin = graph.OriginLSPResolved
			linked.Confidence = 1
			linked.ConfidenceLabel = graph.ConfidenceLabelFor(graph.EdgeCalls, 1)
			linked.Meta = map[string]any{"witness": "ordinary handler call"}
		}
		moves := []graph.EdgeReindex{{Edge: &linked, OldTo: "unresolved::handler"}}
		require.True(t, native.ReindexEdgesApplied(moves))
		require.NoError(t, b.ApplyLinkReindexes(ctx, moves))
	}
	// SQLite keeps even a macro-owned incumbent when AddEdge hits its key.
	incumbent := &graph.Edge{From: caller, To: handler, Kind: graph.EdgeCalls, FilePath: file, Line: saved,
		Origin: graph.OriginASTInferred, Meta: map[string]any{"via": "macro_expansion", "witness": "saved macro call"}}
	require.NoError(t, native.AddBatchChecked(nil, []*graph.Edge{incumbent}))
	b.AddEdge(incumbent)

	before := rebuildEventSQLiteRows(native)
	require.Equal(t, before, rebuildEventSavedRows(t, b), "same complete graph before Macro expansion")
	selection, err := NewFrameworkSynthesizerSelection([]string{SynthMacroExpansion})
	require.NoError(t, err)
	frozen, err := FreezeRebuildFrameworkSelection(ctx, b.RebuildView(), selection)
	require.NoError(t, err)
	require.True(t, frozen.allows(SynthMacroExpansion))
	require.NoError(t, FreezeRebuildMacroExpansionInputs(ctx, b, frozen))

	report := RunFrameworkSynthesizersWithSelection(native, selection)
	require.NoError(t, report.Err)
	require.False(t, report.Incomplete)
	require.Equal(t, 3, report.Total, "two new calls and one already macro-owned")
	want := rebuildEventSQLiteRows(native)
	expected := rebuildEventRows{nodes: before.nodes, edges: slices.Clone(before.edges)}
	for _, line := range []int{first, second} {
		expected.edges = append(expected.edges, &graph.Edge{
			From:            caller,
			To:              handler,
			Kind:            graph.EdgeCalls,
			FilePath:        file,
			Line:            line,
			Confidence:      ConfidenceHeuristic,
			ConfidenceLabel: graph.ConfidenceLabelFor(graph.EdgeCalls, ConfidenceHeuristic),
			Origin:          graph.OriginASTInferred,
			Meta: map[string]any{
				"via":             "macro_expansion",
				"macro":           "APPLY",
				MetaSynthesizedBy: SynthMacroExpansion,
				MetaProvenance:    ProvenanceHeuristic,
			},
		})
	}
	expected.sort()
	require.Equal(t, expected, want, "only the fresh APPLY uses add calls; CHOOSE is ambiguous")

	missing := slices.IndexFunc(facts[0].Edges, func(e *graph.Edge) bool {
		return e.Kind == graph.EdgeCalls && e.From == caller && e.To == "unresolved::APPLY" && e.Line == second
	})
	require.GreaterOrEqual(t, missing, 0, "missing extracted APPLY use")
	incomplete := facts[0]
	incomplete.Edges = slices.Delete(slices.Clone(incomplete.Edges), missing, missing+1)
	_, err = RunRebuildMacroExpansion(ctx, b, rebuildGRPCReceipts(incomplete, facts[1]), frozen)
	require.ErrorIs(t, err, store_pebble.ErrRebuildUnknownProvenance)
	require.Equal(t, before, rebuildEventSavedRows(t, b), "missing proof for the later use must leave even the first use unwritten")

	require.NoError(t, FreezeRebuildMacroExpansionInputs(ctx, b, frozen))
	_, err = RunRebuildMacroExpansion(ctx, b, rebuildGRPCReceipts(facts...), frozen)
	require.NoError(t, err)
	require.Equal(t, want, rebuildEventSavedRows(t, b), "after one Macro expansion turn")
}

func rebuildMacroExpansionFacts(t *testing.T, file, source string) store_pebble.ParseFacts {
	t.Helper()
	parsed, err := languages.NewCExtractor().Extract(file, []byte(source))
	require.NoError(t, err)
	t.Cleanup(parsed.ReleaseTree)
	return roundTripRebuildRNBridgeFacts(t, rebuildEventFacts(file, "c", parsed.Nodes, parsed.Edges...))
}

func TestRebuildMacroExpansionReceiptEntrant(t *testing.T) {
	const file, lang = "use.c", "c"
	caller := rebuildEventNode(file, "run", lang, "")
	callee := rebuildEventNode(file, "work", lang, "")
	first := &graph.Node{ID: file + "::APPLY", Name: "APPLY", Kind: graph.KindMacro, FilePath: file, Language: lang,
		Meta: map[string]any{"macro_kind": macroFunctionKindMeta}}
	other := &graph.Node{ID: file + "::other", Name: first.Name, Kind: graph.KindMacro, FilePath: file, Language: lang,
		Meta: map[string]any{"macro_kind": "object"}}
	body := &graph.Edge{From: first.ID, To: callee.ID, Kind: graph.EdgeCalls, FilePath: file, Line: 1}
	otherBody := &graph.Edge{From: other.ID, To: callee.ID, Kind: graph.EdgeCalls, FilePath: file, Line: 2}
	use := &graph.Edge{From: caller.ID, To: "unresolved::APPLY", Kind: graph.EdgeCalls, FilePath: file, Line: 3}
	facts := rebuildEventFacts(file, lang, []*graph.Node{caller, callee, first, other}, body, otherBody, use)
	selection, err := NewFrameworkSynthesizerSelection([]string{SynthMacroExpansion})
	require.NoError(t, err)
	for _, active := range []bool{false, true} {
		b, native := openRebuildEventStores(t, facts)
		before := rebuildEventSavedRows(t, b)
		frozen, err := FreezeRebuildFrameworkSelection(t.Context(), b.RebuildView(), selection)
		require.NoError(t, err)
		require.NoError(t, FreezeRebuildMacroExpansionInputs(t.Context(), b, frozen))
		raw := facts
		raw.Nodes = slices.Clone(facts.Nodes)
		if active {
			entrant := *other
			entrant.Language = "cpp"
			entrant.Meta = map[string]any{"macro_kind": macroFunctionKindMeta}
			raw.Nodes[3] = &entrant
		}
		stats, err := RunRebuildMacroExpansion(t.Context(), b, rebuildGRPCReceipts(raw), frozen)
		if active {
			var refused *store_pebble.RebuildUnknownProvenanceError
			require.ErrorAs(t, err, &refused)
			require.EqualValues(t, 1, refused.Count)
			want := graph.EdgeIdentityFor(use)
			want.To = callee.ID
			require.Equal(t, want, refused.First)
			require.Zero(t, stats)
			require.Equal(t, before, rebuildEventSavedRows(t, b))
			continue
		}
		require.NoError(t, err)
		require.EqualValues(t, 1, stats.Adds)
		report := RunFrameworkSynthesizersWithSelection(native, selection)
		require.NoError(t, report.Err)
		require.Equal(t, rebuildEventSQLiteRows(native), rebuildEventSavedRows(t, b))
	}
}

func TestRebuildMacroExpansionCountsSkippedUseClaim(t *testing.T) {
	const file, lang = "use.c", "c"
	caller := rebuildEventNode(file, "run", lang, "")
	caller.RepoPrefix = "other"
	callee := rebuildEventNode(file, "work", lang, "")
	macro := &graph.Node{ID: file + "::APPLY", Name: "APPLY", Kind: graph.KindMacro, FilePath: file, Language: lang,
		Meta: map[string]any{"macro_kind": macroFunctionKindMeta}}
	body := &graph.Edge{From: macro.ID, To: callee.ID, Kind: graph.EdgeCalls, FilePath: file, Line: 1}
	use := &graph.Edge{From: caller.ID, To: "unresolved::APPLY", Kind: graph.EdgeCalls, FilePath: file, Line: 2}
	facts := rebuildEventFacts(file, lang, []*graph.Node{caller, callee, macro}, body, use)
	selection, err := NewFrameworkSynthesizerSelection([]string{SynthMacroExpansion})
	require.NoError(t, err)
	for _, duplicate := range []bool{false, true} {
		ctx := t.Context()
		b := openRebuildGRPCBuild(t, facts)
		derived := *use
		derived.To = macro.ID
		require.NoError(t, b.ApplyLinkReindexes(ctx, []graph.EdgeReindex{{Edge: &derived, OldTo: use.To}}))
		if duplicate {
			// The old key cannot bind in the caller's repo, but it still
			// claims the raw receipt used by the directly bound key.
			b.AddEdge(use)
		}
		before := rebuildEventSavedRows(t, b)
		frozen, err := FreezeRebuildFrameworkSelection(ctx, b.RebuildView(), selection)
		require.NoError(t, err)
		require.NoError(t, FreezeRebuildMacroExpansionInputs(ctx, b, frozen))
		stats, err := RunRebuildMacroExpansion(ctx, b, rebuildGRPCReceipts(facts), frozen)
		if duplicate {
			require.ErrorIs(t, err, store_pebble.ErrRebuildUnknownProvenance)
			require.Zero(t, stats)
			require.Equal(t, before, rebuildEventSavedRows(t, b))
			continue
		}
		require.NoError(t, err)
		require.EqualValues(t, 1, stats.Adds)
		rows := rebuildEventSavedRows(t, b)
		require.Contains(t, rows.edges, macroExpansionEdge(caller.ID, callee.ID, file, 2, macro.Name))
	}
}

func TestRebuildMacroExpansionProvesAmbiguity(t *testing.T) {
	const file, lang = "cohort.c", "c"
	caller := rebuildEventNode(file, "run", lang, "")
	kept := rebuildEventNode(file, "kept", lang, "")
	omitted := rebuildEventNode(file, "omitted", lang, "")
	control := rebuildEventNode(file, "control", lang, "")
	macro := func(id, name string) *graph.Node {
		return &graph.Node{ID: file + "::" + id, Name: name, Kind: graph.KindMacro, FilePath: file, Language: lang,
			Meta: map[string]any{"macro_kind": macroFunctionKindMeta}}
	}
	first, second, third, unique := macro("first", "APPLY"), macro("second", "APPLY"), macro("third", "APPLY"), macro("unique", "UNIQUE")
	call := func(from, to string, line int) *graph.Edge {
		return &graph.Edge{From: from, To: to, Kind: graph.EdgeCalls, FilePath: file, Line: line}
	}
	firstBody, secondBody := call(first.ID, kept.ID, 1), call(second.ID, omitted.ID, 2)
	thirdBody, uniqueBody := call(third.ID, omitted.ID, 3), call(unique.ID, control.ID, 4)
	use := call(caller.ID, "unresolved::APPLY", 5)
	controlUse := call(caller.ID, "unresolved::UNIQUE", 6)
	add := macroExpansionEdge(caller.ID, kept.ID, file, use.Line, first.Name)
	selection, err := NewFrameworkSynthesizerSelection([]string{SynthMacroExpansion})
	require.NoError(t, err)
	for _, tc := range []struct {
		name     string
		omitNode *graph.Node
		omitBody *graph.Edge
		third    bool
		occupy   bool
		direct   bool
		refuse   bool
	}{
		{name: "missing declaration", omitNode: second, refuse: true},
		{name: "missing body", omitBody: secondBody, refuse: true},
		{name: "complete ambiguity"},
		{name: "two independent proofs", omitNode: third, omitBody: thirdBody, third: true},
		{name: "sole possible output has incumbent", omitNode: second, occupy: true},
		{name: "direct use cannot select a different member", omitNode: second, direct: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			nodes := []*graph.Node{caller, kept, omitted, control, first, second, unique}
			site := *use
			if tc.direct {
				site.To = second.ID
			}
			edges := []*graph.Edge{firstBody, secondBody, uniqueBody, &site, controlUse}
			if tc.third {
				nodes = append(nodes, third)
				edges = append(edges, thirdBody)
			}
			if tc.occupy {
				edges = append(edges, call(caller.ID, kept.ID, use.Line))
			}
			facts := rebuildEventFacts(file, lang, nodes, edges...)
			receipt := rebuildEventFacts(file, lang,
				slices.DeleteFunc(slices.Clone(nodes), func(n *graph.Node) bool { return n == tc.omitNode }),
				slices.DeleteFunc(slices.Clone(edges), func(e *graph.Edge) bool { return e == tc.omitBody })...)
			b := openRebuildGRPCBuild(t, facts)
			before := rebuildEventSavedRows(t, b)
			oracle := facts
			if tc.refuse {
				oracle = receipt
			}
			native := rebuildGRPCNative(t, oracle)
			report := RunFrameworkSynthesizersWithSelection(native, selection)
			require.NoError(t, report.Err)
			want := rebuildEventSQLiteRows(native)
			if tc.refuse {
				require.Contains(t, want.edges, add, "native receipts expose the suppressed add")
			}
			frozen, err := FreezeRebuildFrameworkSelection(t.Context(), b.RebuildView(), selection)
			require.NoError(t, err)
			require.NoError(t, FreezeRebuildMacroExpansionInputs(t.Context(), b, frozen))
			stats, err := RunRebuildMacroExpansion(t.Context(), b, rebuildGRPCReceipts(receipt), frozen)
			if tc.refuse {
				var refused *store_pebble.RebuildUnknownProvenanceError
				require.ErrorAs(t, err, &refused)
				require.Equal(t, &store_pebble.RebuildUnknownProvenanceError{Count: 1, First: graph.EdgeIdentityFor(add)}, refused)
				require.Zero(t, stats)
				require.Equal(t, before, rebuildEventSavedRows(t, b))
				return
			}
			require.NoError(t, err)
			require.EqualValues(t, 1, stats.Adds)
			require.Equal(t, want, rebuildEventSavedRows(t, b))
		})
	}
}

func TestRebuildMacroExpansionProvesFirstAndLastProposal(t *testing.T) {
	const file, lang = "proposals.c", "c"
	caller := rebuildEventNode(file, "run", lang, "")
	callee := rebuildEventNode(file, "work", lang, "")
	nodes := []*graph.Node{caller, callee}
	var edges, uses []*graph.Edge
	for _, name := range []string{"FIRST", "MIDDLE", "WINNER"} {
		n := &graph.Node{ID: file + "::" + name, Name: name, Kind: graph.KindMacro, FilePath: file, Language: lang,
			Meta: map[string]any{"macro_kind": macroFunctionKindMeta}}
		nodes = append(nodes, n)
		body := &graph.Edge{From: n.ID, To: callee.ID, Kind: graph.EdgeCalls, FilePath: file, Line: 1}
		use := &graph.Edge{From: caller.ID, To: n.ID, Kind: graph.EdgeCalls, FilePath: file, Line: 5}
		edges = append(edges, body, use)
		uses = append(uses, use)
	}
	facts := rebuildEventFacts(file, lang, nodes, edges...)
	selection, err := NewFrameworkSynthesizerSelection([]string{SynthMacroExpansion})
	require.NoError(t, err)
	add := macroExpansionEdge(caller.ID, callee.ID, file, 5, "WINNER")
	for _, tc := range []struct {
		name   string
		omit   []*graph.Edge
		refuse bool
	}{
		{"middle proposal", []*graph.Edge{uses[1]}, false},
		{"first proposal", []*graph.Edge{uses[0]}, true},
		{"last proposal", []*graph.Edge{uses[2]}, true},
		{"both deciding proposals", []*graph.Edge{uses[0], uses[2]}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			b, native := openRebuildEventStores(t, facts)
			before := rebuildEventSavedRows(t, b)
			frozen, err := FreezeRebuildFrameworkSelection(t.Context(), b.RebuildView(), selection)
			require.NoError(t, err)
			require.NoError(t, FreezeRebuildMacroExpansionInputs(t.Context(), b, frozen))
			receipt := facts
			receipt.Edges = slices.DeleteFunc(slices.Clone(edges), func(e *graph.Edge) bool { return slices.Contains(tc.omit, e) })
			stats, err := RunRebuildMacroExpansion(t.Context(), b, rebuildGRPCReceipts(receipt), frozen)
			if tc.refuse {
				var refused *store_pebble.RebuildUnknownProvenanceError
				require.ErrorAs(t, err, &refused)
				require.Equal(t, &store_pebble.RebuildUnknownProvenanceError{Count: 1, First: graph.EdgeIdentityFor(add)}, refused)
				require.Zero(t, stats)
				require.Equal(t, before, rebuildEventSavedRows(t, b))
				return
			}
			require.NoError(t, err)
			require.Equal(t, store_pebble.RebuildMacroExpansionStats{Sites: 3, Owned: 3, Adds: 1}, stats)
			report := RunFrameworkSynthesizersWithSelection(native, selection)
			require.NoError(t, report.Err)
			want := rebuildEventSQLiteRows(native)
			require.Contains(t, want.edges, add)
			require.Equal(t, want, rebuildEventSavedRows(t, b))
		})
	}
}

func TestRebuildMacroExpansionRequiresDeclarationAndBody(t *testing.T) {
	const file, lang = "proof.c", "c"
	caller := rebuildEventNode(file, "run", lang, "")
	callee := rebuildEventNode(file, "work", lang, "")
	macro := &graph.Node{ID: file + "::ACTIVE", Name: "ACTIVE", Kind: graph.KindMacro, FilePath: file, Language: lang,
		Meta: map[string]any{"macro_kind": macroFunctionKindMeta}}
	dormant := &graph.Node{ID: file + "::DORMANT", Name: "DORMANT", Kind: graph.KindMacro, FilePath: file, Language: lang,
		Meta: map[string]any{"macro_kind": macroFunctionKindMeta}}
	call := func(from, to string, line int) *graph.Edge {
		return &graph.Edge{From: from, To: to, Kind: graph.EdgeCalls, FilePath: file, Line: line}
	}
	body := call(macro.ID, callee.ID, 1)
	use := call(caller.ID, "unresolved::ACTIVE", 3)
	stale := call(caller.ID, "unresolved::ACTIVE", 4)
	facts := rebuildEventFacts(file, lang, []*graph.Node{caller, callee, macro, dormant}, body, use, stale,
		call(caller.ID, dormant.ID, 5), call(file+"::missing", "unresolved::ACTIVE", 6))
	selection, err := NewFrameworkSynthesizerSelection([]string{SynthMacroExpansion})
	require.NoError(t, err)
	for _, what := range []string{"declaration", "body", "complete"} {
		t.Run(what, func(t *testing.T) {
			ctx := t.Context()
			b, native := openRebuildEventStores(t, facts)
			frozen, err := FreezeRebuildFrameworkSelection(ctx, b.RebuildView(), selection)
			require.NoError(t, err)
			require.NoError(t, FreezeRebuildMacroExpansionInputs(ctx, b, frozen))
			moved := *stale
			moved.To = macro.ID
			moves := []graph.EdgeReindex{{Edge: &moved, OldTo: stale.To}}
			require.NoError(t, b.ApplyLinkReindexes(ctx, moves))
			before := rebuildEventSavedRows(t, b)
			receipt := facts
			switch what {
			case "declaration":
				receipt.Nodes = slices.DeleteFunc(slices.Clone(facts.Nodes), func(n *graph.Node) bool { return n == macro })
			case "body":
				receipt.Edges = slices.DeleteFunc(slices.Clone(facts.Edges), func(e *graph.Edge) bool { return e == body })
			}
			stats, err := RunRebuildMacroExpansion(ctx, b, rebuildGRPCReceipts(receipt), frozen)
			add := macroExpansionEdge(caller.ID, callee.ID, file, use.Line, macro.Name)
			if what != "complete" {
				var refused *store_pebble.RebuildUnknownProvenanceError
				require.ErrorAs(t, err, &refused)
				require.Equal(t, &store_pebble.RebuildUnknownProvenanceError{Count: 1, First: graph.EdgeIdentityFor(add)}, refused)
				require.Zero(t, stats)
				require.Equal(t, before, rebuildEventSavedRows(t, b))
				return
			}
			require.NoError(t, err)
			require.Equal(t, store_pebble.RebuildMacroExpansionStats{Sites: 1, Owned: 1, Adds: 1}, stats)
			// Native freezes the same keys before the move and refetches only
			// those keys at its turn, not the newly created replacement.
			streams := newFrameworkStreamCandidates(native, map[string]int{"c": 1}, map[string]int{SynthMacroExpansion: 1})
			collectFrameworkEdgeCensus(native, streams)
			keys := streams.perPass[SynthMacroExpansion]
			require.NotNil(t, keys)
			require.True(t, native.ReindexEdgesApplied(moves))
			bundle := refetchFrameworkCandidates(native, keys)
			require.NotNil(t, bundle)
			require.Equal(t, 1, runLegacyFrameworkSynth(native, func(g graph.Store) int { return resolveMacroExpansionCalls(g, bundle) }))
			want := rebuildEventSQLiteRows(native)
			require.Contains(t, want.edges, add)
			require.Equal(t, want, rebuildEventSavedRows(t, b))
		})
	}
}

func TestRebuildMacroExpansionCapsOnlyReachedRoles(t *testing.T) {
	const file, lang = "cap.c", "c"
	large := strings.Repeat("x", 16<<20)
	selection, err := NewFrameworkSynthesizerSelection([]string{SynthMacroExpansion})
	require.NoError(t, err)
	for _, tc := range []struct {
		name, largeRow string
		unrelated      bool
		refusal        string
	}{
		{"large active body", "body", false, "macro expansion edge row"},
		{"large deciding caller", "caller", false, "macro expansion caller node"},
		{"large unrelated body", "body", true, ""},
		{"large unrelated caller", "caller", true, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := t.Context()
			caller := rebuildEventNode(file, "run", lang, "")
			nonmacro := rebuildEventNode(file, "ordinary", lang, "")
			callee := rebuildEventNode(file, "work", lang, "")
			macro := &graph.Node{ID: file + "::APPLY", Name: "APPLY", Kind: graph.KindMacro, FilePath: file, Language: lang,
				Meta: map[string]any{"macro_kind": macroFunctionKindMeta}}
			body := &graph.Edge{From: macro.ID, To: callee.ID, Kind: graph.EdgeCalls, FilePath: file, Line: 1}
			use := &graph.Edge{From: caller.ID, To: "unresolved::APPLY", Kind: graph.EdgeCalls, FilePath: file, Line: 3}
			edges := []*graph.Edge{body, use}
			if tc.largeRow == "caller" {
				caller.Meta = map[string]any{"payload": large}
				if tc.unrelated {
					caller.RepoPrefix = "another-repo"
				}
			} else if tc.unrelated {
				extra := *body
				extra.From, extra.Context = nonmacro.ID, large
				edges = append(edges, &extra)
			} else {
				body.Context = large
			}
			facts := rebuildEventFacts(file, lang, []*graph.Node{caller, callee, macro, nonmacro}, edges...)
			b := openRebuildGRPCBuild(t, facts)
			frozen, err := FreezeRebuildFrameworkSelection(ctx, b.RebuildView(), selection)
			require.NoError(t, err)
			require.True(t, frozen.allows(SynthMacroExpansion))
			require.NoError(t, FreezeRebuildMacroExpansionInputs(ctx, b, frozen))
			stats, err := RunRebuildMacroExpansion(ctx, b, rebuildGRPCReceipts(facts), frozen)
			id := graph.EdgeIdentityFor(macroExpansionEdge(caller.ID, callee.ID, file, use.Line, macro.Name))
			added, readErr := b.RebuildView().Edge(ctx, id)
			require.NoError(t, readErr)
			if tc.refusal != "" {
				require.ErrorIs(t, err, store_pebble.ErrRebuildLinkInputSize)
				require.ErrorContains(t, err, tc.refusal)
				require.Zero(t, stats)
				require.Nil(t, added)
				return
			}
			require.NoError(t, err)
			if tc.largeRow == "caller" {
				require.Zero(t, stats)
				require.Nil(t, added)
				return
			}
			require.EqualValues(t, 1, stats.Adds)
			require.Equal(t, macroExpansionEdge(caller.ID, callee.ID, file, use.Line, macro.Name), added)
		})
	}
}

func TestRebuildMacroExpansionCFamilyMarkerNodeCap(t *testing.T) {
	const file, lang = "macro.js", "javascript"
	caller := rebuildEventNode(file, "run", lang, "")
	callee := rebuildEventNode(file, "work", lang, "")
	macro := &graph.Node{ID: file + "::APPLY", Name: "APPLY", Kind: graph.KindMacro, FilePath: file, Language: lang,
		Meta: map[string]any{"macro_kind": macroFunctionKindMeta}}
	marker := &graph.Node{ID: "a.c", Kind: graph.KindFile, Language: "c", FilePath: "a.c"}
	spare := &graph.Node{ID: "b.c", Kind: graph.KindFile, Language: "c", FilePath: "b.c"}
	body := &graph.Edge{From: macro.ID, To: callee.ID, Kind: graph.EdgeCalls, FilePath: file, Line: 1}
	use := &graph.Edge{From: caller.ID, To: "unresolved::APPLY", Kind: graph.EdgeCalls, FilePath: file, Line: 3}
	facts := []store_pebble.ParseFacts{
		rebuildEventFacts(file, lang, []*graph.Node{caller, callee, macro}, body, use),
		rebuildEventFacts(marker.FilePath, marker.Language, []*graph.Node{marker}),
		rebuildEventFacts(spare.FilePath, spare.Language, []*graph.Node{spare}),
	}
	selection, err := NewFrameworkSynthesizerSelection([]string{SynthMacroExpansion})
	require.NoError(t, err)
	for _, count := range []int{2, 3} {
		t.Run(fmt.Sprintf("%d receipts", count), func(t *testing.T) {
			ctx := t.Context()
			b := openRebuildGRPCBuild(t, facts...)
			frozen, err := FreezeRebuildFrameworkSelection(ctx, b.RebuildView(), selection)
			require.NoError(t, err)
			require.True(t, frozen.allows(SynthMacroExpansion))
			require.NoError(t, FreezeRebuildMacroExpansionInputs(ctx, b, frozen))
			large := *marker
			large.Meta = map[string]any{"padding": strings.Repeat("x", 16<<20)}
			b.AddNode(&large)
			stats, err := RunRebuildMacroExpansion(ctx, b, rebuildGRPCReceipts(facts[:count]...), frozen)
			want := macroExpansionEdge(caller.ID, callee.ID, file, use.Line, macro.Name)
			got, readErr := b.RebuildView().Edge(ctx, graph.EdgeIdentityFor(want))
			require.NoError(t, readErr)
			if count == 2 {
				require.ErrorIs(t, err, store_pebble.ErrRebuildMacroExpansion)
				require.ErrorIs(t, err, store_pebble.ErrRebuildLinkInputSize)
				require.ErrorContains(t, err, "C-family marker node")
				require.Zero(t, stats)
				require.Nil(t, got)
				return
			}
			require.NoError(t, err)
			require.EqualValues(t, 1, stats.Adds)
			require.Equal(t, want, got)
		})
	}
}
