package resolver

import (
	"slices"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/graph/store_pebble"
)

// A frozen unresolved use whose live caller repository holds no APPLY can
// still bind natively when the caller receipt names the repository of a
// live indexed APPLY. Only that add may refuse; an incumbent at the add
// identity keeps the native answer unchanged.
func TestRebuildMacroExpansionReceiptCallerRepository(t *testing.T) {
	const callerFile, macroFile, use, kept = "left/main.c", "right/apply.c", 5, 7
	macro := &graph.Node{ID: macroFile + "::APPLY", Name: "APPLY", Kind: graph.KindMacro, FilePath: macroFile,
		Language: "c", RepoPrefix: "right", Meta: map[string]any{"macro_kind": macroFunctionKindMeta}}
	handler := &graph.Node{ID: macroFile + "::handler", Name: "handler", Kind: graph.KindFunction, FilePath: macroFile,
		Language: "c", RepoPrefix: "right"}
	keep := &graph.Node{ID: macroFile + "::keep", Name: "keep", Kind: graph.KindFunction, FilePath: macroFile,
		Language: "c", RepoPrefix: "right"}
	live := &graph.Node{ID: callerFile + "::run", Name: "run", Kind: graph.KindFunction, FilePath: callerFile,
		Language: "c", RepoPrefix: "left"}
	entrant := *live
	entrant.RepoPrefix = "right"
	call := func(from, to, file string, line int) *graph.Edge {
		return &graph.Edge{From: from, To: to, Kind: graph.EdgeCalls, FilePath: file, Line: line}
	}
	macroWithControl := rebuildEventFacts(macroFile, "c", []*graph.Node{macro, handler, keep},
		call(macro.ID, handler.ID, macroFile, 1), call(keep.ID, "unresolved::APPLY", macroFile, kept))
	callerUse := call(live.ID, "unresolved::APPLY", callerFile, use)
	add := macroExpansionEdge(live.ID, handler.ID, callerFile, use, macro.Name)
	stationary := macroExpansionEdge(keep.ID, handler.ID, macroFile, kept, macro.Name)
	lsp := call(live.ID, handler.ID, callerFile, use)
	lsp.Origin = graph.OriginLSPResolved
	lsp.Confidence = 1
	lsp.ConfidenceLabel = graph.ConfidenceLabelFor(graph.EdgeCalls, 1)
	lsp.Meta = map[string]any{"witness": "ordinary handler call"}
	owned := call(live.ID, handler.ID, callerFile, use)
	owned.Origin = graph.OriginASTInferred
	owned.Meta = map[string]any{"via": "macro_expansion", "witness": "saved macro call"}
	padding := strings.Repeat("x", 16<<20)
	for _, fixture := range []struct {
		name      string
		incumbent *graph.Edge
		control   bool
		oversized bool
	}{
		{"missing add", nil, true, false},
		{"no live site", nil, false, false},
		{"real incumbent", lsp, true, false},
		{"macro incumbent", owned, true, false},
		{"large caller, missing add", nil, true, true},
		{"large caller, real incumbent", lsp, true, true},
	} {
		t.Run(fixture.name, func(t *testing.T) {
			ctx := t.Context()
			macroFacts := macroWithControl
			if !fixture.control {
				macroFacts.Edges = macroFacts.Edges[:1]
			}
			liveFacts := []store_pebble.ParseFacts{rebuildEventFacts(callerFile, "c", []*graph.Node{live}, callerUse), macroFacts}
			rawFacts := []store_pebble.ParseFacts{rebuildEventFacts(callerFile, "c", []*graph.Node{&entrant}, callerUse), macroFacts}
			b, nativeLive := openRebuildEventStores(t, liveFacts...)
			_, nativeRaw := openRebuildEventStores(t, rawFacts...)
			if fixture.incumbent != nil {
				require.NoError(t, nativeLive.AddBatchChecked(nil, []*graph.Edge{fixture.incumbent}))
				require.NoError(t, nativeRaw.AddBatchChecked(nil, []*graph.Edge{fixture.incumbent}))
				b.AddEdge(fixture.incumbent)
			}
			before := rebuildEventSavedRows(t, b)
			selection, err := NewFrameworkSynthesizerSelection([]string{SynthMacroExpansion})
			require.NoError(t, err)
			require.NoError(t, RunFrameworkSynthesizersWithSelection(nativeLive, selection).Err)
			require.NoError(t, RunFrameworkSynthesizersWithSelection(nativeRaw, selection).Err)
			liveEdges, rawEdges := rebuildEventSQLiteRows(nativeLive).edges, rebuildEventSQLiteRows(nativeRaw).edges
			require.Equal(t, fixture.control, rebuildMacroExpansionHasEdge(liveEdges, stationary), "native live control")
			require.Equal(t, fixture.control, rebuildMacroExpansionHasEdge(rawEdges, stationary), "native receipt control")
			if fixture.incumbent == nil {
				require.False(t, rebuildMacroExpansionHasEdge(liveEdges, add), "the live caller repository has no APPLY")
				require.True(t, rebuildMacroExpansionHasEdge(rawEdges, add), "the receipt caller repository selects the live APPLY")
			}

			frozen, err := FreezeRebuildFrameworkSelection(ctx, b.RebuildView(), selection)
			require.NoError(t, err)
			require.True(t, frozen.allows(SynthMacroExpansion))
			require.NoError(t, FreezeRebuildMacroExpansionInputs(ctx, b, rebuildGRPCReceipts(rawFacts...), frozen))
			if fixture.oversized {
				large := *live
				large.Meta = map[string]any{"padding": padding}
				b.AddNode(&large)
				before = rebuildEventSavedRows(t, b)
			}
			stats, err := RunRebuildMacroExpansion(ctx, b, rebuildGRPCReceipts(rawFacts...), frozen)
			if fixture.oversized && fixture.incumbent == nil {
				require.ErrorIs(t, err, store_pebble.ErrRebuildMacroExpansion)
				require.ErrorIs(t, err, store_pebble.ErrRebuildLinkInputSize)
				require.ErrorContains(t, err, "caller node")
				require.Zero(t, stats)
				require.Equal(t, before, rebuildEventSavedRows(t, b))
				return
			}
			if fixture.incumbent == nil {
				var refused *store_pebble.RebuildUnknownProvenanceError
				require.ErrorAs(t, err, &refused)
				require.Equal(t, &store_pebble.RebuildUnknownProvenanceError{Count: 1, First: graph.EdgeIdentityFor(add)}, refused,
					"refuse only the add the receipt caller can change")
				require.Zero(t, stats)
				require.Equal(t, before, rebuildEventSavedRows(t, b), "a refused turn writes no row")
				return
			}
			require.NoError(t, err, "the incumbent keeps the native receipt answer")
			require.Equal(t, uint64(1), stats.Adds)
			require.Equal(t, rawEdges, rebuildEventSavedRows(t, b).edges)
		})
	}
}

