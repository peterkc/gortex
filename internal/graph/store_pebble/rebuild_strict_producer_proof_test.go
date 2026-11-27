package store_pebble

import (
	"maps"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/zzet/gortex/internal/graph"
)

func TestRebuildKMPExpectActualProvesExactMarkerRole(t *testing.T) {
	for _, tc := range []struct {
		name   string
		raw    map[string]any
		live   map[string]any
		refuse bool
	}{
		{"exact", nil, nil, false},
		{"unrelated", nil, map[string]any{"note": "changed"}, false},
		{"empty", nil, map[string]any{"kmp_role": ""}, true},
		{"nil", nil, map[string]any{"kmp_role": nil}, true},
		{"typed", nil, map[string]any{"kmp_role": []string{"expect"}}, true},
		{"changed type", map[string]any{"kmp_role": int(1)}, map[string]any{"kmp_role": int64(1)}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			expect := &graph.Node{ID: "expect", Name: "Clock", Kind: graph.KindFunction, Language: "go", FilePath: "common.go",
				Meta: map[string]any{"kmp_role": "expect"}}
			actual := &graph.Node{ID: "actual", Name: "Clock", Kind: graph.KindFunction, Language: "java", FilePath: "platform.java",
				Meta: map[string]any{"kmp_role": "actual"}}
			// Neither declaration can also prove the sole Kotlin marker.
			marker := &graph.Node{ID: "marker.kt", Kind: graph.KindFile, Language: "kotlin", FilePath: "marker.kt", Meta: tc.raw}
			b, receipts := seedSyntheticCustody(t, producerProofFacts(expect), producerProofFacts(actual), producerProofFacts(marker))
			live := *marker
			live.Meta = tc.live
			b.AddNode(&live)
			makeEdge := func(n *graph.Node, to string, kind graph.EdgeKind) *graph.Edge {
				return &graph.Edge{From: n.ID, To: to, Kind: kind, FilePath: n.FilePath, Line: n.StartLine}
			}
			stats, err := b.RebuildKMPExpectActual(t.Context(), receipts, 1, makeEdge)
			implements := graph.EdgeIdentityFor(makeEdge(actual, expect.ID, graph.EdgeImplements))
			if tc.refuse {
				require.ErrorIs(t, err, ErrRebuildKMPExpectActual)
				requireProducerProofRefusal(t, b, err, RebuildUnknownProvenanceError{Count: 2, First: implements})
				require.Zero(t, stats)
				return
			}
			require.NoError(t, err)
			require.EqualValues(t, 2, stats.Edges)
			references := graph.EdgeIdentityFor(makeEdge(expect, actual.ID, graph.EdgeReferences))
			requireSyntheticCustody(t, b, map[graph.EdgeIdentity]rebuildEdgeProof{
				implements: rebuildProofKMPExpectActual,
				references: rebuildProofKMPExpectActual,
			})
		})
	}
}

