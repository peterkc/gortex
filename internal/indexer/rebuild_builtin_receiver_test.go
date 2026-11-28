package indexer

import (
	"encoding/binary"
	"encoding/hex"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/zeebo/blake3"
	"github.com/zzet/gortex/internal/config"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/graph/store_pebble"
	"github.com/zzet/gortex/internal/parser"
	"github.com/zzet/gortex/internal/parser/languages"
	"github.com/zzet/gortex/internal/resolver"
	"go.uber.org/zap"
)

func TestRebuildBuiltinReceiverHostReceipt(t *testing.T) {
	ctx := t.Context()
	const file = "View.vue"
	const content = "<template><div/></template>\n<script setup lang=\"ts\">\nfunction sample() {\n  [1].slice(0);\n}\n</script>\n"
	root, src := rebuildInputFixture(t)
	writeFile(t, filepath.Join(root, file), content)
	reg := parser.NewRegistry()
	reg.Register(languages.NewVueExtractor())
	idx := New(graph.New(), reg, config.Default().Index, zap.NewNop())
	t.Cleanup(idx.Close)
	idx.installRebuildParsePolicy(freezeRebuildParsePolicy(root, idx.config))
	idx.SetRootPath(root)
	input, _, err := readRebuildInput(ctx, src, file, "vue", 1024)
	require.NoError(t, err)
	store := rebuildParseStore{dir: t.TempDir(), limit: 1 << 20}
	recipe := rebuildDigest{1}
	// This is the pre-K1 contract receipt recipe for a Vue input, not a
	// bare-parse key. Neither Vue nor its delegate uses a content refinement.
	ordinary := idx.rebuildBaseExtractorRecipe(recipe, input)
	old := append([]byte("gortex/rebuild/contract-parse\x00"), ordinary[:]...)
	old = append(old, store_pebble.ContractParseFactsFormat)
	old = binary.AppendUvarint(old, rebuildContractCaptureVersion)
	old = binary.AppendUvarint(old, postExtractionPolicyVersion)
	old = binary.AppendUvarint(old, uint64(extractorVersionForLang(input.Language)))
	old = binary.AppendUvarint(old, 0)
	oldKey := input.parseKey(blake3.Sum256(append(old, idx.rebuildContractPolicy().suffix...)))
	oldPayload := encodeRebuildTestParseFacts(t, store_pebble.ParseFacts{
		File: graph.FileMetaRow{FilePath: file}, EffectiveLanguage: "vue",
	})
	require.NoError(t, store.write(oldKey, oldPayload))
	v1 := append([]byte("gortex/rebuild/contract-parse-receiver\x00"), ordinary[:]...)
	v1 = append(v1, store_pebble.ContractParseFactsFormat)
	v1 = binary.AppendUvarint(v1, 1)
	v1 = binary.AppendUvarint(v1, rebuildContractCaptureVersion)
	v1 = binary.AppendUvarint(v1, postExtractionPolicyVersion)
	v1 = binary.AppendUvarint(v1, uint64(extractorVersionForLang(input.Language)))
	v1 = binary.AppendUvarint(v1, 0)
	v1Key := input.parseKey(blake3.Sum256(append(v1, idx.rebuildContractPolicy().suffix...)))
	require.NoError(t, store.write(v1Key, oldPayload))
	bounds := rebuildTransformBounds{FileBytes: 1024, CaptureBytes: 1024}
	facts, reused, err := idx.loadRebuildFile(ctx, src, store, input, recipe, bounds)
	require.NoError(t, err)
	require.False(t, reused)
	saved, err := store.read(oldKey)
	require.NoError(t, err)
	require.Equal(t, oldPayload, saved)
	saved, err = store.read(v1Key)
	require.NoError(t, err)
	require.Equal(t, oldPayload, saved)
	cached, reused, err := idx.loadRebuildFile(ctx, nil, store, input, recipe, bounds)
	require.NoError(t, err)
	require.True(t, reused, "matching receipts replay without a content source")
	require.Equal(t, facts, cached)
	var calls []*graph.Edge
	for _, e := range cached.Edges {
		if e.Kind == graph.EdgeCalls && e.To == "unresolved::*.slice" {
			calls = append(calls, e)
		}
	}
	require.Len(t, calls, 1)
	raw := calls[0]
	hint, ok := raw.BuiltinReceiverHint()
	require.True(t, ok)
	require.Equal(t, "typescript", hint.Language)
	require.Equal(t, "array", hint.Family)
	require.Equal(t, file, raw.FilePath)
	require.Equal(t, 4, raw.Line)
	require.NotContains(t, raw.Meta, graph.MetaLinkStatus)

	private, dir := openRebuildTestFileStore(t)
	inputs := []rebuildInput{input}
	inputRoot, err := rebuildInputRoot(inputs)
	require.NoError(t, err)
	digest, err := hex.DecodeString(inputRoot)
	require.NoError(t, err)
	job := rebuildFileStages{
		workDir: dir, incarnation: rebuildTestIncarnation, inputs: inputs,
		recipes: map[string]rebuildDigest{"vue": recipe}, bounds: bounds,
		witness: store_pebble.Witness{
			Input: store_pebble.HeadDigest(digest), Recipe: store_pebble.HeadDigest{2},
			Profile: store_pebble.FullProfile, Stage: store_pebble.StageManifest,
		},
	}
	head, err := store_pebble.OpenHead(filepath.Join(t.TempDir(), "head"), job.incarnation, job.witness.Profile)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, head.Close()) })
	var offered []store_pebble.Stage
	counts, err := idx.buildRebuildFileStages(ctx, nil, store, private, job, func(tables store_pebble.StageTables) error {
		offered = append(offered, tables.Witness.Stage)
		_, err := head.PublishStage(ctx, tables)
		return err
	})
	require.NoError(t, err)
	require.Equal(t, []store_pebble.Stage{store_pebble.StageManifest, store_pebble.StageFileGraph}, offered)
	picked, err := counts.census.selectPendingCallers(calls)
	require.NoError(t, err)
	require.Empty(t, picked, "main uses the recording host's language, not the raw delegate hint")
	snapshot := head.OpenSnapshot()
	t.Cleanup(func() { require.NoError(t, snapshot.Close()) })
	require.NotNil(t, snapshot.GetNode(raw.From))
	for _, e := range snapshot.GetOutEdges(raw.From) {
		require.NotEqual(t, graph.EdgeCalls, e.Kind)
	}
	require.Empty(t, snapshot.GetInEdges("builtin::ts::array::slice"))
}

