package store_pebble

import (
	"encoding/binary"
	"iter"
	"strings"
	"testing"

	"github.com/cockroachdb/pebble/v2"
	"github.com/stretchr/testify/require"
	"github.com/zzet/gortex/internal/graph"
)

func TestRebuildKMPExpectActualMuxAndCustody(t *testing.T) {
	makeNode := func(id, file, name, lang, role string) *graph.Node {
		return &graph.Node{ID: id, Name: name, Kind: graph.KindFunction, FilePath: file, Language: lang,
			StartLine: 3, Meta: map[string]any{"kmp_role": role}}
	}
	expect := makeNode("common.go::clock", "common.go", "clock", "go", "expect")
	protected := makeNode("common.go::clock#param:p", "common.go", "clock", "go", "expect")
	actual := makeNode("actual.java::clock", "actual.java", "clock", "java", "actual")
	makeEdge := func(n *graph.Node, to string, kind graph.EdgeKind) *graph.Edge {
		return &graph.Edge{From: n.ID, To: to, Kind: kind, FilePath: n.FilePath, Line: n.StartLine,
			Origin: graph.OriginASTInferred}
	}
	stationary := makeEdge(expect, actual.ID, graph.EdgeReferences)
	stationary.Origin = graph.OriginASTResolved
	b, receipts := seedSyntheticCustody(t,
		ParseFacts{EffectiveLanguage: "go", File: graph.FileMetaRow{FilePath: expect.FilePath}, Nodes: []*graph.Node{expect, protected},
			Edges: []*graph.Edge{stationary}},
		ParseFacts{EffectiveLanguage: "java", File: graph.FileMetaRow{FilePath: actual.FilePath}, Nodes: []*graph.Node{actual}})
	ctx := t.Context()
	stats, err := b.RebuildKMPExpectActual(ctx, receipts, makeEdge)
	require.NoError(t, err)
	require.Zero(t, stats)

	// A live Kotlin marker with no receipt opens the census but cannot
	// authorize new rows: the pass refuses and writes nothing.
	marker := &graph.Node{ID: "marker.kt", Kind: graph.KindFile, FilePath: "marker.kt", Language: " Kotlin "}
	b.AddNode(marker)
	stats, err = b.RebuildKMPExpectActual(ctx, receipts, makeEdge)
	var refused *RebuildUnknownProvenanceError
	require.ErrorAs(t, err, &refused)
	first := graph.EdgeIdentityFor(makeEdge(actual, expect.ID, graph.EdgeImplements))
	require.Equal(t, &RebuildUnknownProvenanceError{Count: 2, First: first}, refused)
	require.Zero(t, stats)
	unproved, err := b.pendingEdge(pendingEdgeKey(makeEdge(actual, expect.ID, graph.EdgeImplements)))
	require.NoError(t, err)
	require.Nil(t, unproved)

	// With the marker proved by its receipt, the pass installs both rows.
	// The marker isn't itself a participating declaration or a proof endpoint.
	marker = &graph.Node{ID: "marker.kt", Kind: graph.KindFile, FilePath: "marker.kt", Language: "kotlin"}
	b, receipts = seedSyntheticCustody(t,
		ParseFacts{EffectiveLanguage: "go", File: graph.FileMetaRow{FilePath: expect.FilePath}, Nodes: []*graph.Node{expect, protected},
			Edges: []*graph.Edge{stationary}},
		ParseFacts{EffectiveLanguage: "java", File: graph.FileMetaRow{FilePath: actual.FilePath}, Nodes: []*graph.Node{actual}},
		ParseFacts{EffectiveLanguage: "kotlin", File: graph.FileMetaRow{FilePath: marker.FilePath}, Nodes: []*graph.Node{marker}})
	// The oversized Kotlin row cannot pair, and the small marker already
	// proves admission. Its metadata must not limit marker discovery.
	b.AddNode(&graph.Node{ID: "decoy.kt", Kind: graph.KindFile, FilePath: "decoy.kt", Language: "kotlin",
		Meta: map[string]any{"padding": strings.Repeat("x", rebuildKMPRowLimit)}})
	stats, err = b.RebuildKMPExpectActual(ctx, receipts, makeEdge)
	require.NoError(t, err)
	require.EqualValues(t, 2, stats.Edges)
	live, err := b.pendingEdge(pendingEdgeKey(stationary))
	require.NoError(t, err)
	require.Equal(t, stationary, live)
	dropped, err := b.pendingEdge(pendingEdgeKey(makeEdge(actual, protected.ID, graph.EdgeImplements)))
	require.NoError(t, err)
	require.Nil(t, dropped)
	requireSyntheticCustody(t, b, map[graph.EdgeIdentity]rebuildEdgeProof{
		graph.EdgeIdentityFor(makeEdge(actual, expect.ID, graph.EdgeImplements)):    rebuildProofKMPExpectActual,
		graph.EdgeIdentityFor(makeEdge(protected, actual.ID, graph.EdgeReferences)): rebuildProofKMPExpectActual,
	})
}