func TestRebuildReactNativeNativePairingProvesExactDeclarationMetadata(t *testing.T) {
	for _, tc := range []struct {
		name   string
		key    string
		value  any
		refuse bool
	}{
		{"exact", "", nil, false},
		{"unrelated", "note", "changed", false},
		{"empty module", "rn_module", "", true},
		{"nil method", "rn_method", nil, true},
		{"typed method", "rn_method", []string{"now"}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ios := &graph.Node{ID: "ios", Kind: graph.KindMethod, Language: "swift", FilePath: "clock.swift",
				Meta: map[string]any{"rn_module": "Clock", "rn_method": "now"}}
			android := &graph.Node{ID: "android", Kind: graph.KindMethod, Language: "kotlin", FilePath: "clock.kt", Meta: maps.Clone(ios.Meta)}
			live := *android
			live.Meta = maps.Clone(android.Meta)
			if tc.refuse {
				delete(android.Meta, tc.key)
			}
			if tc.key != "" {
				live.Meta[tc.key] = tc.value
			}
			b, receipts := seedSyntheticCustody(t, producerProofFacts(ios), producerProofFacts(android))
			b.AddNode(&live)
			e := &graph.Edge{From: ios.ID, To: android.ID, Kind: graph.EdgeReferences, FilePath: ios.FilePath}
			if tc.refuse {
				// The join needs nonempty module and method names. Stage the demand
				// directly to test absence and zero values at the receipt boundary.
				p := rebuildRNPairPass{b: b, spillAt: 1}
				require.NoError(t, p.propose(e))
				err := p.prove(t.Context(), receipts)
				requireProducerProofRefusal(t, b, err, RebuildUnknownProvenanceError{Count: 1, First: graph.EdgeIdentityFor(e)})
				return
			}
			stats, err := b.RebuildReactNativeNativePairing(t.Context(), receipts, 1, func(language string) string {
				if language == "swift" {
					return "ios"
				}
				return "android"
			}, func(from, to *graph.Node, _, _ string) *graph.Edge {
				return &graph.Edge{From: from.ID, To: to.ID, Kind: graph.EdgeReferences, FilePath: from.FilePath}
			})
			require.NoError(t, err)
			require.EqualValues(t, 2, stats.Edges)
			reverse := &graph.Edge{From: android.ID, To: ios.ID, Kind: graph.EdgeReferences, FilePath: android.FilePath}
			requireSyntheticCustody(t, b, map[graph.EdgeIdentity]rebuildEdgeProof{
				graph.EdgeIdentityFor(e):       rebuildProofRNPair,
				graph.EdgeIdentityFor(reverse): rebuildProofRNPair,
			})
		})
	}
}

func TestRebuildSwiftObjCProvesExactDeclarationMetadata(t *testing.T) {
	for _, tc := range []struct {
		name   string
		live   map[string]any
		refuse bool
	}{
		{"exact", nil, false},
		{"unrelated", map[string]any{"note": "changed"}, false},
		{"empty selector", map[string]any{"objc_selector": ""}, true},
		{"typed setter", map[string]any{"objc_setter_selector": []string{"setClock:"}}, true},
		{"nil protocols", map[string]any{"objc_protocols": nil}, true},
		{"false objc", map[string]any{"objc": false}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			swift := &graph.Node{ID: "swift", Name: "clock", Kind: graph.KindMethod, Language: "swift", FilePath: "clock.swift"}
			objc := &graph.Node{ID: "objc", Name: "clock", Kind: graph.KindMethod, Language: "objc", FilePath: "clock.m"}
			b, receipts := seedSyntheticCustody(t, producerProofFacts(swift), producerProofFacts(objc))
			live := *swift
			live.Meta = tc.live
			b.AddNode(&live)
			e := &graph.Edge{From: swift.ID, To: objc.ID, Kind: graph.EdgeReferences, FilePath: swift.FilePath}
			stats, err := b.RebuildSwiftObjC(t.Context(), receipts, 1, func(_ RebuildView, propose func(*graph.Edge) error) error {
				return propose(e)
			})
			id := graph.EdgeIdentityFor(e)
			if tc.refuse {
				require.ErrorIs(t, err, ErrRebuildSwiftObjC)
				requireProducerProofRefusal(t, b, err, RebuildUnknownProvenanceError{Count: 1, First: id})
				require.Zero(t, stats)
				return
			}
			require.NoError(t, err)
			require.EqualValues(t, 1, stats.Edges)
			requireSyntheticCustody(t, b, map[graph.EdgeIdentity]rebuildEdgeProof{id: rebuildProofSwiftObjC})
		})
	}
}

func producerProofFacts(n *graph.Node) ParseFacts {
	return ParseFacts{EffectiveLanguage: n.Language, File: graph.FileMetaRow{FilePath: n.FilePath}, Nodes: []*graph.Node{n}}
}

func requireProducerProofRefusal(t *testing.T, b *BuildStore, err error, want RebuildUnknownProvenanceError) {
	t.Helper()
	var refused *RebuildUnknownProvenanceError
	require.ErrorAs(t, err, &refused)
	require.Equal(t, &want, refused)
	require.NoError(t, b.RebuildView().VisitEdges(t.Context(), func(e *graph.Edge) error {
		t.Fatalf("refusal installed edge %+v", e)
		return nil
	}))
	requireSyntheticCustody(t, b, nil)
}
