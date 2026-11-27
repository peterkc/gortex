package resolver

import (
	"fmt"
	"slices"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/graph/store_pebble"
)

const (
	rebuildBridgeExact     = "p/Bridge.swift::custom"
	rebuildBridgeExactObjC = "p/Bridge.m::deliverTo:"
	rebuildBridgeReverse   = "p/Bridge.swift::move"
	rebuildBridgeMoveObjC  = "p/Bridge.m::moveFrom:to:"
	rebuildBridgeProtocol  = "p/Protocol.swift::BridgeDelegate"
	rebuildBridgeAdopter   = "p/Bridge.m::BridgeClass"
)

func rebuildSwiftObjCFixture(extra int) ([]store_pebble.ParseFacts, *graph.Edge) {
	const swiftPath, objcPath, protoPath = "p/Bridge.swift", "p/Bridge.m", "p/Protocol.swift"
	node := func(id, name, lang, path string, kind graph.NodeKind, line int, meta map[string]any) *graph.Node {
		return &graph.Node{ID: id, Name: name, Kind: kind, Language: lang, FilePath: path, StartLine: line, Meta: meta}
	}
	swift := []*graph.Node{
		node(rebuildBridgeExact, "custom", "swift", swiftPath, graph.KindMethod, 5, map[string]any{"objc_selector": "deliverTo:"}),
		node(rebuildBridgeReverse, "move", "swift", swiftPath, graph.KindMethod, 7, nil),
	}
	objc := []*graph.Node{
		node(rebuildBridgeExactObjC, "deliverTo:", "objc", objcPath, graph.KindMethod, 11, nil),
		node(rebuildBridgeMoveObjC, "moveFrom:to:", "objc", objcPath, graph.KindMethod, 13, nil),
		node(rebuildBridgeAdopter, "BridgeClass", "objc", objcPath, graph.KindType, 15,
			map[string]any{"objc_protocols": " Unknown, BridgeDelegate "}),
	}
	// Long, distinct selectors put over 1 MiB into the exact-key census
	// with a few hundred rows and without creating a large match bucket.
	padding := strings.Repeat("x", 8<<10)
	for i := range extra {
		sel := fmt.Sprintf("bridge%d%s:", i, padding)
		swift = append(swift, node(fmt.Sprintf("%s::swift%d", swiftPath, i), "hidden", "swift", swiftPath,
			graph.KindMethod, 20+i, map[string]any{"objc_selector": sel}))
		objc = append(objc, node(fmt.Sprintf("%s::objc%d", objcPath, i), sel, "objc", objcPath,
			graph.KindMethod, 20+i, nil))
	}
	stationary := &graph.Edge{
		From:            rebuildBridgeExactObjC,
		To:              rebuildBridgeExact,
		Kind:            graph.EdgeReferences,
		FilePath:        objcPath,
		Line:            11,
		Confidence:      0.9,
		ConfidenceLabel: graph.ConfidenceLabelFor(graph.EdgeReferences, 0.9),
		Origin:          graph.OriginASTResolved,
		Meta:            map[string]any{"witness": "old stationary row"},
	}
	return []store_pebble.ParseFacts{
		{
			EffectiveLanguage: "swift",
			File:              graph.FileMetaRow{FilePath: swiftPath, NodeCount: len(swift)},
			Nodes:             swift,
		},
		{
			EffectiveLanguage: "objc",
			File:              graph.FileMetaRow{FilePath: objcPath, NodeCount: len(objc)},
			Nodes:             objc,
			Edges:             []*graph.Edge{stationary},
		},
		{
			EffectiveLanguage: "swift",
			File:              graph.FileMetaRow{FilePath: protoPath, NodeCount: 1},
			Nodes:             []*graph.Node{
				node(rebuildBridgeProtocol, "BridgeDelegate", "swift", protoPath, graph.KindInterface, 3, map[string]any{"objc": true}),
			},
		},
	}, stationary
}

func rebuildSwiftObjCPrivateEdges(t *testing.T, b *store_pebble.BuildStore) map[graph.EdgeIdentity]*graph.Edge {
	t.Helper()
	rows := make(map[graph.EdgeIdentity]*graph.Edge)
	require.NoError(t, b.RebuildView().VisitEdges(t.Context(), func(e *graph.Edge) error {
		rows[graph.EdgeIdentityFor(e)] = e
		return nil
	}))
	return rows
}