func TestRebuildKMPExpectActualRejectsInputsAndOccupiedScratch(t *testing.T) {
	b := newBuild(t, FullProfile)
	ctx := t.Context()
	receipts := func(func(ParseFacts, error) bool) { t.Fatal("must not read receipts") }
	makeEdge := func(*graph.Node, string, graph.EdgeKind) *graph.Edge {
		t.Fatal("must not propose edges")
		return nil
	}
	for _, tc := range []struct {
		name     string
		receipts iter.Seq2[ParseFacts, error]
		makeEdge func(*graph.Node, string, graph.EdgeKind) *graph.Edge
	}{
		{"nil receipts", nil, makeEdge},
		{"nil edge builder", receipts, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			stats, err := b.RebuildKMPExpectActual(t.Context(), tc.receipts, tc.makeEdge)
			require.ErrorIs(t, err, ErrRebuildKMPExpectActual)
			require.ErrorIs(t, err, ErrRebuildLinkInputSize)
			require.Zero(t, stats)
		})
	}
	key, value := rebuildKMPKey('w'), []byte("occupied")
	require.NoError(t, b.pending.Set(key, value, pebble.NoSync))
	stats, err := b.RebuildKMPExpectActual(ctx, receipts, makeEdge)
	require.ErrorIs(t, err, ErrRebuildKMPExpectActual)
	require.ErrorIs(t, err, errInvalidRecord)
	require.ErrorContains(t, err, "scratch range occupied")
	require.Zero(t, stats)
	got, err := pendingGet(b.pending, key, copyValue)
	require.NoError(t, err)
	require.Equal(t, value, got)
}

