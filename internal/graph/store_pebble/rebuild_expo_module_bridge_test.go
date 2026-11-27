package store_pebble

import (
	"fmt"
	"testing"

	"github.com/cockroachdb/pebble/v2"
	"github.com/stretchr/testify/require"
	"github.com/zzet/gortex/internal/graph"
)

func TestRebuildExpoModuleBridgeCensusAndCustody(t *testing.T) {
	caller := &graph.Node{ID: "app.ts::open", Kind: graph.KindFunction, Name: "open", FilePath: "app.ts", Language: "typescript"}
	target := &graph.Node{
		ID: "extra.go::Open", Kind: graph.KindMethod, Name: "Open", FilePath: "extra.go", Language: "go",
		Meta: map[string]any{"expo_module": "Widget", "expo_method": "open"},
	}
	// Native checks via, not whether To is unresolved. Keep the trigger
	// distinct from the Expo output by using an unrelated concrete target.
	marker := &graph.Edge{
		From: caller.ID, To: "app.ts::original", Kind: graph.EdgeCalls, FilePath: caller.FilePath, Line: 21,
		Meta: map[string]any{"via": "rn.native", "rn_module": "Widget", "rn_method": "open"},
	}
	b, receipts := seedSyntheticCustody(t,
		ParseFacts{EffectiveLanguage: caller.Language, File: graph.FileMetaRow{FilePath: caller.FilePath},
			Nodes: []*graph.Node{caller}, Edges: []*graph.Edge{marker}},
		ParseFacts{EffectiveLanguage: target.Language, File: graph.FileMetaRow{FilePath: target.FilePath}, Nodes: []*graph.Node{target}})
	family := func(language string) uint8 {
		switch language {
		case "typescript":
			return 1
		case "swift":
			return 2
		default:
			return 0
		}
	}
	isCall := func(e *graph.Edge) bool { return e.Kind == graph.EdgeCalls && e.Meta["via"] == "rn.native" }
	makeCall := func(e *graph.Edge, n *graph.Node) *graph.Edge {
		return &graph.Edge{From: e.From, To: n.ID, Kind: graph.EdgeCalls, FilePath: e.FilePath, Line: e.Line,
			Origin: graph.OriginASTInferred}
	}
	ctx := t.Context()
	before := rebuildFrameworkCustodyRows(t, b)
	flushed := b.pending.Metrics().Levels[0].TableBytesFlushed
	noReceipts := func(func(ParseFacts, error) bool) { t.Fatal("rejected families must not read receipts") }
	stats, err := b.RebuildExpoModuleBridge(ctx, noReceipts, 3, family, isCall, makeCall)
	require.NoError(t, err)
	require.Zero(t, stats)
	require.Equal(t, flushed, b.pending.Metrics().Levels[0].TableBytesFlushed)
	require.Equal(t, before, rebuildFrameworkCustodyRows(t, b))

	// An unrelated native-family marker admits the pass, not just the
	// marker's own declarations. The tagged Go target can then win.
	b.AddNode(&graph.Node{ID: "marker.swift", Kind: graph.KindFile, FilePath: "marker.swift", Language: "swift"})
	stats, err = b.RebuildExpoModuleBridge(ctx, receipts, 3, family, isCall, makeCall)
	require.NoError(t, err)
	require.EqualValues(t, 1, stats.Edges)
	want := makeCall(marker, target)
	live, err := b.pendingEdge(pendingEdgeKey(want))
	require.NoError(t, err)
	require.Equal(t, want, live)
	requireSyntheticCustody(t, b, map[graph.EdgeIdentity]rebuildEdgeProof{graph.EdgeIdentityFor(want): rebuildProofExpoModules})
}

