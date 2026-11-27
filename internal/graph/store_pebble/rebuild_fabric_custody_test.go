package store_pebble

import (
	"errors"
	"iter"
	"maps"
	"slices"
	"strings"
	"testing"

	"github.com/cockroachdb/pebble/v2"
	"github.com/stretchr/testify/require"
	"github.com/zzet/gortex/internal/graph"
)

func TestRebuildFabricStoreBindsNewCustodyAndPreservesIncumbent(t *testing.T) {
	b, facts := seedRebuildFabricStore(t)
	before := rebuildFrameworkCustodyRows(t, b)
	spec, first, second := facts[0].Nodes[0], facts[1].Nodes[0], facts[2].Nodes[0]
	incumbent := rebuildFabricStoreEdge(spec, first, "ColorView")
	incumbentKey := pendingEdgeKey(incumbent)
	rawKey := pendingEdgeKey(facts[0].Edges[0])
	require.Equal(t, rawKey, before[string(rebuildLinkRecorderKey(incumbentKey))])

	stats, err := b.RebuildFabric(t.Context(), sharedClaimsTestReceipts(facts...), rebuildFabricStoreCandidate, rebuildFabricStoreEdge)
	require.NoError(t, err)
	require.EqualValues(t, 3, stats.Edges)
	after := rebuildFrameworkCustodyRows(t, b)
	for _, want := range []*graph.Edge{
		rebuildFabricStoreEdge(first, spec, "ColorView"),
		rebuildFabricStoreEdge(second, spec, "ColorView"),
		rebuildFabricStoreEdge(spec, second, "ColorView"),
	} {
		key := pendingEdgeKey(want)
		live, err := b.pendingEdge(key)
		require.NoError(t, err)
		require.Equal(t, want, live)
		// The reader verifies the digest against the actual saved edge bytes.
		proof, err := readRebuildEdgeCustody(b.pending, key)
		require.NoError(t, err)
		require.Equal(t, rebuildEdgeCustody{proof: rebuildProofFabric}, proof)
		reverse := string(pendingReverseKey(want, key))
		require.Equal(t, key, after[reverse])
		delete(after, string(key))
		delete(after, reverse)
		delete(after, string(rebuildLinkRecorderKey(key)))
	}
	// Only the three new edges, reverse rows and proofs may differ. This
	// includes byte equality of the incumbent edge and its raw recorder.
	require.Equal(t, before, after)

	installed := rebuildFrameworkCustodyRows(t, b)
	unread := func(func(ParseFacts, error) bool) { t.Fatal("Fabric replay read receipts") }
	stats, err = b.RebuildFabric(t.Context(), unread, rebuildFabricStoreCandidate, rebuildFabricStoreEdge)
	require.NoError(t, err)
	require.Zero(t, stats)
	require.Equal(t, installed, rebuildFrameworkCustodyRows(t, b))
}

func TestRebuildFabricStorePreservesOccupiedPairWithoutProof(t *testing.T) {
	b, facts := seedRebuildFabricStore(t)
	spec, first, second := facts[0].Nodes[0], facts[1].Nodes[0], facts[2].Nodes[0]
	// The seed already has a raw-owned forward incumbent. Add its reverse
	// with a different payload so neither direction may be replaced.
	reverse := rebuildFabricStoreEdge(first, spec, "prior resolver")
	b.AddEdge(reverse)
	before := rebuildFrameworkCustodyRows(t, b)
	require.Contains(t, before, string(pendingEdgeKey(reverse)))

	// The live manager is unchanged. Its claim has the same join key, but
	// would not prove an endpoint of a new edge.
	bad := *first
	bad.StartLine++
	facts[1].Nodes = []*graph.Node{&bad}
	stats, err := b.RebuildFabric(t.Context(), sharedClaimsTestReceipts(facts...), rebuildFabricStoreCandidate, rebuildFabricStoreEdge)
	require.NoError(t, err)
	require.EqualValues(t, 2, stats.Edges)
	after := rebuildFrameworkCustodyRows(t, b)
	for _, want := range []*graph.Edge{
		rebuildFabricStoreEdge(spec, second, "ColorView"),
		rebuildFabricStoreEdge(second, spec, "ColorView"),
	} {
		key := pendingEdgeKey(want)
		live, err := b.pendingEdge(key)
		require.NoError(t, err)
		require.Equal(t, want, live)
		proof, err := readRebuildEdgeCustody(b.pending, key)
		require.NoError(t, err)
		require.Equal(t, rebuildEdgeCustody{proof: rebuildProofFabric}, proof)
		reverseKey := string(pendingReverseKey(want, key))
		require.Equal(t, key, after[reverseKey])
		delete(after, string(key))
		delete(after, reverseKey)
		delete(after, string(rebuildLinkRecorderKey(key)))
	}
	require.Equal(t, before, after, "both incumbents must stay byte-identical")
}