func TestRebuildBuiltinReceiverFingerprints(t *testing.T) {
	for _, tc := range []struct {
		path      string
		source    string
		extractor parser.Extractor
	}{
		{"example.go", "package p\nfunc f(xs []int) int { return len(xs) }", languages.NewGoExtractor()},
		{"example.js", "function f() { [1].slice(0); }", languages.NewJavaScriptExtractor()},
		{"example.ts", "function f() { [1].slice(0); }", languages.NewTypeScriptExtractor()},
		{"Example.vue", "<!-- host -->\n<script setup lang=\"ts\">\nfunction f() { [1].slice(0); }\n</script>", languages.NewVueExtractor()},
		{"Example.svelte", "<!-- host -->\n<script>\nfunction f() { [1].slice(0); }\n</script>", languages.NewSvelteExtractor()},
		{"Example.astro", "<!-- host -->\n<script lang=\"ts\">\nfunction f() { [1].slice(0); }\n</script>", languages.NewAstroExtractor()},
		{"example.html", "<!-- host -->\n<script>\nfunction f() { [1].slice(0); }\n</script>", languages.NewHTMLExtractor()},
	} {
		t.Run(tc.path, func(t *testing.T) {
			native, err := parser.Extract(tc.extractor, tc.path, []byte(tc.source), parser.ExtractionOptions{})
			require.NoError(t, err)
			defer native.ReleaseTree()
			captured, err := parser.Extract(tc.extractor, tc.path, []byte(tc.source), parser.ExtractionOptions{CaptureBuiltinReceiverFacts: true})
			require.NoError(t, err)
			defer captured.ReleaseTree()
			var hinted []*graph.Edge
			for _, edge := range captured.Edges {
				if _, ok := edge.BuiltinReceiverHint(); ok {
					hinted = append(hinted, edge)
				}
			}
			require.NotEmpty(t, hinted)
			require.NotEmpty(t, stampExtractionGraphFingerprint(native))
			stampRebuildExtractionGraphFingerprint(captured)
			require.ElementsMatch(t, native.Nodes, captured.Nodes)
			for _, node := range native.Nodes {
				if node.Kind != graph.KindFile {
					continue
				}
				for _, key := range []string{sourceSemanticFingerprintMeta, sourceMetadataFingerprintMeta,
					sourceCoreFingerprintMeta, sourceDerivedDeclFingerprintMeta, sourceDerivedImportFingerprintMeta,
					sourceDerivedRuntimeFingerprintMeta, sourceDerivedArtifactFingerprintMeta, sourceDerivedHierarchyFingerprintMeta} {
					require.NotEmpty(t, node.Meta[key], key)
				}
			}
			for _, edge := range hinted {
				_, ok := edge.BuiltinReceiverHint()
				require.True(t, ok, "receiver evidence stays on the receipt edge")
			}
			// Ordinary payload remains part of the native-compatible digest.
			hinted[0].Meta["receiver_type"] = "changed"
			stampRebuildExtractionGraphFingerprint(captured)
			require.NotEqual(t, fileFingerprintMetadata(t, native), fileFingerprintMetadata(t, captured))
		})
	}
}