func TestRebuildSwiftObjCNativeRowsAndSpill(t *testing.T) {
	selection, err := NewFrameworkSynthesizerSelection([]string{SynthSwiftObjC})
	require.NoError(t, err)
	for _, spillAt := range []int{1, 1 << 20} {
		t.Run(fmt.Sprint(spillAt), func(t *testing.T) {
			var extra int
			if spillAt > 1 {
				extra = 144
			}
			facts, stationary := rebuildSwiftObjCFixture(extra)
			var full store_pebble.ParseFacts
			for _, file := range facts {
				full.Nodes = append(full.Nodes, file.Nodes...)
				full.Edges = append(full.Edges, file.Edges...)
			}
			native := rebuildGRPCNative(t, full)
			live := slices.Clone(facts)
			slices.Reverse(live)
			b := openRebuildGRPCBuild(t, live...)
			receipts := rebuildGRPCReceipts(facts...)
			for pass := range 2 {
				report := RunFrameworkSynthesizersWithSelection(native, selection)
				require.NoError(t, report.Err)
				require.Len(t, report.Per, 1)
				require.Equal(t, SynthSwiftObjC, report.Per[0].Name)
				require.Equal(t, 3+extra, report.Total) // Native counts bridged Swift nodes and ObjC conformers.
				want := make(map[graph.EdgeIdentity]*graph.Edge)
				for _, kind := range []graph.EdgeKind{graph.EdgeReferences, graph.EdgeImplements} {
					for e := range native.EdgesByKind(kind) {
						want[graph.EdgeIdentityFor(e)] = e
					}
				}
				require.Len(t, want, 5+2*extra)
				stats, err := RunRebuildSwiftObjC(t.Context(), b, receipts, selection, spillAt)
				require.NoError(t, err)
				if pass == 0 {
					require.Positive(t, stats.SpillBytes)
				}
				got := rebuildSwiftObjCPrivateEdges(t, b)
				require.Equal(t, want, got)
				require.Equal(t, stationary, got[graph.EdgeIdentityFor(stationary)])
				for _, witness := range []struct {
					kind     graph.EdgeKind
					from, to string
					path     string
					line     int
					selector string
				}{
					{graph.EdgeReferences, rebuildBridgeExact, rebuildBridgeExactObjC, "p/Bridge.swift", 5, "deliverTo:"},
					{graph.EdgeReferences, rebuildBridgeReverse, rebuildBridgeMoveObjC, "p/Bridge.swift", 7, "moveFrom:to:"},
					{graph.EdgeReferences, rebuildBridgeMoveObjC, rebuildBridgeReverse, "p/Bridge.m", 13, "moveFrom:to:"},
					{graph.EdgeImplements, rebuildBridgeAdopter, rebuildBridgeProtocol, "p/Bridge.m", 15, ""},
				} {
					key := graph.EdgeIdentity{
						From:     witness.from,
						To:       witness.to,
						Kind:     witness.kind,
						FilePath: witness.path,
						Line:     witness.line,
					}
					require.Contains(t, got, key)
					require.Equal(t, SynthSwiftObjC, got[key].Meta[MetaSynthesizedBy])
					if witness.selector != "" {
						require.Equal(t, witness.selector, got[key].Meta["objc_selector"])
					}
				}
			}
		})
	}
}

func TestRebuildSwiftObjCSelectionAndMarkers(t *testing.T) {
	selected, err := NewFrameworkSynthesizerSelection([]string{SynthSwiftObjC})
	require.NoError(t, err)
	unselected, err := NewFrameworkSynthesizerSelection([]string{SynthGRPCStub})
	require.NoError(t, err)
	facts, _ := rebuildSwiftObjCFixture(0)

	t.Run("unselected with both markers", func(t *testing.T) {
		b := openRebuildGRPCBuild(t, facts...)
		before := rebuildSwiftObjCPrivateEdges(t, b)
		stats, err := RunRebuildSwiftObjC(t.Context(), b, func(func(store_pebble.ParseFacts, error) bool) {
			t.Fatal("unselected bridge read receipts")
		}, unselected, 1)
		require.NoError(t, err)
		require.Zero(t, stats)
		require.Equal(t, before, rebuildSwiftObjCPrivateEdges(t, b))
	})

	t.Run("selected with only Swift marker", func(t *testing.T) {
		b := openRebuildGRPCBuild(t, facts[0])
		native := rebuildGRPCNative(t, facts[0])
		report := RunFrameworkSynthesizersWithSelection(native, selected)
		require.NoError(t, report.Err)
		require.Zero(t, report.Total)
		stats, err := RunRebuildSwiftObjC(t.Context(), b, rebuildGRPCReceipts(facts[0]), selected, 1)
		require.NoError(t, err)
		require.Zero(t, stats.Edges)
		require.Empty(t, rebuildSwiftObjCPrivateEdges(t, b))
	})
}