func TestRebuildFabricStoreRefusesUnprovedEndpointsBeforeWriting(t *testing.T) {
	spelling := func(f *ParseFacts, n *graph.Node) { n.Meta["fabric_component"] = "COLORVIEW" }
	for _, tc := range []struct {
		name   string
		damage func(*ParseFacts, *graph.Node)
		exact  string
	}{
		{"same normalized spelling", spelling, ""},
		{"kind", func(_ *ParseFacts, n *graph.Node) { n.Kind = graph.KindMethod }, ""},
		{"language", func(f *ParseFacts, n *graph.Node) { f.EffectiveLanguage, n.Language = "objc", "objc" }, ""},
		{"file", func(f *ParseFacts, n *graph.Node) {
			f.File.FilePath, n.FilePath = "elsewhere.java", "elsewhere.java"
		}, ""},
		{"start line", func(_ *ParseFacts, n *graph.Node) { n.StartLine++ }, ""},
		{"repo", func(_ *ParseFacts, n *graph.Node) { n.RepoPrefix = "other" }, ""},
		{"empty effective language", func(f *ParseFacts, _ *graph.Node) { f.EffectiveLanguage = "" }, ""},
		{"node file disagrees with receipt", func(f *ParseFacts, _ *graph.Node) { f.File.FilePath = "elsewhere.java" }, ""},
		{"exact before contradictory", spelling, "before"},
		{"exact after contradictory", spelling, "after"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			b, facts := seedRebuildFabricStore(t)
			bad := facts[2]
			n := *bad.Nodes[0]
			n.Meta = maps.Clone(n.Meta)
			bad.Nodes = []*graph.Node{&n}
			tc.damage(&bad, &n)
			presented := slices.Clone(facts)
			presented[2] = bad
			switch tc.exact {
			case "before":
				presented = slices.Insert(presented, 2, facts[2])
			case "after":
				presented = append(presented, facts[2])
			}

			before := rebuildFrameworkCustodyRows(t, b)
			stats, err := b.RebuildFabric(t.Context(), sharedClaimsTestReceipts(presented...), rebuildFabricStoreCandidate, rebuildFabricStoreEdge)
			require.ErrorIs(t, err, ErrRebuildFabric)
			require.ErrorIs(t, err, ErrRebuildUnknownProvenance)
			var refused *RebuildUnknownProvenanceError
			require.ErrorAs(t, err, &refused)
			want := rebuildFabricStoreEdge(facts[2].Nodes[0], facts[0].Nodes[0], "ColorView")
			require.Equal(t, &RebuildUnknownProvenanceError{Count: 2, First: graph.EdgeIdentityFor(want)}, refused)
			require.Zero(t, stats)
			// The valid first manager sorts before the unproved proposals. Even
			// its admissible edge must not acquire an edge row or recorder.
			require.Equal(t, before, rebuildFrameworkCustodyRows(t, b))
		})
	}
}

func TestRebuildFabricStoreLeavesRowsUnchangedWhenReceiptScanFails(t *testing.T) {
	b, facts := seedRebuildFabricStore(t)
	before := rebuildFrameworkCustodyRows(t, b)
	interrupted := errors.New("Fabric receipt read failed")
	receipts := func(yield func(ParseFacts, error) bool) {
		for _, f := range facts[:2] {
			if !yield(f, nil) {
				return
			}
		}
		yield(ParseFacts{}, interrupted)
	}

	stats, err := b.RebuildFabric(t.Context(), receipts, rebuildFabricStoreCandidate, rebuildFabricStoreEdge)
	require.ErrorIs(t, err, ErrRebuildFabric)
	require.ErrorIs(t, err, interrupted)
	require.Zero(t, stats)
	require.Equal(t, before, rebuildFrameworkCustodyRows(t, b))
}

func TestRebuildFabricStoreRefusesInvalidInputs(t *testing.T) {
	b, facts := seedRebuildFabricStore(t)
	receipts := sharedClaimsTestReceipts(facts...)
	before := rebuildFrameworkCustodyRows(t, b)
	for _, tc := range []struct {
		name      string
		receipts  iter.Seq2[ParseFacts, error]
		candidate rebuildFabricCandidate
		makeEdge  func(*graph.Node, *graph.Node, string) *graph.Edge
	}{
		{"nil receipts", nil, rebuildFabricStoreCandidate, rebuildFabricStoreEdge},
		{"nil candidate", receipts, nil, rebuildFabricStoreEdge},
		{"nil edge maker", receipts, rebuildFabricStoreCandidate, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			stats, err := b.RebuildFabric(t.Context(), tc.receipts, tc.candidate, tc.makeEdge)
			require.ErrorIs(t, err, ErrRebuildFabric)
			require.ErrorIs(t, err, ErrRebuildLinkInputSize)
			require.Zero(t, stats)
			require.Equal(t, before, rebuildFrameworkCustodyRows(t, b))
		})
	}
}

