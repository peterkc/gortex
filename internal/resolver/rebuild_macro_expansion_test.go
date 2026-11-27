package resolver

import (
	"slices"
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
