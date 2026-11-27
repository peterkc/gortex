package resolver

import (
	"fmt"
	"slices"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/graph/store_pebble"
	"github.com/zzet/gortex/internal/graph/store_sqlite"
)

func TestRebuildObserverSQLiteOracle(t *testing.T) {
	const file, lang = "channel.ts", "typescript"
	channel := newRebuildObserverChannel(file, lang)
	keep := rebuildEventNode(file, "keep", lang, "")
	winner := rebuildEventNode(file, "winner", lang, "")
	otherFile := rebuildEventNode(file, "otherFile", lang, "")
	otherLine := rebuildEventNode(file, "otherLine", lang, "")
	foreign := rebuildEventNode(file, "foreign", lang, "")
	field := &graph.Node{
		ID:       file + "::z-channel",
		Name:     "z-channel",
		Kind:     graph.KindField,
		FilePath: file,
		Language: lang,
	}
	channel.facts.Nodes = append(channel.facts.Nodes, field, keep, winner, otherFile, otherLine, foreign)
	incumbent := rebuildObserverEdge(channel.dispatch, keep, graph.EdgeCalls, channel.dispatch.StartLine)
	incumbent.Meta = map[string]any{"fixture": "durable incumbent"}
	channel.facts.Edges = append(channel.facts.Edges,
		rebuildObserverEdge(channel.registrar, field, graph.EdgeAccessesField, 3),
		rebuildObserverEdge(channel.dispatch, field, graph.EdgeAccessesField, 7),
		incumbent,
		rebuildObserverEdge(channel.caller, keep, graph.EdgeReads, 20),
		rebuildObserverEdge(channel.caller, winner, graph.EdgeReferences, 20),
		rebuildObserverEdge(channel.caller, winner, graph.EdgeReads, 20),
		rebuildObserverEdge(channel.caller, otherLine, graph.EdgeReads, 21),
		rebuildObserverEdge(channel.caller, channel.dispatch, graph.EdgeReads, 20),
		rebuildObserverEdge(channel.caller, foreign, graph.EdgeReads, 20),
	)
	// Put the later field first on arrival; the native pass sorts field IDs.
	slices.Reverse(channel.facts.Edges)
	wrongFile := rebuildObserverEdge(channel.caller, otherFile, graph.EdgeReads, 20)
	wrongFile.FilePath = "different.ts"
	other := rebuildEventFacts(wrongFile.FilePath, lang, nil, wrongFile)
	receipts, b, native := openRebuildObserverStores(t, map[string]string{
		channel.dispatch.ID: "work",
		keep.ID:             "work",
		winner.ID:           "work",
		otherFile.ID:        "work",
		otherLine.ID:        "work",
		foreign.ID:          "elsewhere",
	}, channel.facts, other)
	disabled, err := NewFrameworkSynthesizerSelection(nil)
	require.NoError(t, err)
	before := rebuildEventSavedRows(t, b)
	stats, err := RunRebuildObserver(t.Context(), b, rebuildGRPCReceipts(receipts...), disabled, 1)
	require.NoError(t, err)
	require.Zero(t, stats)
	require.Equal(t, before, rebuildEventSavedRows(t, b))

	selection := rebuildObserverSelection(t)
	for range 2 {
		require.NoError(t, RunFrameworkSynthesizersWithSelection(native, selection).Err)
		call := observerChannelEdgeBetween(native, channel.dispatch.ID, winner.ID)
		require.NotNil(t, call)
		require.Equal(t, field.ID, call.Meta["channel_field"])
		require.Equal(t, file, call.FilePath)
		require.Equal(t, channel.dispatch.StartLine, call.Line)
		require.ElementsMatch(t, []*graph.Edge{incumbent, call},
			rebuildObserverNativeDispatchCalls(native, channel.dispatch.ID))

		stats, err := RunRebuildObserver(t.Context(), b, rebuildGRPCReceipts(receipts...), selection, 1)
		require.NoError(t, err)
		require.Positive(t, stats.SpillBytes)
		require.Equal(t, rebuildEventSQLiteRows(native), rebuildEventSavedRows(t, b))
	}
}

func TestRebuildObserverCapBeforeWorkspaceAndLanguage(t *testing.T) {
	for _, tc := range []struct {
		name     string
		language string
		want     int
	}{
		{name: "40 including a foreign workspace", want: 39},
		{name: "41 including a deferred language", language: "python"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			const file, lang = "bus.go", "go"
			channel := newRebuildObserverChannel(file, lang)
			// Keep the individual rows small while the relevant key stream exceeds 1 MiB.
			padding := strings.Repeat("x", 32<<10)
			for i := range 39 {
				cb := rebuildEventNode(file, fmt.Sprintf("callback%02d%s", i, padding), lang, "")
				channel.facts.Nodes = append(channel.facts.Nodes, cb)
				channel.facts.Edges = append(channel.facts.Edges,
					rebuildObserverEdge(channel.caller, cb, graph.EdgeReads, 20))
			}
			foreign := rebuildEventNode(file, "foreign", lang, "")
			channel.facts.Nodes = append(channel.facts.Nodes, foreign)
			channel.facts.Edges = append(channel.facts.Edges,
				rebuildObserverEdge(channel.caller, foreign, graph.EdgeReads, 20))
			inputs := []store_pebble.ParseFacts{channel.facts}
			workspaces := map[string]string{foreign.ID: "elsewhere"}
			if tc.language != "" {
				cb := rebuildEventNode("extra.py", "extra", tc.language, "")
				inputs = append(inputs, rebuildEventFacts(cb.FilePath, tc.language, []*graph.Node{cb}))
				inputs[0].Edges = append(inputs[0].Edges,
					rebuildObserverEdge(channel.caller, cb, graph.EdgeReferences, 20))
				// Removing workspace or language competitors early takes this
				// channel back under the cap. Neither should become an output.
				workspaces[cb.ID] = "elsewhere"
			}
			receipts, b, native := openRebuildObserverStores(t, workspaces, inputs...)
			selection := rebuildObserverSelection(t)
			report := RunFrameworkSynthesizersWithSelection(native, selection)
			require.NoError(t, report.Err)
			require.Len(t, rebuildObserverNativeDispatchCalls(native, channel.dispatch.ID), tc.want)

			stats, err := RunRebuildObserver(t.Context(), b, rebuildGRPCReceipts(receipts...), selection, 1<<20)
			require.NoError(t, err)
			require.Equal(t, uint64(tc.want), stats.Edges)
			if tc.want > 0 {
				require.Positive(t, stats.SpillBytes)
			}
			require.Equal(t, rebuildEventSQLiteRows(native), rebuildEventSavedRows(t, b))
		})
	}
}

