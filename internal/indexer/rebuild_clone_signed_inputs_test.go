package indexer

import (
	"maps"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/zzet/gortex/internal/clones"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/graph/store_pebble"
)

func TestRebuildSignedCloneInputsNativeParity(t *testing.T) {
	shingles := []uint64{1, 3, 5, 7, 11, 13, 17, 19}
	sig, ok := clones.SignatureFromShingles(shingles, 0)
	require.True(t, ok)
	stamp := clones.EncodeSignature(sig)

	// The two raw bodies are below this floor. If signed-only rows count
	// toward it, the repeated hashes are filtered and both raw bodies drop.
	previous := cmsMinCorpus
	t.Cleanup(func() { cmsMinCorpus = previous })
	cmsMinCorpus = 3
	for _, tc := range []struct {
		name       string
		raw, pairs int
	}{
		{name: "no_raw_bodies", pairs: 1},
		{name: "mixed", raw: 2, pairs: 3},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var facts []store_pebble.ParseFacts
			if tc.raw != 0 {
				for _, row := range []rebuildCloneInput{
					{ID: "a.go::F", FilePath: "a.go", Language: "go", Line: 3, Tokens: 500, Shingles: shingles},
					// Same signature, but disjoint length classes. Losing the
					// numeric token count would let the signed rows match it.
					{ID: "b.go::F", FilePath: "b.go", Language: "go", Line: 7, Tokens: 40, Shingles: shingles},
				} {
					f := rebuildCloneInputFixture(row, "go")
					f.Nodes[0].Meta[cloneSigMetaKey] = clones.EncodeSignature(clones.Signature{1})
					facts = append(facts, f)
				}
			}
			facts = append(facts, rebuildSignedCloneFacts(stamp))
			build, _ := openRebuildTestFileStore(t)
			receipts := seedRebuildCloneInputReceipts(t, build, facts...)
			view := build.RebuildView()

			oracle := graph.New()
			var before, nativeNodes []*graph.Node
			for _, f := range facts {
				for _, n := range f.Nodes {
					nativeNode := *n
					nativeNode.Meta = maps.Clone(n.Meta)
					nativeNodes = append(nativeNodes, &nativeNode)
					before = append(before, n)
				}
			}
			oracle.AddBatch(nativeNodes, nil)
			// The method's live count can change shape without changing its value.
			method := *before[len(before)-1]
			method.Meta = maps.Clone(method.Meta)
			method.Meta[cloneTokensMetaKey] = 500
			build.AddNode(&method)
			before[len(before)-1] = &method
			page, err := oracle.CloneCorpusPage("", "", 10)
			require.NoError(t, err)
			require.Empty(t, page, "native cold Graph starts without a clone sidecar")
			const threshold = 0.82
			native, baseline := detectClonesAndEmitEdgesWithBaselineCtx(t.Context(), oracle, "", threshold)
			require.Equal(t, tc.raw, baseline.corpus)
			require.Equal(t, len(before), native.Items)
			require.Equal(t, tc.pairs, native.Pairs)
			require.Equal(t, 2*tc.pairs, native.Edges)
			require.Zero(t, native.DiffusedEdges)

			var edges []*graph.Edge
			stats, err := visitRebuildDirectCloneEdges(t.Context(), build, receipts, threshold, func(e *graph.Edge) error {
				edges = append(edges, e)
				return nil
			})
			require.NoError(t, err)
			require.Equal(t, store_pebble.RebuildClonePairStats{Pairs: uint64(tc.pairs)}, stats)
			require.ElementsMatch(t, oracle.AllEdges(), edges, "full directed native edge rows")

			var stamps []*graph.Node
			counts, err := visitRebuildCloneNodeStamps(t.Context(), build, receipts, func(n *graph.Node) error {
				stamps = append(stamps, n)
				return nil
			})
			require.NoError(t, err)
			require.Equal(t, tc.raw, counts.Corpus)
			require.Equal(t, tc.raw, counts.Kept)
			require.Len(t, stamps, tc.raw)
			var wantStamps []*graph.Node
			wantShingles := make(map[string][]uint64)
			for _, f := range facts {
				for id, values := range f.Shingles {
					wantShingles[id] = values
					wantStamps = append(wantStamps, oracle.GetNode(id))
				}
			}
			require.ElementsMatch(t, wantStamps, stamps, "only raw bodies are stamped, even when they already have a signature")
			gotShingles := make(map[string][]uint64)
			require.NoError(t, view.VisitCloneShingles(t.Context(), "", func(id string, values []uint64) error {
				gotShingles[id] = values
				return nil
			}))
			require.Equal(t, wantShingles, gotShingles, "the working sidecar retains only raw bodies")
			requireRebuildTestFileRows(t, build, before, nil)
		})
	}
}

func TestRebuildSignedCloneInputsRejectLiveDrift(t *testing.T) {
	for _, name := range []string{"stamp", "tokens", "typed_raw_metadata", "sidecar"} {
		t.Run(name, func(t *testing.T) {
			facts := rebuildSignedCloneFacts(clones.EncodeSignature(clones.Signature{1}))
			build, _ := openRebuildTestFileStore(t)
			receipts := seedRebuildCloneInputReceipts(t, build, facts)
			// Change only the working method, after the receipt is frozen.
			n := facts.Nodes[1]
			switch name {
			case "stamp":
				n.Meta[cloneSigMetaKey] = clones.EncodeSignature(clones.Signature{2})
			case "tokens":
				n.Meta[cloneTokensMetaKey] = int64(501)
			case "typed_raw_metadata":
				n.Meta[cloneShinglesMetaKey] = []uint64{}
			case "sidecar":
				require.NoError(t, build.BulkSetCloneShingles("", map[string][]uint64{n.ID: {1}}))
			}
			build.AddNode(n)

			calls := 0
			stats, err := visitRebuildDirectCloneEdges(t.Context(), build, receipts, 0, func(*graph.Edge) error {
				calls++
				return nil
			})
			require.ErrorIs(t, err, store_pebble.ErrRebuildView)
			require.Zero(t, stats)
			require.Zero(t, calls)
			requireRebuildTestFileRows(t, build, facts.Nodes, nil)
		})
	}
}

func rebuildSignedCloneFacts(stamp string) store_pebble.ParseFacts {
	const path = "signed.rs"
	return store_pebble.ParseFacts{
		EffectiveLanguage: "rust",
		File:              graph.FileMetaRow{FilePath: path, NodeCount: 2},
		Nodes: []*graph.Node{
			{ID: path + "::F", FilePath: path, Language: "rust", Kind: graph.KindFunction, Name: "F", StartLine: 9, EndLine: 9,
				Meta: map[string]any{cloneSigMetaKey: stamp, cloneTokensMetaKey: float64(500)}},
			{ID: path + "::M", FilePath: path, Language: "rust", Kind: graph.KindMethod, Name: "M", StartLine: 12, EndLine: 12,
				Meta: map[string]any{cloneSigMetaKey: stamp, cloneTokensMetaKey: int64(500)}},
		},
	}
}