func TestRebuildFabricStorePreservesOccupiedScratch(t *testing.T) {
	b, facts := seedRebuildFabricStore(t)
	key := rebuildFabricKey('s', facts[0].Nodes[0].ID)
	require.NoError(t, b.pending.Set(key, []byte("existing Fabric scratch"), pebble.NoSync))
	before := rebuildFrameworkCustodyRows(t, b)
	stats, err := b.RebuildFabric(t.Context(), sharedClaimsTestReceipts(facts...), rebuildFabricStoreCandidate, rebuildFabricStoreEdge)
	require.ErrorIs(t, err, ErrRebuildFabric)
	require.ErrorIs(t, err, errInvalidRecord)
	require.Zero(t, stats)
	require.Equal(t, before, rebuildFrameworkCustodyRows(t, b))
}

func TestRebuildFabricStoreRefusesOversizedNode(t *testing.T) {
	spec := &graph.Node{ID: "spec.ts::view", Kind: graph.KindType, FilePath: "spec.ts", Language: "typescript",
		Meta: map[string]any{"fabric_component": "View"}}
	manager := &graph.Node{ID: "view.java::manager", Kind: graph.KindType, FilePath: "view.java", Language: "java",
		Meta: map[string]any{"fabric_component": "view", "fabric_native": "java", "padding": strings.Repeat("x", 16<<20)}}
	b, receipts := seedSyntheticCustody(t, producerProofFacts(spec), producerProofFacts(manager))
	key := pendingNodeKey(manager.ID)
	rowBytes, err := pendingGet(b.pending, key, func(value []byte) (int, error) { return len(key) + len(value), nil })
	require.NoError(t, err)
	require.Greater(t, rowBytes, 16<<20)

	stats, err := b.RebuildFabric(t.Context(), receipts, rebuildFabricStoreCandidate, rebuildFabricStoreEdge)
	require.ErrorIs(t, err, ErrRebuildFabric)
	require.ErrorIs(t, err, ErrRebuildLinkInputSize)
	require.Zero(t, stats)
	require.NoError(t, b.RebuildView().VisitEdges(t.Context(), func(e *graph.Edge) error {
		t.Fatalf("oversized node installed edge %+v", e)
		return nil
	}))
	requireSyntheticCustody(t, b, nil)
	occupied, err := rebuildGuardHasPrefix(t.Context(), b.pending, rebuildFabricPrefix)
	require.NoError(t, err)
	require.False(t, occupied)
}

func seedRebuildFabricStore(t *testing.T) (*BuildStore, []ParseFacts) {
	t.Helper()
	spec := &graph.Node{ID: "spec.ts::ColorView", Kind: graph.KindType, FilePath: "spec.ts", Language: "typescript", StartLine: 2,
		Meta: map[string]any{"fabric_component": "ColorView"}}
	first := &graph.Node{ID: "a.java::manager", Kind: graph.KindType, FilePath: "a.java", Language: "java", StartLine: 3,
		Meta: map[string]any{"fabric_component": "colorview", "fabric_native": "java"}}
	second := &graph.Node{ID: "b.java::manager", Kind: graph.KindType, FilePath: "b.java", Language: "java", StartLine: 4,
		Meta: map[string]any{"fabric_component": "colorview", "fabric_native": "java"}}
	incumbent := rebuildFabricStoreEdge(spec, first, "ColorView")
	incumbent.Via = "prior linker"
	raw := *incumbent
	raw.To = "unresolved::ColorView"
	facts := []ParseFacts{producerProofFacts(spec), producerProofFacts(first), producerProofFacts(second)}
	facts[0].Edges = []*graph.Edge{&raw}
	b := newBuild(t, FullProfile)
	for i := range facts {
		facts[i].File.NodeCount = 1
		require.NoError(t, b.AddParsedFile("", facts[i]))
	}
	// Install the incumbent with actual receipt lineage, not an injected proof.
	require.NoError(t, b.ApplyLinkReindexes(t.Context(), []graph.EdgeReindex{{Edge: incumbent, OldTo: raw.To}}))
	return b, facts
}

func rebuildFabricStoreCandidate(n *graph.Node) (string, string, bool) {
	component, _ := n.Meta["fabric_component"].(string)
	_, native := n.Meta["fabric_native"]
	// Only case-folding is needed for these fixtures. The resolver's native
	// marker interpretation and edge payload are covered by its SQLite oracle.
	return component, strings.ToLower(component), native
}

func rebuildFabricStoreEdge(from, to *graph.Node, component string) *graph.Edge {
	return &graph.Edge{From: from.ID, To: to.ID, Kind: graph.EdgeReferences, FilePath: from.FilePath, Line: from.StartLine,
		Via: component, Meta: map[string]any{"to_language": to.Language}}
}
