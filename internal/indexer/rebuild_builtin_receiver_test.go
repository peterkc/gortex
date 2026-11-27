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
