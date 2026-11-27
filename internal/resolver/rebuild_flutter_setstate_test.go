package resolver

import (
	"slices"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/graph/store_pebble"
	"github.com/zzet/gortex/internal/parser/languages"
)

func TestRebuildFlutterSetStateAfterReact(t *testing.T) {
	const file = "widget.go"
	const class = file + "::Widget"
	const caller = class + ".refresh"
	facts, err := languages.NewGoExtractor().Extract(file, []byte(`package p
 type Widget struct{}
 func (w *Widget) render() {}
 func (w *Widget) build() {}
 func (w *Widget) setState() {}
 func (w *Widget) refresh() {
     w.setState()
 }
`))
	require.NoError(t, err)
	t.Cleanup(facts.ReleaseTree)
	b, native, proof := openRebuildReactSetStateStores(t, rebuildEventFacts(file, "go", facts.Nodes, facts.Edges...))
	ctx := t.Context()
	selection, err := NewFrameworkSynthesizerSelection([]string{SynthReactSetState, SynthFlutterSetState})
	require.NoError(t, err)
	react := rebuildReactSetStateSelection(t)
	require.NoError(t, RunFrameworkSynthesizersWithSelection(native, react).Err)
	receipts := rebuildGRPCReceipts(proof...)
	reactStats, err := RunRebuildReactSetState(ctx, b, receipts, selection)
	require.NoError(t, err)
	require.EqualValues(t, 1, reactStats.Edges)
	beforeFlutter := rebuildEventSavedRows(t, b)
	require.Equal(t, rebuildEventSQLiteRows(native), beforeFlutter)

	stats, err := RunRebuildFlutterSetState(ctx, b, receipts, react)
	require.NoError(t, err)
	require.Zero(t, stats)
	require.Equal(t, beforeFlutter, rebuildEventSavedRows(t, b))

	unproved := proof[0]
	unproved.Edges = slices.DeleteFunc(slices.Clone(unproved.Edges), func(e *graph.Edge) bool {
		return e.From == caller && e.Kind == graph.EdgeCalls && isSetStateTarget(e.To)
	})
	require.Less(t, len(unproved.Edges), len(proof[0].Edges))
	stats, err = RunRebuildFlutterSetState(ctx, b, rebuildGRPCReceipts(unproved), selection)
	require.ErrorIs(t, err, store_pebble.ErrRebuildFlutterSetState)
	require.ErrorIs(t, err, store_pebble.ErrRebuildUnknownProvenance)
	require.Zero(t, stats)
	require.Equal(t, beforeFlutter, rebuildEventSavedRows(t, b))

	flutter, err := NewFrameworkSynthesizerSelection([]string{SynthFlutterSetState})
	require.NoError(t, err)
	report := RunFrameworkSynthesizersWithSelection(native, flutter)
	require.NoError(t, report.Err)
	require.Equal(t, 1, report.Total)
	want := rebuildEventSQLiteRows(native)
	out := rebuildFlutterSetStateOutput(caller, class+".build", class, file, 6)
	require.Contains(t, want.edges, out)
	require.Len(t, want.edges, len(beforeFlutter.edges)+1)
	for _, e := range beforeFlutter.edges {
		require.Contains(t, want.edges, e)
	}
	for i := range 2 {
		stats, err := RunRebuildFlutterSetState(ctx, b, receipts, selection)
		require.NoError(t, err)
		require.EqualValues(t, 1-i, stats.Edges)
		require.Equal(t, want, rebuildEventSavedRows(t, b))
	}
}