func TestRebuildExpoModuleBridgePreservesReactNativeCustody(t *testing.T) {
	caller := &graph.Node{ID: "app.ts::open", Kind: graph.KindFunction, Name: "open", FilePath: "app.ts", Language: "typescript"}
	target := &graph.Node{ID: "native.swift::open", Kind: graph.KindMethod, Name: "open", FilePath: "native.swift", Language: "swift",
		Meta: map[string]any{"rn_module": "Widget", "rn_method": "open", "expo_module": "Widget", "expo_method": "open"}}
	peer := &graph.Node{ID: "peer.swift::open", Kind: graph.KindMethod, Name: "open", FilePath: "peer.swift", Language: "swift",
		Meta: map[string]any{"expo_module": "Widget", "expo_method": "open"}}
	first := &graph.Edge{From: caller.ID, To: "unresolved::rn::Widget::open", Kind: graph.EdgeCalls, FilePath: caller.FilePath, Line: 30,
		Meta: map[string]any{"via": "rn.native", "rn_module": "Widget", "rn_method": "open"}}
	second := *first
	second.Line = 4
	callerFacts := producerProofFacts(caller)
	callerFacts.Edges = []*graph.Edge{first, &second}
	b, receipts := seedSyntheticCustody(t, callerFacts, producerProofFacts(target), producerProofFacts(peer))

	ctx := t.Context()
	isCall := func(e *graph.Edge) bool { return e.Kind == graph.EdgeCalls && e.Meta["via"] == "rn.native" }
	makeCall := func(e *graph.Edge, n *graph.Node) *graph.Edge {
		return &graph.Edge{From: e.From, To: n.ID, Kind: graph.EdgeCalls, FilePath: e.FilePath, Line: e.Line,
			Origin: graph.OriginASTInferred}
	}
	isWeb := func(language string) bool { return language == "typescript" }
	rn, err := b.RebuildReactNativeBridge(ctx, receipts, isWeb, isCall, makeCall)
	require.NoError(t, err)
	require.EqualValues(t, 1, rn.Edges)
	want := map[graph.EdgeIdentity]rebuildEdgeProof{graph.EdgeIdentityFor(makeCall(first, target)): rebuildProofRNBridge}
	requireSyntheticCustody(t, b, want)

	family := func(language string) uint8 {
		switch language {
		case "typescript":
			return 1
		case "swift":
			return 2
		default:
			return 0
		}
	}
	before := rebuildFrameworkCustodyRows(t, b)
	bad := *target
	bad.Name = "changed"
	// Expo needs the peer receipt, not the receipt for the RN incumbent.
	expoReceipts := sharedClaimsTestReceipts(callerFacts, producerProofFacts(&bad), producerProofFacts(peer))
	expo, err := b.RebuildExpoModuleBridge(ctx, expoReceipts, 3, family, isCall, makeCall)
	require.NoError(t, err)
	require.EqualValues(t, 1, expo.Edges)
	newEdge := makeCall(first, peer)
	want[graph.EdgeIdentityFor(newEdge)] = rebuildProofExpoModules
	requireSyntheticCustody(t, b, want)
	key := pendingEdgeKey(newEdge)
	after := rebuildFrameworkCustodyRows(t, b)
	delete(after, string(key))
	delete(after, string(pendingReverseKey(newEdge, key)))
	delete(after, string(rebuildLinkRecorderKey(key)))
	require.Equal(t, before, after, "the RN incumbent and both raw sites must stay byte-identical")
}

