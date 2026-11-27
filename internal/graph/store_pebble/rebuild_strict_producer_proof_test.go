package store_pebble

import (
	"maps"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/zzet/gortex/internal/graph"
)

func TestRebuildKMPExpectActualProvesNativeRole(t *testing.T) {
	for _, tc := range []struct {
		name       string
		marker     map[string]any
		changeRole bool
	}{
		{name: "exact"},
		{name: "empty marker role", marker: map[string]any{"kmp_role": ""}},
		{name: "nil marker role", marker: map[string]any{"kmp_role": nil}},
		{name: "typed marker role", marker: map[string]any{"kmp_role": []string{"expect"}}},
		{name: "changed marker role", marker: map[string]any{"kmp_role": "expect"}},
		{name: "changed endpoint role", changeRole: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			expect := &graph.Node{ID: "expect", Name: "Clock", Kind: graph.KindFunction, Language: "go", FilePath: "common.go",
				Meta: map[string]any{"kmp_role": "expect"}}
			actual := &graph.Node{ID: "actual", Name: "Clock", Kind: graph.KindFunction, Language: "java", FilePath: "platform.java",
				Meta: map[string]any{"kmp_role": "actual"}}
			// Neither declaration can also prove the sole Kotlin marker.
			marker := &graph.Node{ID: "marker.kt", Kind: graph.KindFile, Language: "kotlin", FilePath: "marker.kt"}
			b, receipts := seedSyntheticCustody(t, producerProofFacts(expect), producerProofFacts(actual), producerProofFacts(marker))
			live := *marker
			live.Meta = tc.marker
			b.AddNode(&live)
			if tc.changeRole {
				changed := *expect
				changed.Meta = map[string]any{"kmp_role": "actual"}
				receipts = func(yield func(ParseFacts, error) bool) {
					for _, n := range []*graph.Node{&changed, actual, marker} {
						if !yield(producerProofFacts(n), nil) {
							return
						}
					}
				}
			}
			makeEdge := func(n *graph.Node, to string, kind graph.EdgeKind) *graph.Edge {
				return &graph.Edge{From: n.ID, To: to, Kind: kind, FilePath: n.FilePath, Line: n.StartLine}
			}
			stats, err := b.RebuildKMPExpectActual(t.Context(), receipts, makeEdge)
			implements := graph.EdgeIdentityFor(makeEdge(actual, expect.ID, graph.EdgeImplements))
			if tc.changeRole {
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

func TestRebuildReactNativeNativePairingProvesNativeMetadata(t *testing.T) {
	for _, tc := range []struct {
		name, key string
		value     any
		zero      bool
		refuse    bool
	}{
		{name: "exact"},
		{"unrelated", "note", "changed", false, false},
		{"empty module", "rn_module", "", true, false},
		{"nil method", "rn_method", nil, true, false},
		{"typed method", "rn_method", []string{"now"}, true, false},
		{"changed module", "rn_module", "Calendar", false, true},
		{"changed method", "rn_method", "later", false, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ios := &graph.Node{ID: "ios", Kind: graph.KindMethod, Language: "swift", FilePath: "clock.swift",
				Meta: map[string]any{"rn_module": "Clock", "rn_method": "now"}}
			android := &graph.Node{ID: "android", Kind: graph.KindMethod, Language: "kotlin", FilePath: "clock.kt", Meta: maps.Clone(ios.Meta)}
			live := *android
			live.Meta = maps.Clone(android.Meta)
			if tc.zero {
				delete(android.Meta, tc.key)
			}
			if tc.key != "" {
				live.Meta[tc.key] = tc.value
			}
			b, receipts := seedSyntheticCustody(t, producerProofFacts(ios), producerProofFacts(android))
			b.AddNode(&live)
			e := &graph.Edge{From: ios.ID, To: android.ID, Kind: graph.EdgeReferences, FilePath: ios.FilePath}
			if tc.zero || tc.refuse {
				// A zero or changed join key cannot elect a pair. Stage the
				// demand directly to isolate receipt proof from pair selection.
				before := rebuildFrameworkCustodyRows(t, b)
				p := rebuildRNPairPass{b: b}
				require.NoError(t, p.propose(e))
				err := p.prove(t.Context(), receipts)
				if tc.refuse {
					requireProducerProofRefusal(t, b, err, RebuildUnknownProvenanceError{Count: 1, First: graph.EdgeIdentityFor(e)})
					after := rebuildFrameworkCustodyRows(t, b)
					// The public stage, not the direct proof call, clears scratch.
					maps.DeleteFunc(after, func(key string, _ []byte) bool {
						return strings.HasPrefix(key, string(rebuildRNPairPrefix))
					})
					require.Equal(t, before, after)
				} else {
					require.NoError(t, err)
				}
				return
			}
			stats, err := b.RebuildReactNativeNativePairing(t.Context(), receipts, func(language string) string {
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

func TestRebuildSwiftObjCProvesNativeMetadata(t *testing.T) {
	for _, tc := range []struct {
		name   string
		kind   graph.NodeKind
		objc   bool
		live   map[string]any
		refuse bool
	}{
		{name: "exact"},
		{name: "unrelated", live: map[string]any{"note": "changed"}},
		{name: "empty selector", live: map[string]any{"objc_selector": ""}},
		{name: "typed setter", live: map[string]any{"objc_setter_selector": []string{"setClock:"}}},
		{name: "nil protocols", live: map[string]any{"objc_protocols": nil}},
		{name: "false objc", kind: graph.KindInterface, live: map[string]any{"objc": false}},
		{name: "changed selector", live: map[string]any{"objc_selector": "clock"}, refuse: true},
		{name: "changed setter", kind: graph.KindField, live: map[string]any{"objc_setter_selector": "setClock:"}, refuse: true},
		{name: "Swift interface objc bit", kind: graph.KindInterface, live: map[string]any{"objc": true}, refuse: true},
		{name: "Swift method ignores protocols and objc bit", live: map[string]any{"objc_protocols": "Clock", "objc": true}},
		{name: "ObjC method ignores Swift selectors and protocols", objc: true, live: map[string]any{
			"objc_selector": "clock", "objc_setter_selector": "setClock:", "objc_protocols": "Clock",
		}},
		{name: "ObjC class protocols", objc: true, kind: graph.KindType, live: map[string]any{"objc_protocols": "Clock"}, refuse: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			swift := &graph.Node{ID: "swift", Name: "clock", Kind: graph.KindMethod, Language: "swift", FilePath: "clock.swift"}
			objc := &graph.Node{ID: "objc", Name: "clock", Kind: graph.KindMethod, Language: "objc", FilePath: "clock.m"}
			changed := swift
			if tc.objc {
				changed = objc
			}
			if tc.kind != "" {
				changed.Kind = tc.kind
			}
			b, receipts := seedSyntheticCustody(t, producerProofFacts(swift), producerProofFacts(objc))
			live := *changed
			live.Meta = tc.live
			b.AddNode(&live)
			before := rebuildFrameworkCustodyRows(t, b)
			e := &graph.Edge{From: swift.ID, To: objc.ID, Kind: graph.EdgeReferences, FilePath: swift.FilePath}
			stats, err := b.RebuildSwiftObjC(t.Context(), receipts, func(_ RebuildView, propose func(*graph.Edge) error) error {
				return propose(e)
			})
			id := graph.EdgeIdentityFor(e)
			if tc.refuse {
				require.ErrorIs(t, err, ErrRebuildSwiftObjC)
				requireProducerProofRefusal(t, b, err, RebuildUnknownProvenanceError{Count: 1, First: id})
				require.Zero(t, stats)
				require.Equal(t, before, rebuildFrameworkCustodyRows(t, b))
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
