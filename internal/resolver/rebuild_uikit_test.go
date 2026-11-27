package resolver

import (
	"slices"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/graph/store_pebble"
)

func TestRebuildUIKitSQLiteOracle(t *testing.T) {
	const swiftFile, objcFile, goFile = "App/Sources/Use.swift", "App/Sources/Use.m", "App/Sources/Use.go"
	node := func(file, name, language string, kind graph.NodeKind) *graph.Node {
		return &graph.Node{ID: file + "::" + name, Name: name, FilePath: file, Language: language, Kind: kind}
	}
	swift := node(swiftFile, "use", "swift", graph.KindFunction)
	objc := node(objcFile, "use", "objc", graph.KindFunction)
	// Only the exact source node path rules this source out: its language
	// and the edge's recording path would both admit it.
	nonApple := node(goFile, "use", "swift", graph.KindFunction)
	local := node(swiftFile, "DetailViewController", "swift", graph.KindType)
	preferred := node("App/ViewControllers/Detail.go", local.Name, "go", graph.KindType)
	cell := node("App/Cells/PostCell.swift", "PostCell", "swift", graph.KindType)
	delegate := node(swiftFile, "FeedDelegate", "swift", graph.KindType)
	dataSource := node("App/Shared/FeedDataSource.swift", "FeedDataSource", "swift", graph.KindType)

	site := func(from, name, path string, kind graph.EdgeKind, line int) *graph.Edge {
		return &graph.Edge{From: from, To: "unresolved::" + name, Kind: kind, FilePath: path, Line: line,
			Origin: graph.OriginTextMatched, Confidence: 0.42, ConfidenceLabel: "AMBIGUOUS",
			Meta: map[string]any{"site": "parse"}}
	}
	makeController := site(swift.ID, preferred.Name, swiftFile, graph.EdgeInstantiates, 1)
	ref := site(swift.ID, cell.Name, swiftFile, graph.EdgeReferences, 2)
	call := site(swift.ID, dataSource.Name, swiftFile, graph.EdgeCalls, 3)
	typed := site(objc.ID, delegate.Name, objcFile, graph.EdgeTypedAs, 4)
	prior := site(swift.ID, cell.Name, swiftFile, graph.EdgeReferences, 5)
	foreign := site(nonApple.ID, cell.Name, swiftFile, graph.EdgeReferences, 6)

	facts := []store_pebble.ParseFacts{
		rebuildEventFacts(swiftFile, "swift", []*graph.Node{swift, local, delegate}, makeController, ref, call, prior, foreign),
		rebuildEventFacts(objcFile, "objc", []*graph.Node{objc}, typed),
		rebuildEventFacts(goFile, "go", []*graph.Node{nonApple}),
		rebuildEventFacts(cell.FilePath, cell.Language, []*graph.Node{cell}),
		rebuildEventFacts(dataSource.FilePath, dataSource.Language, []*graph.Node{dataSource}),
		rebuildEventFacts(preferred.FilePath, preferred.Language, []*graph.Node{preferred}),
	}
	for i := range facts {
		facts[i] = roundTripRebuildRNBridgeFacts(t, facts[i])
	}
	// Only Calls and References were present at the freeze. The other two
	// kinds arrive before UIKit, with their original parse receipts.
	cold := slices.Clone(facts)
	cold[0].Edges = cold[0].Edges[1:]
	cold[1].Edges = nil
	live := []*graph.Edge{makeController, typed}

	selected, err := NewFrameworkSynthesizerSelection([]string{SynthUIKitResolve})
	require.NoError(t, err)
	for _, name := range []string{"complete", "missing_preferred_target_receipt"} {
		t.Run(name, func(t *testing.T) {
			ctx := t.Context()
			build, native := openRebuildEventStores(t, cold...)
			require.Equal(t, rebuildEventSQLiteRows(native), rebuildEventSavedRows(t, build))
			frozen, err := FreezeRebuildFrameworkSelection(ctx, build.RebuildView(), selected)
			require.NoError(t, err)
			require.True(t, frozen.allows(SynthUIKitResolve))
			require.NoError(t, FreezeRebuildUIKitInputs(ctx, build, frozen))

			// An exact predecessor reindex consumes one captured reference.
			// The other site with the same From and To must still bind.
			settled := *prior
			settled.To, settled.Origin = local.ID, graph.OriginASTResolved
			settled.Confidence, settled.ConfidenceLabel = 1, "EXTRACTED"
			settled.Meta = map[string]any{"site": "predecessor"}
			moves := []graph.EdgeReindex{{Edge: &settled, OldTo: prior.To}}
			require.NoError(t, build.ApplyLinkReindexes(ctx, moves))
			require.True(t, native.ReindexEdgesApplied(moves))
			build.AddBatch(nil, live)
			require.NoError(t, native.AddBatchChecked(nil, live))
			before := rebuildEventSavedRows(t, build)
			require.Equal(t, rebuildEventSQLiteRows(native), before)
			require.Contains(t, before.edges, &settled)
			require.NotContains(t, before.edges, prior)

			if name == "missing_preferred_target_receipt" {
				// The preferred Go target is still saved; accepting only the
				// proved same-directory Swift target would change the winner.
				changed, err := RunRebuildUIKit(ctx, build, rebuildGRPCReceipts(facts[:len(facts)-1]...), frozen)
				require.ErrorIs(t, err, store_pebble.ErrRebuildUnknownProvenance)
				require.Zero(t, changed)
				require.Equal(t, before, rebuildEventSavedRows(t, build))
				return
			}

			want := rebuildEventRows{nodes: before.nodes, edges: []*graph.Edge{&settled, foreign}}
			for _, binding := range []struct {
				edge       *graph.Edge
				target     *graph.Node
				confidence float64
			}{
				{makeController, preferred, 0.9}, // Preferred directory beats the same directory.
				{ref, cell, 0.9},
				{typed, delegate, 0.85}, // Same directory, no preferred candidate.
				{call, dataSource, 0.7}, // Sole candidate outside both directories.
			} {
				bound := *binding.edge
				bound.To, bound.Origin = binding.target.ID, graph.OriginASTInferred
				bound.Confidence = binding.confidence
				bound.ConfidenceLabel = graph.ConfidenceLabelFor(bound.Kind, binding.confidence)
				bound.Meta = map[string]any{
					"site": "parse", MetaSynthesizedBy: SynthUIKitResolve, MetaProvenance: ProvenanceHeuristic,
				}
				want.edges = append(want.edges, &bound)
			}
			want.sort()
			report := RunFrameworkSynthesizersWithSelection(native, selected)
			require.NoError(t, report.Err)
			require.Equal(t, 4, report.Total)
			require.Equal(t, want, rebuildEventSQLiteRows(native))

			changed, err := RunRebuildUIKit(ctx, build, rebuildGRPCReceipts(facts...), frozen)
			require.NoError(t, err)
			require.EqualValues(t, 4, changed)
			require.Equal(t, want, rebuildEventSavedRows(t, build))
		})
	}
}