// Native never reads the caller language, so a script-host receipt whose
// effective language differs from the caller node language still proves
// the caller when its repository and placement are exact.
func TestRebuildMacroExpansionScriptHostCaller(t *testing.T) {
	const hostFile, macroFile, use = "web/view.vue", "web/apply.c", 3
	macro := &graph.Node{ID: macroFile + "::APPLY", Name: "APPLY", Kind: graph.KindMacro, FilePath: macroFile,
		Language: "c", RepoPrefix: "web", Meta: map[string]any{"macro_kind": macroFunctionKindMeta}}
	handler := &graph.Node{ID: macroFile + "::handler", Name: "handler", Kind: graph.KindFunction, FilePath: macroFile,
		Language: "c", RepoPrefix: "web"}
	caller := &graph.Node{ID: hostFile + "::run", Name: "run", Kind: graph.KindFunction, FilePath: hostFile,
		Language: "typescript", RepoPrefix: "web"}
	macroFacts := rebuildEventFacts(macroFile, "c", []*graph.Node{macro, handler},
		&graph.Edge{From: macro.ID, To: handler.ID, Kind: graph.EdgeCalls, FilePath: macroFile, Line: 1})
	host := rebuildEventFacts(hostFile, "vue", []*graph.Node{caller},
		&graph.Edge{From: caller.ID, To: "unresolved::APPLY", Kind: graph.EdgeCalls, FilePath: hostFile, Line: use})
	add := macroExpansionEdge(caller.ID, handler.ID, hostFile, use, macro.Name)
	for _, fixture := range []struct {
		name     string
		language string
		repo     string
	}{
		{"script host receipt", "vue", "web"},
		{"empty receipt language", "", "web"},
		{"receipt caller in another repository", "vue", "elsewhere"},
	} {
		t.Run(fixture.name, func(t *testing.T) {
			ctx := t.Context()
			b, native := openRebuildEventStores(t, host, macroFacts)
			selection, err := NewFrameworkSynthesizerSelection([]string{SynthMacroExpansion})
			require.NoError(t, err)
			require.NoError(t, RunFrameworkSynthesizersWithSelection(native, selection).Err)
			want := rebuildEventSQLiteRows(native)
			require.True(t, rebuildMacroExpansionHasEdge(want.edges, add), "native adds the use-site call")

			before := rebuildEventSavedRows(t, b)
			frozen, err := FreezeRebuildFrameworkSelection(ctx, b.RebuildView(), selection)
			require.NoError(t, err)
			require.True(t, frozen.allows(SynthMacroExpansion))
			raw := *caller
			raw.RepoPrefix = fixture.repo
			receipt := rebuildEventFacts(hostFile, fixture.language, []*graph.Node{&raw}, host.Edges...)
			require.NoError(t, FreezeRebuildMacroExpansionInputs(ctx, b, rebuildGRPCReceipts(receipt, macroFacts), frozen))
			stats, err := RunRebuildMacroExpansion(ctx, b, rebuildGRPCReceipts(receipt, macroFacts), frozen)
			if fixture.name != "script host receipt" {
				var refused *store_pebble.RebuildUnknownProvenanceError
				require.ErrorAs(t, err, &refused)
				require.Equal(t, &store_pebble.RebuildUnknownProvenanceError{Count: 1, First: graph.EdgeIdentityFor(add)}, refused)
				require.Zero(t, stats)
				require.Equal(t, before, rebuildEventSavedRows(t, b), "a refused turn writes no row")
				return
			}
			require.NoError(t, err, "a non-empty receipt language is enough for the caller")
			require.Equal(t, uint64(1), stats.Adds)
			require.Equal(t, want, rebuildEventSavedRows(t, b))
		})
	}
}

func rebuildMacroExpansionHasEdge(edges []*graph.Edge, want *graph.Edge) bool {
	id := graph.EdgeIdentityFor(want)
	return slices.ContainsFunc(edges, func(e *graph.Edge) bool { return graph.EdgeIdentityFor(e) == id })
}
