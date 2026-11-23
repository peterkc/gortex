package indexer

import (
	"errors"
	"maps"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/zzet/gortex/internal/clones"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/graph/store_pebble"
)

func TestRebuildCloneRelationsNativeParity(t *testing.T) {
	const threshold = 0.82
	facts := rebuildCloneRelationFixture(t)
	var before, nativeNodes []*graph.Node
	oracle := graph.New()
	for _, f := range facts {
		for _, n := range f.Nodes {
			before = append(before, n)
			oracleNode := *n
			oracleNode.Meta = maps.Clone(n.Meta)
			nativeNodes = append(nativeNodes, &oracleNode)
		}
	}
	oracle.AddBatch(nativeNodes, nil)
	native, baseline := detectClonesAndEmitEdgesWithBaselineCtx(t.Context(), oracle, "", threshold)
	require.Equal(t, 1, baseline.corpus)
	require.Equal(t, 4, native.Items)
	require.Equal(t, 2, native.Pairs)
	require.Equal(t, 4, native.Edges)
	require.Equal(t, 1, native.DiffusedPairs)
	require.Equal(t, 2, native.DiffusedEdges)
	wantEdges := oracle.AllEdges()
	require.Len(t, wantEdges, 6)
	wantStamp := oracle.GetNode(before[0].ID)
	require.NotContains(t, wantStamp.Meta, cloneShinglesMetaKey)
	require.Contains(t, wantStamp.Meta, cloneSigMetaKey)
	wantStats := rebuildCloneRelationStats{
		Signatures: rebuildCloneSignatureCounts{Corpus: 1, Kept: 1, Signed: 3},
		Direct: store_pebble.RebuildClonePairStats{
			Pairs:              uint64(native.Pairs),
			SkippedBuckets:     uint64(native.SkippedBuckets),
			SkippedBucketItems: uint64(native.SkippedBucketItems),
		},
		Diffused: native.DiffusedPairs,
	}

	for _, tc := range []struct {
		name         string
		edgesOnError int
	}{
		{name: "success"},
		{name: "receipt_error"},
		{name: "node_error"},
		{name: "direct_error", edgesOnError: 2},
		{name: "semantic_error", edgesOnError: 5},
	} {
		t.Run(tc.name, func(t *testing.T) {
			build, _ := openRebuildTestFileStore(t)
			receipts := seedRebuildCloneInputReceipts(t, build, facts...)
			assertUnchanged := func() {
				t.Helper()
				requireRebuildTestFileRows(t, build, before, nil)
				shingles := make(map[string][]uint64)
				require.NoError(t, build.RebuildView().VisitCloneShingles(t.Context(), "", func(id string, values []uint64) error {
					shingles[id] = values
					return nil
				}))
				require.Equal(t, facts[0].Shingles, shingles)
			}
			failure := errors.New("planted clone relations failure")
			var stamps []*graph.Node
			var edges []*graph.Edge
			passes := 0
			visit := func(failAt string) (rebuildCloneRelationStats, error) {
				stamps, edges, passes = nil, nil, 0
				replay := func(yield func(store_pebble.ParseFacts, error) bool) {
					passes++
					for f, err := range receipts {
						// Fail after the first raw item has entered the second pass.
						if failAt == "receipt_error" && passes == 2 && err == nil && f.File.FilePath != facts[0].File.FilePath {
							yield(store_pebble.ParseFacts{}, failure)
							return
						}
						if !yield(f, err) {
							return
						}
					}
				}
				return visitRebuildCloneRelations(t.Context(), build, replay, threshold, func(n *graph.Node) error {
					require.Empty(t, edges, "node stamps must precede edges")
					stamps = append(stamps, n)
					if failAt == "node_error" {
						return failure
					}
					return nil
				}, func(e *graph.Edge) error {
					wantKind := graph.EdgeSimilarTo
					if len(edges) >= native.Edges {
						wantKind = graph.EdgeSemanticallyRelated
					}
					require.Equal(t, wantKind, e.Kind, "direct rows must precede semantic rows")
					edges = append(edges, e)
					if failAt == tc.name && len(edges) == tc.edgesOnError {
						return failure
					}
					return nil
				})
			}
			if tc.name != "success" {
				stats, err := visit(tc.name)
				require.ErrorIs(t, err, failure)
				require.Zero(t, stats)
				if tc.name != "receipt_error" {
					require.Len(t, stamps, 1)
				}
				require.Len(t, edges, tc.edgesOnError)
				assertUnchanged()
			}
			// Retry on the same build: leftover pair or diffusion scratch refuses entry.
			stats, err := visit("")
			require.NoError(t, err)
			require.Equal(t, wantStats, stats)
			require.Equal(t, 2, passes)
			require.Equal(t, []*graph.Node{wantStamp}, stamps)
			require.ElementsMatch(t, wantEdges, edges, "complete directed native edge rows")
			assertUnchanged()
		})
	}
}

func rebuildCloneRelationFixture(t *testing.T) []store_pebble.ParseFacts {
	t.Helper()
	shingles := []uint64{1, 3, 5, 7, 11, 13, 17, 19}
	// Share the hub's signature but keep the raw item in a disjoint token
	// class (40 vs. 500). Losing either token count adds spurious pairs.
	raw := rebuildCloneInputFixture(rebuildCloneInput{
		ID: "a.go::F", FilePath: "a.go", Language: "go", Line: 3, Tokens: 40, Shingles: shingles,
	}, "go")
	hub, ok := clones.SignatureFromShingles(shingles, 0)
	require.True(t, ok)
	tsLeaf, rustLeaf := hub, hub
	// The hub shares 56/64 slots with each leaf. The leaves share only
	// 48/64 with each other, so their relation must come from diffusion.
	for i := range 8 {
		tsLeaf[i] ^= 1
		rustLeaf[i+8] ^= 1
	}
	facts := []store_pebble.ParseFacts{raw}
	for _, item := range []struct {
		path, lang string
		kind       graph.NodeKind
		line       int
		sig        clones.Signature
	}{
		{path: "b.go", lang: "go", kind: graph.KindFunction, line: 5, sig: hub},
		{path: "c.ts", lang: "typescript", kind: graph.KindFunction, line: 7, sig: tsLeaf},
		{path: "d.rs", lang: "rust", kind: graph.KindMethod, line: 9, sig: rustLeaf},
	} {
		facts = append(facts, store_pebble.ParseFacts{
			EffectiveLanguage: item.lang,
			File:              graph.FileMetaRow{FilePath: item.path, NodeCount: 1},
			Nodes: []*graph.Node{{
				ID: item.path + "::F", Name: "F", FilePath: item.path, Language: item.lang,
				Kind: item.kind, StartLine: item.line, EndLine: item.line,
				Meta: map[string]any{cloneSigMetaKey: clones.EncodeSignature(item.sig), cloneTokensMetaKey: 500},
			}},
		})
	}
	return facts
}