func TestRebuildSwiftObjCRefusesMissingEndpointReceiptBeforeWriting(t *testing.T) {
	selection, err := NewFrameworkSynthesizerSelection([]string{SynthSwiftObjC})
	require.NoError(t, err)
	facts, stationary := rebuildSwiftObjCFixture(0)
	b := openRebuildGRPCBuild(t, facts...)
	before := rebuildSwiftObjCPrivateEdges(t, b)
	require.Equal(t, map[graph.EdgeIdentity]*graph.Edge{graph.EdgeIdentityFor(stationary): stationary}, before)

	// Selector pairs are proved; only the protocol endpoint lacks a receipt.
	_, err = RunRebuildSwiftObjC(t.Context(), b, rebuildGRPCReceipts(facts[:2]...), selection, 1)
	require.ErrorIs(t, err, store_pebble.ErrRebuildUnknownProvenance)
	var refusal *store_pebble.RebuildUnknownProvenanceError
	require.ErrorAs(t, err, &refusal)
	require.Equal(t, before, rebuildSwiftObjCPrivateEdges(t, b))
}

func TestRebuildSwiftObjCProtocolOrder(t *testing.T) {
	facts, _ := rebuildSwiftObjCFixture(0)
	winner := facts[2].Nodes[0]
	loser := *winner
	loser.FilePath = "a/Protocol.swift"
	loser.ID = loser.FilePath + "::BridgeDelegate"
	// Insert the greater ID first. Native's node-ID order, not insertion
	// order or the arrival sequence of receipts, determines the protocol.
	facts = append(facts, store_pebble.ParseFacts{
		EffectiveLanguage: "swift",
		File:              graph.FileMetaRow{FilePath: loser.FilePath, NodeCount: 1},
		Nodes:             []*graph.Node{&loser},
	})
	var all store_pebble.ParseFacts
	for _, f := range facts {
		all.Nodes = append(all.Nodes, f.Nodes...)
		all.Edges = append(all.Edges, f.Edges...)
	}
	selection, err := NewFrameworkSynthesizerSelection([]string{SynthSwiftObjC})
	require.NoError(t, err)
	native := rebuildGRPCNative(t, all)
	require.NoError(t, RunFrameworkSynthesizersWithSelection(native, selection).Err)
	want := slices.Collect(native.EdgesByKind(graph.EdgeImplements))
	require.Len(t, want, 1)
	require.Equal(t, winner.ID, want[0].To)
	b := openRebuildGRPCBuild(t, facts...)
	_, err = RunRebuildSwiftObjC(t.Context(), b, rebuildGRPCReceipts(facts...), selection, 1)
	require.NoError(t, err)
	rows := rebuildSwiftObjCPrivateEdges(t, b)
	require.Equal(t, want[0], rows[graph.EdgeIdentityFor(want[0])])
	bad := openRebuildGRPCBuild(t, facts...)
	before := rebuildSwiftObjCPrivateEdges(t, bad)
	// Only the losing declaration is in the receipt stream. It cannot
	// stand in for the actual native winner.
	noWinner := slices.Delete(slices.Clone(facts), 2, 3)
	_, err = RunRebuildSwiftObjC(t.Context(), bad, rebuildGRPCReceipts(noWinner...), selection, 1)
	var refusal *store_pebble.RebuildUnknownProvenanceError
	require.ErrorAs(t, err, &refusal)
	require.Equal(t, uint64(1), refusal.Count)
	require.Equal(t, graph.EdgeIdentityFor(want[0]), refusal.First)
	require.Equal(t, before, rebuildSwiftObjCPrivateEdges(t, bad))
}