func TestRebuildUIKitKeepsPreLoopSharedSites(t *testing.T) {
	const file, lang = "App/Use.swift", "swift"
	source := rebuildEventNode(file, "run", lang, "")
	target := rebuildEventNode(file, "DetailViewController", lang, "")
	target.Kind = graph.KindType
	call := &graph.Edge{From: source.ID, To: "unresolved::" + target.Name, Kind: graph.EdgeCalls, FilePath: file, Line: 1}
	ref := &graph.Edge{From: source.ID, To: call.To, Kind: graph.EdgeReferences, FilePath: file, Line: 2}
	lateCall, lateRef := *call, *ref
	lateCall.Line, lateRef.Line = 3, 4
	late := []*graph.Edge{&lateCall, &lateRef}
	facts := rebuildEventFacts(file, lang, []*graph.Node{source, target}, call, ref)
	build, native := openRebuildEventStores(t, facts)
	ctx := t.Context()
	selected, err := NewFrameworkSynthesizerSelection([]string{SynthUIKitResolve})
	require.NoError(t, err)
	frozen, err := FreezeRebuildFrameworkSelection(ctx, build.RebuildView(), selected)
	require.NoError(t, err)
	require.True(t, frozen.allows(SynthUIKitResolve))
	require.NoError(t, FreezeRebuildUIKitInputs(ctx, build, frozen))
	streams := newFrameworkStreamCandidates(native, map[string]int{"apple": 1}, map[string]int{SynthUIKitResolve: 1})
	collectFrameworkEdgeCensus(native, streams)

	// An earlier turn introduces two eligible, receipt-backed sites.
	// They are live when UIKit runs but not in either shared snapshot.
	build.AddBatch(nil, late)
	require.NoError(t, native.AddBatchChecked(nil, late))
	bundle := streams.passStreams(native, SynthUIKitResolve)
	require.NotNil(t, bundle)
	defer streams.releasePass(SynthUIKitResolve, bundle)
	require.Len(t, bundle.calls, 1)
	require.Len(t, bundle.refs, 1)
	require.Equal(t, 2, runLegacyFrameworkSynth(native, func(g graph.Store) int { return resolveUIKitRefs(g, bundle) }))
	want := rebuildEventSQLiteRows(native)
	require.Contains(t, want.edges, &lateCall)
	require.Contains(t, want.edges, &lateRef)

	facts.Edges = []*graph.Edge{call, ref, &lateCall, &lateRef}
	moves, err := RunRebuildUIKit(ctx, build, rebuildGRPCReceipts(facts), frozen)
	require.NoError(t, err)
	require.EqualValues(t, 2, moves)
	require.Equal(t, want, rebuildEventSavedRows(t, build))
}