func TestRebuildExpoModuleBridgeRejectsLostMarker(t *testing.T) {
	for _, tc := range []struct {
		name string
		lost bool
	}{
		{name: "intact marker"},
		{name: "last raw row hides marker", lost: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			caller := &graph.Node{ID: "app.ts::open", Kind: graph.KindFunction, Name: "open", FilePath: "app.ts", Language: "typescript"}
			target := &graph.Node{
				ID: "Widget.swift::open", Kind: graph.KindMethod, Name: "open", FilePath: "Widget.swift", Language: "swift",
				Meta: map[string]any{"expo_module": "Widget", "expo_method": "open"},
			}
			marker := &graph.Edge{
				From: caller.ID, To: "unresolved::rn::Widget::open", Kind: graph.EdgeCalls, FilePath: caller.FilePath, Line: 21,
				Meta: map[string]any{"via": "rn.native", "rn_module": "Widget", "rn_method": "open"},
			}
			edges := []*graph.Edge{marker}
			if tc.lost {
				last := *marker
				last.Meta = nil
				edges = append(edges, &last)
			}
			b, receipts := seedSyntheticCustody(t,
				ParseFacts{EffectiveLanguage: caller.Language, File: graph.FileMetaRow{FilePath: caller.FilePath},
					Nodes: []*graph.Node{caller}, Edges: edges},
				ParseFacts{EffectiveLanguage: target.Language, File: graph.FileMetaRow{FilePath: target.FilePath}, Nodes: []*graph.Node{target}},
			)
			stored, err := b.pendingEdge(pendingEdgeKey(marker))
			require.NoError(t, err)
			require.Equal(t, edges[len(edges)-1], stored)
			before := rebuildFrameworkCustodyRows(t, b)
			want := graph.EdgeIdentity{From: caller.ID, To: target.ID, Kind: graph.EdgeCalls, FilePath: caller.FilePath, Line: marker.Line}
			family := func(language string) uint8 {
				switch language {
				case "typescript":
					return 1
				case "swift":
					return 2
				default:
					return 0
				}
			}
			offers := 0
			stats, err := b.RebuildExpoModuleBridge(t.Context(), receipts, 3, family,
				func(e *graph.Edge) bool { return e.Kind == graph.EdgeCalls && e.Meta["via"] == "rn.native" },
				func(e *graph.Edge, n *graph.Node) *graph.Edge {
					offers++
					return &graph.Edge{From: e.From, To: n.ID, Kind: graph.EdgeCalls, FilePath: e.FilePath, Line: e.Line}
				},
			)
			if tc.lost {
				var refusal *RebuildUnknownProvenanceError
				require.ErrorIs(t, err, ErrRebuildExpoModuleBridge)
				require.ErrorAs(t, err, &refusal)
				require.Equal(t, &RebuildUnknownProvenanceError{Count: 1, First: want}, refusal)
				require.Zero(t, stats)
				require.Zero(t, offers)
				require.Equal(t, before, rebuildFrameworkCustodyRows(t, b))
				return
			}
			require.NoError(t, err)
			require.Equal(t, uint64(1), stats.Edges)
			require.Equal(t, 1, offers)
			requireSyntheticCustody(t, b, map[graph.EdgeIdentity]rebuildEdgeProof{want: rebuildProofExpoModules})
		})
	}
}

func TestRebuildReactNativeBridgeRejectsNilFamilyAdapter(t *testing.T) {
	b := newBuild(t, FullProfile)
	receipts := func(func(ParseFacts, error) bool) { t.Fatal("must not read receipts") }
	stats, err := b.RebuildReactNativeBridge(t.Context(), receipts, nil,
		func(*graph.Edge) bool { return false }, func(*graph.Edge, *graph.Node) *graph.Edge { return nil })
	require.ErrorIs(t, err, ErrRebuildReactNativeBridge)
	require.ErrorIs(t, err, ErrRebuildLinkInputSize)
	require.Zero(t, stats)
}

func TestRebuildNativeModuleBridgeFamilyPrecheckRejectsNodeIdentity(t *testing.T) {
	b := newBuild(t, FullProfile)
	b.AddNode(&graph.Node{ID: "a-web", Kind: graph.KindFile, Language: "typescript"})
	raw, err := encodeNode(&graph.Node{ID: "different", Kind: graph.KindFile, Language: "swift"})
	require.NoError(t, err)
	require.NoError(t, b.pending.Set(pendingNodeKey("z-apple"), raw, pebble.NoSync))
	before := rebuildFrameworkCustodyRows(t, b)
	receipts := func(func(ParseFacts, error) bool) {
		t.Fatal("family precheck must not read receipts")
	}
	isCall := func(*graph.Edge) bool {
		t.Fatal("family precheck must not read Calls")
		return false
	}
	makeCall := func(*graph.Edge, *graph.Node) *graph.Edge {
		t.Fatal("family precheck must not elect an output")
		return nil
	}
	isApple := func(language string) bool { return language == "swift" }
	rn, err := b.RebuildReactNativeBridge(t.Context(), receipts, isApple, isCall, makeCall)
	require.ErrorIs(t, err, ErrRebuildReactNativeBridge)
	require.ErrorIs(t, err, errInvalidRecord)
	require.ErrorContains(t, err, "React Native bridge node identity")
	require.Zero(t, rn)
	require.Equal(t, before, rebuildFrameworkCustodyRows(t, b))

	expo, err := b.RebuildExpoModuleBridge(t.Context(), receipts, 3, func(language string) uint8 {
		switch language {
		case "typescript":
			return 1
		case "swift":
			return 2
		default:
			return 0
		}
	}, isCall, makeCall)
	require.ErrorIs(t, err, ErrRebuildExpoModuleBridge)
	require.ErrorIs(t, err, errInvalidRecord)
	require.ErrorContains(t, err, "Expo module bridge node identity")
	require.Zero(t, expo)
	require.Equal(t, before, rebuildFrameworkCustodyRows(t, b))
}