func fileFingerprintMetadata(t *testing.T, result *parser.ExtractionResult) string {
	t.Helper()
	for _, node := range result.Nodes {
		if node.Kind == graph.KindFile {
			return node.Meta[sourceMetadataFingerprintMeta].(string)
		}
	}
	t.Fatal("missing fingerprinted file node")
	return ""
}

func TestRebuildReceiverFingerprintKeepsRawEdgesProof(t *testing.T) {
	file := &graph.Node{ID: "file.go", FilePath: "file.go", Kind: graph.KindFile}
	edge := &graph.Edge{From: file.ID, To: "unresolved::len", Kind: graph.EdgeCalls, FilePath: file.FilePath}
	result := &parser.ExtractionResult{Nodes: []*graph.Node{file}, Edges: []*graph.Edge{edge}}
	require.NotEmpty(t, stampExtractionGraphFingerprint(result))
	raw := file.Meta[sourceRawEdgesFingerprintMeta]
	require.NotEmpty(t, raw)
	edge.SetBuiltinReceiverHint(graph.BuiltinReceiverHint{Language: "go", Form: "unbound", Name: "len"})
	stampRebuildExtractionGraphFingerprint(result)
	require.Equal(t, raw, file.Meta[sourceRawEdgesFingerprintMeta])
	edge.Meta["receiver_type"] = "T"
	stampRebuildExtractionGraphFingerprint(result)
	require.NotEqual(t, raw, file.Meta[sourceRawEdgesFingerprintMeta])
}

