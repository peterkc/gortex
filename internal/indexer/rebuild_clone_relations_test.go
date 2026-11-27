package indexer

import (
	"errors"
	"fmt"
	"maps"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/zzet/gortex/internal/clones"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/graph/store_pebble"
	"github.com/zzet/gortex/internal/resolver"
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
		Signatures: rebuildCloneSignatureCounts{
			Corpus: 1, Kept: 1, Signed: 3,
			Claims: store_pebble.RebuildShadowedIDStats{ScratchBytes: 76, PeakRowBytes: 19},
		},
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
				return visitRebuildCloneRelations(t.Context(), build, replay, rebuildClonePredecessors{}, threshold, func(n *graph.Node) error {
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

	t.Run("apply", func(t *testing.T) {
		build, _ := openRebuildTestFileStore(t)
		receipts := seedRebuildCloneInputReceipts(t, build, facts...)
		var ns rebuildResolverNamespace
		require.NoError(t, ns.add(rebuildNamespaceRow{key: "requirements.txt", kind: rebuildNamespaceRegular}))
		require.NoError(t, ns.add(rebuildNamespaceRow{key: ".", kind: rebuildNamespaceCloseExact}))
		require.NoError(t, ns.freeze())
		stats, err := applyRebuildCloneRelations(t.Context(), build, receipts, rebuildClonePredecessors{namespace: &ns, modules: true}, threshold)
		require.NoError(t, err)
		require.Equal(t, wantStats, stats)
		view := build.RebuildView()
		var nodes []*graph.Node
		require.NoError(t, view.VisitNodes(t.Context(), func(n *graph.Node) error {
			nodes = append(nodes, n)
			return nil
		}))
		require.ElementsMatch(t, oracle.AllNodes(), nodes)
		var edges []*graph.Edge
		require.NoError(t, view.VisitEdges(t.Context(), func(e *graph.Edge) error {
			edges = append(edges, e)
			return nil
		}))
		require.ElementsMatch(t, wantEdges, edges)
	})
}

func TestRebuildCloneRootManifestPredecessors(t *testing.T) {
	for _, tc := range []struct {
		name         string
		kind         rebuildNamespaceKind
		modules      bool
		composerLock bool
		want         uint64
	}{
		{name: "regular", kind: rebuildNamespaceRegular, modules: true, want: 1},
		{name: "two_ids", kind: rebuildNamespaceRegular, modules: true, composerLock: true, want: 2},
		{name: "opaque", kind: rebuildNamespaceUnsupported, modules: true, want: 1},
		{name: "absence", modules: true},
		{name: "directory", kind: rebuildNamespaceDirectory, modules: true},
		{name: "modules_off", kind: rebuildNamespaceRegular},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var ns rebuildResolverNamespace
			if tc.kind != rebuildNamespaceUnknown {
				require.NoError(t, ns.add(rebuildNamespaceRow{key: "requirements.txt", kind: tc.kind}))
			}
			if tc.composerLock {
				require.NoError(t, ns.add(rebuildNamespaceRow{key: "composer.lock", kind: rebuildNamespaceRegular}))
			}
			require.NoError(t, ns.add(rebuildNamespaceRow{key: ".", kind: rebuildNamespaceCloseExact}))
			require.NoError(t, ns.freeze())

			facts := rebuildCloneRelationFixture(t)
			// A plugin can supply this literal ID for a same-path signed callable.
			facts[1].Nodes[0].ID = "requirements.txt"
			facts[2].Nodes[0].ID = "composer.lock"
			build, _ := openRebuildTestFileStore(t)
			receipts := seedRebuildCloneInputReceipts(t, build, facts...)
			predecessors := rebuildClonePredecessors{namespace: &ns, modules: tc.modules}
			stats, err := applyRebuildCloneRelations(t.Context(), build, receipts, predecessors, 0.82)
			if tc.want == 0 {
				require.NoError(t, err)
				require.EqualValues(t, 2, stats.Direct.Pairs)
				return
			}
			require.ErrorIs(t, err, store_pebble.ErrRebuildUnknownProvenance)
			reply := buildRefusal(err)
			require.Equal(t, uint8(36), reply.Refusal)
			require.Equal(t, tc.want, reply.UnknownProvenance)
			require.Contains(t, reply.Detail, fmt.Sprintf("root_manifest=%d", tc.want))
			require.Zero(t, stats)
			requireRebuildTestFileRows(t, build, []*graph.Node{
				facts[0].Nodes[0], facts[1].Nodes[0], facts[2].Nodes[0], facts[3].Nodes[0],
			}, nil)
		})
	}
}