func TestRebuildNativeModuleBridgeCaptureCountsLostCustodyOutputs(t *testing.T) {
	for _, name := range []string{"React Native", "Expo"} {
		t.Run(name, func(t *testing.T) {
			ctx := t.Context()
			b := newBuild(t, FullProfile)
			b.AddNode(&graph.Node{ID: "caller", Kind: graph.KindFunction, FilePath: "app.ts", Language: "typescript"})
			for _, id := range []string{"a-native", "b-native", "c-native", "z-native"} {
				b.AddNode(&graph.Node{ID: id, Kind: graph.KindMethod, Name: "open", FilePath: "native.swift", Language: "swift",
					Meta: map[string]any{"rn_module": id, "rn_method": "open", "expo_module": id, "expo_method": "open"}})
			}
			// View order sorts by marker target, not output target. The first
			// two sites affect the same pair. Two later sites claim one raw
			// recorder. The last pair sorts first, and its later site
			// has the smaller complete edge key.
			sharedRaw := pendingEdgeKey(&graph.Edge{From: "caller", To: "raw", Kind: graph.EdgeCalls, FilePath: "app.ts", Line: 1})
			for i, tc := range []struct {
				module  string
				custody rebuildEdgeCustody
			}{
				{"z-native", rebuildEdgeCustody{proof: rebuildProofDirect}},
				{"z-native", rebuildEdgeCustody{proof: rebuildProofReceiver}},
				{"b-native", rebuildEdgeCustody{raw: sharedRaw}},
				{"c-native", rebuildEdgeCustody{raw: sharedRaw}},
				{"a-native", rebuildEdgeCustody{proof: rebuildProofUntrackedMove}},
				{"a-native", rebuildEdgeCustody{proof: rebuildProofReceiver}},
			} {
				e := &graph.Edge{From: "caller", To: fmt.Sprintf("marker-%d", i), Kind: graph.EdgeCalls, FilePath: "app.ts", Line: 10 - i,
					Meta: map[string]any{"via": "rn.native", "rn_module": tc.module, "rn_method": "open"}}
				require.NoError(t, b.withWrites(func() error { return b.putEdgeWithCustody(e, tc.custody) }))
				if i == 0 {
					// Stale proof, with the trigger and key intact.
					e.Confidence = 0.6
					value, err := encodeEdge(e)
					require.NoError(t, err)
					require.NoError(t, b.pending.Set(pendingEdgeKey(e), value, pebble.NoSync))
				}
			}
			before := rebuildFrameworkCustodyRows(t, b)
			receipts := func(func(ParseFacts, error) bool) { t.Fatal("capture refusal must not read receipts") }
			isCall := func(e *graph.Edge) bool { return e.Kind == graph.EdgeCalls && e.Meta["via"] == "rn.native" }
			makeCall := func(*graph.Edge, *graph.Node) *graph.Edge {
				t.Fatal("capture refusal must not elect an output")
				return nil
			}
			var stats RebuildReactNativeBridgeStats
			var err error
			if name == "Expo" {
				stats, err = b.RebuildExpoModuleBridge(ctx, receipts, 3, func(language string) uint8 {
					if language == "typescript" {
						return 1
					}
					return 2
				}, isCall, makeCall)
				require.ErrorIs(t, err, ErrRebuildExpoModuleBridge)
			} else {
				stats, err = b.RebuildReactNativeBridge(ctx, receipts, func(language string) bool {
					return language == "typescript" || language == "swift"
				}, isCall, makeCall)
				require.ErrorIs(t, err, ErrRebuildReactNativeBridge)
			}
			var refused *RebuildUnknownProvenanceError
			require.ErrorAs(t, err, &refused)
			first := graph.EdgeIdentity{From: "caller", To: "a-native", Kind: graph.EdgeCalls, FilePath: "app.ts", Line: 5}
			require.Equal(t, &RebuildUnknownProvenanceError{Count: 4, First: first}, refused)
			require.Zero(t, stats)
			require.Equal(t, before, rebuildFrameworkCustodyRows(t, b))
		})
	}
}