func TestRebuildGoBuiltinScopeFromExtraction(t *testing.T) {
	for _, tc := range []struct {
		file, pkg, sibling, siblingPkg string
	}{
		{"pkg/use.go", "p", "pkg/sibling.go", "p"},
		{"pkg/use_test.go", "p", "pkg/sibling.go", "p"},
		{"pkg/use.go", "p", "pkg/sibling_test.go", "p_test"},
		{"pkg/use_test.go", "p_test", "pkg/sibling.go", "p"},
	} {
		file := tc.file
		for _, declared := range []bool{false, true} {
			name := file + "/unbound"
			if declared {
				name = file + "/same_package_declaration"
			}
			if tc.pkg != tc.siblingPkg {
				name = file + "/different_package_unbound"
				if declared {
					name = file + "/different_package_declaration"
				}
			}
			t.Run(name, func(t *testing.T) {
				ctx := t.Context()
				idx := newTestIndexer(graph.New())
				t.Cleanup(idx.Close)
				idx.SetRootPath(t.TempDir())
				ext := languages.NewGoExtractor()
				caller, err := idx.extractPreparedRebuildFile(ctx, file, ext, []byte("package "+tc.pkg+"\nfunc use(xs []int) int { return len(xs) }\n"))
				require.NoError(t, err)
				siblingSource := "package " + tc.siblingPkg + "\nfunc other(xs []int) int { return 1 }\n"
				if declared {
					siblingSource = "package " + tc.siblingPkg + "\nfunc len(xs []int) int { return 1 }\n"
				}
				sibling, err := idx.extractPreparedRebuildFile(ctx, tc.sibling, ext, []byte(siblingSource))
				require.NoError(t, err)
				for _, facts := range []store_pebble.ParseFacts{sibling, caller} {
					var scope string
					for _, n := range facts.Nodes {
						if n.Kind == graph.KindFile {
							scope, _ = n.Meta[store_pebble.RebuildGoBuiltinScopeReceiptMeta].(string)
						}
					}
					want := tc.pkg
					if facts.File.FilePath == sibling.File.FilePath {
						want = tc.siblingPkg
						if declared {
							want += " len"
						}
					}
					require.Equal(t, want, scope)
				}
				receipts := func(yield func(store_pebble.ParseFacts, error) bool) {
					if yield(sibling, nil) {
						yield(caller, nil)
					}
				}
				build, dir := openRebuildTestFileStore(t)
				claims, err := build.ReduceRebuildSharedClaims(ctx, 1<<20, receipts, func(keep func(string, uint64) (bool, error)) error {
					for i, facts := range []store_pebble.ParseFacts{sibling, caller} {
						if err := build.AddParsedFileSelected("", facts, func(id string) (bool, error) {
							return keep(id, uint64(i+1))
						}); err != nil {
							return err
						}
					}
					return nil
				})
				require.NoError(t, err)
				require.Zero(t, claims.UnexplainedNodes)
				for _, path := range []string{file, sibling.File.FilePath} {
					n, err := build.RebuildView().Node(ctx, path)
					require.NoError(t, err)
					require.NotNil(t, n)
					require.NotContains(t, n.Meta, store_pebble.RebuildGoBuiltinScopeReceiptMeta)
				}
				opts := store_pebble.RebuildLinkOptions{ScratchParent: dir, SpillBytes: 1 << 20}
				require.NoError(t, resolver.RunRebuildMain(ctx, build, opts, resolver.RebuildMainInputs{
					SelectCallers: func(page []*graph.Edge) ([]*graph.Edge, error) { return page, nil },
					Receipts:      receipts,
				}))
				require.NoError(t, runRebuildTestFiles(ctx, build, receipts, nil))
				_, err = applyRebuildCloneRelations(ctx, build, receipts, rebuildClonePredecessors{fileClaims: &claims}, 0.8)
				require.NoError(t, err)
				const builtin = "builtin::go::len"
				readCall := func() *graph.Edge {
					var calls []*graph.Edge
					require.NoError(t, build.RebuildView().VisitEdges(ctx, func(e *graph.Edge) error {
						if e.From == file+"::use" && e.Kind == graph.EdgeCalls {
							calls = append(calls, e)
						}
						return nil
					}))
					require.Len(t, calls, 1)
					return calls[0]
				}
				extractor := func(string) string { return "go" }
				require.NoError(t, build.SettleRebuildPlatformCalls(ctx, extractor))
				call := readCall()
				status, ok := call.LinkStatus()
				require.True(t, ok)
				if !declared {
					require.Equal(t, builtin, call.To)
					require.Equal(t, graph.LinkStatusPlatform, status)
					return
				}
				// Main's same-directory lookup can pick the ordinary declaration,
				// even from a different Go package. Settlement must leave it alone.
				require.Equal(t, tc.sibling+"::len", call.To)
				require.Equal(t, graph.LinkStatusResolved, status)
				// Isolate the fallback status decision when a builtin move is selected.
				// The actual main result above is not changed to force a builtin target.
				build, _ = openRebuildTestFileStore(t)
				require.NoError(t, build.AddParsedFile("", sibling))
				require.NoError(t, build.AddParsedFile("", caller))
				require.NoError(t, build.RebuildGoBuiltins(ctx, true, 1<<20, func(e *graph.Edge, _ func(string) (*graph.Node, error)) (string, error) {
					if e.Kind == graph.EdgeCalls && e.To == "unresolved::len" {
						return builtin, nil
					}
					return "", nil
				}))
				require.NoError(t, build.SettleRebuildPlatformCalls(ctx, extractor))
				call = readCall()
				require.Equal(t, builtin, call.To)
				status, ok = call.LinkStatus()
				require.True(t, ok)
				want := graph.LinkStatusPlatform
				if tc.pkg == tc.siblingPkg {
					want = graph.LinkStatusProbablePlatform
				}
				require.Equal(t, want, status)
			})
		}
	}
}