func TestRebuildCloneModuleAndContractPredecessors(t *testing.T) {
	for _, tc := range []struct {
		name, root string
		modules    bool
		count      uint64
		detail     string
	}{
		{"all_domains", "requirements.txt", true, 3, "root_manifest=1 root_module=1 contract=1"},
		{"checked_root_absent", "pom.xml", true, 1, "root_manifest=0 root_module=0 contract=1"},
		{"modules_off", "requirements.txt", false, 1, "root_manifest=0 root_module=0 contract=1"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var ns rebuildResolverNamespace
			require.NoError(t, ns.add(rebuildNamespaceRow{key: tc.root, kind: rebuildNamespaceRegular}))
			require.NoError(t, ns.add(rebuildNamespaceRow{key: ".", kind: rebuildNamespaceCloseExact}))
			require.NoError(t, ns.freeze())
			// Check a raw input too: dropping a body later does not remove it
			// from the affected population or from the native CMS calculation.
			raw := rebuildCloneInputFixture(rebuildCloneInput{
				ID: "http::GET::/app", FilePath: "a.go", Language: "go", Line: 3,
				Tokens: 40, Shingles: []uint64{1, 3, 5, 7, 11, 13, 17, 19},
			}, "go")
			facts := rebuildCloneRelationFixture(t)
			facts[0] = raw
			facts[1].Nodes[0].ID = "requirements.txt"
			facts[2].Nodes[0].ID = "module::pypi:example@1"
			build, _ := openRebuildTestFileStore(t)
			receipts := seedRebuildCloneInputReceipts(t, build, facts...)
			predecessors := rebuildClonePredecessors{namespace: &ns, modules: tc.modules}
			stats, err := applyRebuildCloneRelations(t.Context(), build, receipts, predecessors, 0.82)
			require.ErrorIs(t, err, store_pebble.ErrRebuildUnknownProvenance)
			reply := buildRefusal(err)
			require.Equal(t, uint8(36), reply.Refusal)
			require.Equal(t, tc.count, reply.UnknownProvenance)
			require.Contains(t, reply.Detail, tc.detail)
			require.Zero(t, stats)
			requireRebuildTestFileRows(t, build, []*graph.Node{
				facts[0].Nodes[0], facts[1].Nodes[0], facts[2].Nodes[0], facts[3].Nodes[0],
			}, nil)
		})
	}
}

func TestRebuildCloneDynamicIDPredecessors(t *testing.T) {
	facts := rebuildCloneRelationFixture(t)
	facts[1].Nodes[0].ID = "repo::builtin::ts::array::push"
	facts[2].Nodes[0].ID = "module::go:os/exec"
	facts[3].Nodes[0].ID = "string::process::Command::new"
	build, _ := openRebuildTestFileStore(t)
	receipts := seedRebuildCloneInputReceipts(t, build, facts...)
	stats, err := applyRebuildCloneRelations(t.Context(), build, receipts, rebuildClonePredecessors{}, 0.82)
	require.ErrorIs(t, err, store_pebble.ErrRebuildUnknownProvenance)
	reply := buildRefusal(err)
	require.Equal(t, uint8(36), reply.Refusal)
	require.Equal(t, uint64(2), reply.UnknownProvenance)
	require.Contains(t, reply.Detail, "go_module=1 process=1")
	require.Zero(t, stats)
	requireRebuildTestFileRows(t, build, []*graph.Node{
		facts[0].Nodes[0], facts[1].Nodes[0], facts[2].Nodes[0], facts[3].Nodes[0],
	}, nil)
}