func TestRebuildKMPExpectActualLightScanAndCandidateRowLimit(t *testing.T) {
	ctx := t.Context()
	b := newBuild(t, FullProfile)
	b.AddNode(&graph.Node{ID: "a-marker", Language: " Kotlin ", Kind: graph.KindFile})
	receipts := func(func(ParseFacts, error) bool) { t.Fatal("must not read receipts") }
	makeEdge := func(*graph.Node, string, graph.EdgeKind) *graph.Edge {
		t.Fatal("must not propose edges")
		return nil
	}
	key := pendingNodeKey("z-row")
	n := &graph.Node{ID: "different", Name: "clock", Kind: graph.KindVariable, Language: "go"}
	raw, err := encodeNode(n)
	require.NoError(t, err)
	require.NoError(t, b.pending.Set(key, raw, pebble.NoSync))
	found, err := rebuildKMPHasKotlin(ctx, b.RebuildView())
	require.NoError(t, err) // The probe must stop before the bad row.
	require.True(t, found)
	stats, err := b.RebuildKMPExpectActual(ctx, receipts, makeEdge)
	require.ErrorIs(t, err, ErrRebuildKMPExpectActual)
	require.ErrorIs(t, err, errInvalidRecord)
	require.ErrorContains(t, err, "KMP expect/actual node identity")
	require.Zero(t, stats)

	n.ID = "z-row"
	n.Meta = map[string]any{"kmp_role": "actual", "pad": strings.Repeat("x", rebuildKMPRowLimit)}
	raw, err = encodeNode(n)
	require.NoError(t, err)
	require.NoError(t, b.pending.Set(key, raw, pebble.NoSync))
	stats, err = b.RebuildKMPExpectActual(ctx, receipts, makeEdge)
	require.NoError(t, err) // The cap does not cover excluded kinds.
	require.Zero(t, stats)

	n.Kind = graph.KindFunction
	delete(n.Meta, "kmp_role")
	raw, err = encodeNode(n)
	require.NoError(t, err)
	require.NoError(t, b.pending.Set(key, raw, pebble.NoSync))
	stats, err = b.RebuildKMPExpectActual(ctx, receipts, makeEdge)
	require.NoError(t, err) // A named function without a role is not a candidate.
	require.Zero(t, stats)

	n.Meta["kmp_role"] = "actual"
	raw, err = encodeNode(n)
	require.NoError(t, err)
	require.NoError(t, b.pending.Set(key, raw, pebble.NoSync))
	stats, err = b.RebuildKMPExpectActual(ctx, receipts, makeEdge)
	require.ErrorIs(t, err, ErrRebuildKMPExpectActual)
	require.ErrorIs(t, err, ErrRebuildLinkInputSize)
	require.Zero(t, stats)
}

func TestRebuildKMPExpectActualScratchGuards(t *testing.T) {
	b := newBuild(t, FullProfile)
	p := rebuildKMPPass{b: b}
	n := &graph.Node{ID: "expect", Kind: graph.KindFunction, Name: "clock", FilePath: "common.go",
		Meta: map[string]any{"kmp_role": "expect"}}
	b.AddNode(n)
	proof := rebuildKMPKey('n', n.ID)
	require.NoError(t, p.put(proof, []byte{rebuildCapabilityWanted}))
	require.NoError(t, p.noteReceipt(n, n.FilePath, ""))
	state, err := p.state(proof)
	require.NoError(t, err)
	require.Equal(t, byte(rebuildCapabilityWanted|rebuildCapabilityUnproved), state)
	require.NoError(t, p.put(proof, []byte{rebuildCapabilityWanted, 0}))
	_, err = p.state(proof)
	require.ErrorIs(t, err, errInvalidRecord)

	marker := rebuildKMPKey('k', "marker")
	require.NoError(t, p.put(marker, []byte{rebuildCapabilityWanted, 0}))
	err = p.prove(t.Context(), func(func(ParseFacts, error) bool) {})
	require.ErrorIs(t, err, errInvalidRecord)
	require.ErrorContains(t, err, "KMP Kotlin marker proof state")

	proxy := graph.ProxyNodeID("other", "remote-declaration")
	for _, e := range []*graph.Edge{
		{From: proxy, To: "live", Kind: graph.EdgeReferences},
		{From: "live", To: proxy, Kind: graph.EdgeReferences},
	} {
		require.NoError(t, p.propose(e))
	}
	require.Zero(t, p.proposals)
	require.ErrorIs(t, p.put(rebuildKMPKey('w'), make([]byte, rebuildKMPRowLimit)), ErrRebuildLinkInputSize)

	e := &graph.Edge{From: "actual", To: n.ID, Kind: graph.EdgeImplements}
	raw, err := encodeEdge(e)
	require.NoError(t, err)
	wrongKey := binary.BigEndian.AppendUint64(rebuildKMPKey('w'), 0)
	wrongKey = append(wrongKey, pendingEdgeKey(&graph.Edge{From: "other", To: n.ID, Kind: e.Kind})...)
	require.NoError(t, p.put(wrongKey, raw))
	err = p.visitProposals(t.Context(), func(*graph.Edge) error {
		t.Fatal("must not yield a mismatched proposal")
		return nil
	})
	require.ErrorIs(t, err, errInvalidRecord)
	require.ErrorContains(t, err, "KMP expect/actual proposal key")
}