func TestRebuildReceiverV2ThroughV5ReceiptsAreStale(t *testing.T) {
	for _, tc := range []struct {
		file, language, source string
		extractor              parser.Extractor
		versions               []uint64
	}{
		{"a.go", "go", "package p\nfunc use(xs []int) int { return len(xs) }\n", languages.NewGoExtractor(), []uint64{2, 3, 4, 5}},
		{"a.js", "javascript", "function use(xs) { xs.slice(0); }", languages.NewJavaScriptExtractor(), []uint64{3, 4, 5}},
		{"a.ts", "typescript", "function use(xs: number[]) { xs.slice(0); }", languages.NewTypeScriptExtractor(), []uint64{3, 4, 5}},
		{"A.vue", "vue", "<!-- host -->\n<script setup lang=\"ts\">\nfunction use() { [1].slice(0); }\n</script>", languages.NewVueExtractor(), []uint64{4, 5}},
		{"A.svelte", "svelte", "<!-- host -->\n<script>\nfunction use() { [1].slice(0); }\n</script>", languages.NewSvelteExtractor(), []uint64{4, 5}},
		{"A.astro", "astro", "<!-- host -->\n<script lang=\"ts\">\nfunction use() { [1].slice(0); }\n</script>", languages.NewAstroExtractor(), []uint64{4, 5}},
		{"a.html", "html", "<!-- host -->\n<script>\nfunction use() { [1].slice(0); }\n</script>", languages.NewHTMLExtractor(), []uint64{4, 5}},
	} {
		t.Run(tc.file, func(t *testing.T) {
			ctx := t.Context()
			root, src := rebuildInputFixture(t)
			writeFile(t, filepath.Join(root, tc.file), tc.source)
			reg := parser.NewRegistry()
			reg.Register(tc.extractor)
			idx := New(graph.New(), reg, config.Default().Index, zap.NewNop())
			t.Cleanup(idx.Close)
			idx.SetRootPath(root)
			idx.installRebuildParsePolicy(freezeRebuildParsePolicy(root, idx.config))
			input, _, err := readRebuildInput(ctx, src, tc.file, tc.language, 1024)
			require.NoError(t, err)
			store := rebuildParseStore{dir: t.TempDir(), limit: 1 << 20}
			recipe := rebuildDigest{1}
			ordinary := idx.rebuildBaseExtractorRecipe(recipe, input)
			payload := encodeRebuildTestParseFacts(t, store_pebble.ParseFacts{
				File: graph.FileMetaRow{FilePath: tc.file}, EffectiveLanguage: tc.language,
			})
			oldKeys := make([]rebuildDigest, 0, len(tc.versions))
			for _, version := range tc.versions {
				old := append([]byte("gortex/rebuild/contract-parse-receiver\x00"), ordinary[:]...)
				old = append(old, store_pebble.ContractParseFactsFormat)
				old = binary.AppendUvarint(old, version)
				old = binary.AppendUvarint(old, rebuildContractCaptureVersion)
				old = binary.AppendUvarint(old, postExtractionPolicyVersion)
				old = binary.AppendUvarint(old, uint64(extractorVersionForLang(tc.language)))
				old = binary.AppendUvarint(old, 0)
				key := input.parseKey(blake3.Sum256(append(old, idx.rebuildContractPolicy().suffix...)))
				require.NoError(t, store.write(key, payload))
				oldKeys = append(oldKeys, key)
			}
			bounds := rebuildTransformBounds{FileBytes: 1024, CaptureBytes: 1024}
			fresh, reused, err := idx.loadRebuildFile(ctx, src, store, input, recipe, bounds)
			require.NoError(t, err)
			require.False(t, reused)
			require.NotEmpty(t, fresh.Nodes)
			for _, key := range oldKeys {
				saved, err := store.read(key)
				require.NoError(t, err)
				require.Equal(t, payload, saved)
			}
			cached, reused, err := idx.loadRebuildFile(ctx, nil, store, input, recipe, bounds)
			require.NoError(t, err)
			require.True(t, reused)
			require.Equal(t, fresh, cached)
		})
	}
}