func TestRebuildCloneGoBuiltinIncumbent(t *testing.T) {
	// Portable receipts have no RepoPrefix, so an ordinary Go caller selects this ID.
	const id = "builtin::go::len"
	for _, name := range []string{"len", "cap"} {
		t.Run(name, func(t *testing.T) {
			ctx := t.Context()
			signed := rebuildCloneRelationFixture(t)[1]
			signed.Nodes[0].ID = id
			// A plugin may supply an explicit ID and a valid native signature.
			// Exercise the same reply check that rejects explicit KindBuiltin.
			prepared := []byte("fixture")
			signed.File.Size, signed.File.ContentHash = len(prepared), contentHashForSource(prepared)
			input := rebuildInput{Path: signed.File.FilePath, Language: signed.EffectiveLanguage}
			key := input.parseKey(rebuildDigest{1})
			payload := encodeRebuildTestParseFacts(t, signed)
			header := makeRebuildParseEntryHeader(key, payload)
			_, err := checkPendingRebuildReply(header[:], payload, key, input.Path, input.Language, prepared)
			require.NoError(t, err)

			use := &graph.Node{ID: "a.go::Use", Name: "Use", FilePath: "a.go", Language: "go", Kind: graph.KindFunction}
			call := &graph.Edge{From: use.ID, To: "unresolved::" + name, Kind: graph.EdgeCalls, FilePath: use.FilePath, Line: 2}
			goFile := store_pebble.ParseFacts{File: graph.FileMetaRow{FilePath: use.FilePath, NodeCount: 1}, EffectiveLanguage: "go",
				Nodes: []*graph.Node{use}, Edges: []*graph.Edge{call}}
			native := newFTSStore(t)
			require.NoError(t, native.AddBatchChecked(signed.Nodes, nil))
			require.NoError(t, native.AddBatchChecked(goFile.Nodes, goFile.Edges))
			require.Equal(t, graph.KindFunction, native.GetNode(id).Kind)
			stats, err := resolver.New(native).ResolveAllContext(ctx)
			require.NoError(t, err)
			require.False(t, stats.Incomplete)

			build, dir := openRebuildTestFileStore(t)
			receipts := seedRebuildCloneInputReceipts(t, build, goFile, signed)
			err = resolver.RunRebuildMain(ctx, build, store_pebble.RebuildLinkOptions{ScratchParent: dir, SpillBytes: 1 << 20}, resolver.RebuildMainInputs{
				SelectCallers: func(page []*graph.Edge) ([]*graph.Edge, error) { return page, nil },
				Receipts:      receipts,
			})
			if name == "len" {
				require.Equal(t, graph.KindBuiltin, native.GetNode(id).Kind)
				var refusal *store_pebble.RebuildUnknownProvenanceError
				require.ErrorAs(t, err, &refusal)
				require.EqualValues(t, 1, refusal.Count)
				require.Equal(t, graph.EdgeIdentityFor(call), refusal.First)
				raw, err := build.RebuildView().Edge(ctx, refusal.First)
				require.NoError(t, err)
				require.NotNil(t, raw, "refuse before applying the attribution moves")
				return
			}
			require.NoError(t, err)
			require.Equal(t, graph.KindFunction, native.GetNode(id).Kind)
			require.Equal(t, graph.KindBuiltin, native.GetNode("builtin::go::cap").Kind)
			cloned, err := applyRebuildCloneRelations(ctx, build, receipts, rebuildClonePredecessors{}, 0.82)
			require.NoError(t, err)
			require.Equal(t, 1, cloned.Signatures.Signed, "the unused builtin ID is still an ordinary clone input")
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