func TestRebuildObserverRefusesMissingWinnerReceipt(t *testing.T) {
	var inputs []store_pebble.ParseFacts
	for _, file := range []string{"a.ts", "z.ts"} {
		channel := newRebuildObserverChannel(file, "typescript")
		cb := rebuildEventNode(file, "callback", "typescript", "")
		channel.facts.Nodes = append(channel.facts.Nodes, cb)
		channel.facts.Edges = append(channel.facts.Edges,
			rebuildObserverEdge(channel.caller, cb, graph.EdgeReads, 21),
			rebuildObserverEdge(channel.caller, cb, graph.EdgeReads, 20),
		)
		inputs = append(inputs, channel.facts)
	}
	receipts, b, native := openRebuildObserverStores(t, nil, inputs...)
	before := rebuildEventSavedRows(t, b)
	selection := rebuildObserverSelection(t)
	report := RunFrameworkSynthesizersWithSelection(native, selection)
	require.NoError(t, report.Err)
	require.Equal(t, 2, report.Total)
	// Keep the later channel's wrong-line receipt and live winning row. A
	// missing proof cannot justify either dropping this pair or writing the first.
	receipts[1].Edges = receipts[1].Edges[:len(receipts[1].Edges)-1]
	stats, err := RunRebuildObserver(t.Context(), b, rebuildGRPCReceipts(receipts...), selection, 1)
	require.ErrorIs(t, err, store_pebble.ErrRebuildUnknownProvenance)
	require.Zero(t, stats)
	require.Equal(t, before, rebuildEventSavedRows(t, b))
}

type rebuildObserverChannelFixture struct {
	facts                       store_pebble.ParseFacts
	caller, registrar, dispatch *graph.Node
}

func newRebuildObserverChannel(file, language string) rebuildObserverChannelFixture {
	caller := rebuildEventNode(file, "setup", language, "")
	registrar := rebuildEventNode(file, "subscribe", language, "")
	dispatch := rebuildEventNode(file, "notify", language, "")
	registrar.Kind = graph.KindMethod
	dispatch.Kind = graph.KindMethod
	dispatch.StartLine = 6
	field := rebuildEventNode(file, "a-channel", language, "")
	field.Kind = graph.KindField
	facts := rebuildEventFacts(file, language, []*graph.Node{caller, registrar, dispatch, field},
		rebuildObserverEdge(registrar, field, graph.EdgeAccessesField, 3),
		rebuildObserverEdge(dispatch, field, graph.EdgeAccessesField, 7),
		rebuildObserverEdge(caller, registrar, graph.EdgeCalls, 20),
	)
	return rebuildObserverChannelFixture{facts: facts, caller: caller, registrar: registrar, dispatch: dispatch}
}

func rebuildObserverEdge(from, to *graph.Node, kind graph.EdgeKind, line int) *graph.Edge {
	return &graph.Edge{
		From:     from.ID,
		To:       to.ID,
		Kind:     kind,
		FilePath: from.FilePath,
		Line:     line,
		Origin:   graph.OriginASTResolved,
	}
}

func rebuildObserverNativeDispatchCalls(native *store_sqlite.Store, id string) []*graph.Edge {
	return slices.DeleteFunc(native.GetOutEdges(id), func(e *graph.Edge) bool {
		return e.Kind != graph.EdgeCalls
	})
}

func rebuildObserverSelection(t *testing.T) FrameworkSynthesizerSelection {
	t.Helper()
	selection, err := NewFrameworkSynthesizerSelection([]string{SynthObserverChannel})
	require.NoError(t, err)
	return selection
}

func openRebuildObserverStores(
	t *testing.T, workspaces map[string]string, facts ...store_pebble.ParseFacts,
) ([]store_pebble.ParseFacts, *store_pebble.BuildStore, *store_sqlite.Store) {
	t.Helper()
	receipts := make([]store_pebble.ParseFacts, 0, len(facts))
	live := make([]store_pebble.ParseFacts, 0, len(facts))
	for _, f := range facts {
		f.File.NodeCount = len(f.Nodes)
		raw, err := store_pebble.EncodeParseFacts(f)
		require.NoError(t, err)
		portable, err := store_pebble.DecodeParseFacts(raw)
		require.NoError(t, err)
		receipts = append(receipts, portable)
		// Workspace comes from live graph placement, not the parse receipt.
		f.Nodes = slices.Clone(f.Nodes)
		for i, n := range f.Nodes {
			placed := *n
			placed.WorkspaceID = workspaces[n.ID]
			f.Nodes[i] = &placed
		}
		live = append(live, f)
	}
	b, native := openRebuildEventStores(t, live...)
	require.Equal(t, rebuildEventSQLiteRows(native), rebuildEventSavedRows(t, b))
	return receipts, b, native
}