func TestRebuildFlutterSetStateUsesCheckedReactCall(t *testing.T) {
	const file, class = "widget.go", "widget.go::Widget"
	const callerID, renderID, buildID = class + ".refresh", class + ".setState", class + ".build"
	caller := &graph.Node{ID: callerID, Name: "refresh", Kind: graph.KindMethod, FilePath: file, Language: "go", StartLine: 8}
	// The lifecycle is named render. Its persisted ID makes the React
	// output a qualifying live call for Flutter's native target check.
	render := &graph.Node{ID: renderID, Name: "render", Kind: graph.KindMethod, FilePath: file, Language: "go", StartLine: 3}
	build := &graph.Node{ID: buildID, Name: "build", Kind: graph.KindMethod, FilePath: file, Language: "go", StartLine: 5}
	raw := &graph.Edge{From: callerID, To: "unresolved::setState", Kind: graph.EdgeCalls, FilePath: file, Line: 9}
	facts := rebuildEventFacts(file, "go", []*graph.Node{caller, render, build},
		&graph.Edge{From: callerID, To: class, Kind: graph.EdgeMemberOf, FilePath: file, Line: 8},
		&graph.Edge{From: renderID, To: class, Kind: graph.EdgeMemberOf, FilePath: file, Line: 3},
		&graph.Edge{From: buildID, To: class, Kind: graph.EdgeMemberOf, FilePath: file, Line: 5}, raw)
	b, native, proof := openRebuildReactSetStateStores(t, facts)
	ctx := t.Context()
	receipts := rebuildGRPCReceipts(proof...)
	react := rebuildReactSetStateSelection(t)
	stats, err := RunRebuildReactSetState(ctx, b, receipts, react)
	require.NoError(t, err)
	require.EqualValues(t, 1, stats.Edges)
	require.NoError(t, RunFrameworkSynthesizersWithSelection(native, react).Err)

	// Remove the original witness from the live target predicate in both
	// stores. Keep its receipt, so it cannot substitute for a live call.
	moved := *raw
	moved.To = class + ".helper"
	move := []graph.EdgeReindex{{Edge: &moved, OldTo: raw.To}}
	require.NoError(t, b.ApplyLinkReindexes(ctx, move))
	native.ReindexEdges(move)
	before := rebuildEventSavedRows(t, b)
	require.Equal(t, rebuildEventSQLiteRows(native), before)
	var witnesses []*graph.Edge
	for _, e := range before.edges {
		if e.From == callerID && e.Kind == graph.EdgeCalls && isSetStateTarget(e.To) {
			witnesses = append(witnesses, e)
		}
	}
	require.Len(t, witnesses, 1)
	require.Equal(t, renderID, witnesses[0].To)
	require.Equal(t, SynthReactSetState, witnesses[0].Meta[MetaSynthesizedBy])
	flutter, err := NewFrameworkSynthesizerSelection([]string{SynthFlutterSetState})
	require.NoError(t, err)
	report := RunFrameworkSynthesizersWithSelection(native, flutter)
	require.NoError(t, report.Err)
	require.Equal(t, 1, report.Total)
	stats, err = RunRebuildFlutterSetState(ctx, b, receipts, flutter)
	require.NoError(t, err)
	require.EqualValues(t, 1, stats.Edges)
	want := rebuildEventSQLiteRows(native)
	require.Contains(t, want.edges, rebuildFlutterSetStateOutput(callerID, buildID, class, file, caller.StartLine))
	require.Equal(t, want, rebuildEventSavedRows(t, b))
}

func TestRebuildFlutterSetStateKeepsNativeBuildElection(t *testing.T) {
	const file, class = "widget.go", "widget.go::Z-owner"
	facts, _ := rebuildReactSetStateFixture(file, true)
	// Reuse the competing owners and 1 MiB fixture from the React oracle.
	// Only the prior render declarations change role. IDs and receipt order
	// stay fixed so the largest method ID still wins the explicit native sort.
	for _, n := range facts.Nodes {
		if n.Name == "render" {
			n.Name = "build"
		}
	}
	b, native, proof := openRebuildReactSetStateStores(t, facts)
	selection, err := NewFrameworkSynthesizerSelection([]string{SynthReactSetState, SynthFlutterSetState})
	require.NoError(t, err)
	before := rebuildEventSavedRows(t, b)
	receipts := rebuildGRPCReceipts(proof...)
	react, err := RunRebuildReactSetState(t.Context(), b, receipts, selection)
	require.NoError(t, err)
	require.Zero(t, react)
	require.Equal(t, before, rebuildEventSavedRows(t, b))
	report := RunFrameworkSynthesizersWithSelection(native, selection)
	require.NoError(t, report.Err)
	want := rebuildEventSQLiteRows(native)
	require.Contains(t, want.edges, rebuildFlutterSetStateOutput(file+"::refresh", file+"::render-z", class, file, 10))
	require.Len(t, want.edges, len(before.edges)+41)
	stats, err := RunRebuildFlutterSetState(t.Context(), b, receipts, selection)
	require.NoError(t, err)
	require.EqualValues(t, 41, stats.Edges)
	require.Equal(t, want, rebuildEventSavedRows(t, b))
}

func rebuildFlutterSetStateOutput(from, to, class, file string, line int) *graph.Edge {
	return &graph.Edge{
		From:            from,
		To:              to,
		Kind:            graph.EdgeCalls,
		FilePath:        file,
		Line:            line,
		Origin:          graph.OriginASTInferred,
		Confidence:      0.6,
		ConfidenceLabel: graph.ConfidenceLabelFor(graph.EdgeCalls, 0.6),
		Meta: map[string]any{
			"via":             "flutter.setstate",
			"state_class":     class,
			MetaSynthesizedBy: SynthFlutterSetState,
			MetaProvenance:    ProvenanceHeuristic,
		},
	}
}
