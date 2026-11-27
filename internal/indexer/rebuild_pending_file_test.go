package indexer

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/zzet/gortex/internal/config"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/graph/store_pebble"
	"github.com/zzet/gortex/internal/parser"
	"github.com/zzet/gortex/internal/parser/languages"
	"go.uber.org/zap"
)

func TestPendingRebuildFileSavesNativeReceipt(t *testing.T) {
	t.Run("ASCII", func(t *testing.T) {
		checkPendingRebuildFileNativeReceipt(t, "events.go", "events.go")
	})
	t.Run("NFD", func(t *testing.T) {
		checkPendingRebuildFileNativeReceipt(t, "events-e\u0301.go", "events-\u00e9.go")
	})
}

func checkPendingRebuildFileNativeReceipt(t *testing.T, path, graphPath string) {
	t.Helper()
	ctx := t.Context()
	root, src := rebuildInputFixture(t)
	exe, err := os.Executable()
	require.NoError(t, err)
	t.Setenv(crashWorkerEnv, "1") // TestMain runs the same entry point as __parse-worker.
	content := []byte(`package sample
const Label = "orders"
func Broken() { value := }
func Again() { another := }
`)
	// The frozen key hashes the on-disk bytes, but both receipts describe
	// the transformed bytes. The BOM exercises that distinction.
	require.NoError(t, os.WriteFile(filepath.Join(root, path), append([]byte{0xef, 0xbb, 0xbf}, content...), 0o600))
	input, _, err := readRebuildInput(ctx, src, path, "go", 1<<20)
	require.NoError(t, err)

	cfg := config.Default().Index
	require.True(t, cfg.Coverage.IsEnabled("ownership"), "test the default ownership policy")
	owners := filepath.Join(root, "CODEOWNERS")
	require.NoError(t, os.WriteFile(owners, []byte("* @wrong\n/"+graphPath+" @frozen\n"), 0o600))
	reg := parser.NewRegistry()
	goExtractor := languages.NewGoExtractor()
	reg.Register(goExtractor)
	idx := New(graph.New(), reg, cfg, zap.NewNop())
	policy := freezeRebuildParsePolicy(root, cfg)
	idx.installRebuildParsePolicy(policy)
	idx.SetRootPath(root)
	t.Cleanup(idx.Close)
	require.NoError(t, os.WriteFile(owners, []byte("* @edited\n"), 0o600))

	recipe := rebuildDigest{4}
	bounds := rebuildTransformBounds{FileBytes: 1 << 20}
	key := input.parseKey(idx.rebuildExtractorRecipe(recipe, input))
	store := rebuildParseStore{dir: t.TempDir(), limit: 1 << 20}
	nativeStore := rebuildParseStore{dir: t.TempDir(), limit: 1 << 20}
	_, reused, err := idx.loadPendingRebuildFile(ctx, src, store, input, recipe, bounds, exe)
	require.NoError(t, err)
	require.False(t, reused)
	entry, err := store.read(key)
	require.NoError(t, err)
	facts, err := store_pebble.DecodeParseFacts(entry)
	require.NoError(t, err)
	require.Positive(t, facts.File.NodeCount)
	require.NotEmpty(t, facts.Edges)
	require.NotEmpty(t, facts.Symbols)
	require.Equal(t, "go", facts.EffectiveLanguage)
	require.Equal(t, len(content), facts.File.Size)
	require.Equal(t, contentHashForSource(content), facts.File.ContentHash)
	require.Equal(t, path, facts.File.FilePath)
	require.Contains(t, facts.Constants, graph.ConstantValueRow{FilePath: path, NodeID: graphPath + "::Label", Value: "orders"})
	var fileNode *graph.Node
	for _, n := range facts.Nodes {
		if n.Kind == graph.KindFile {
			fileNode = n
			break
		}
	}
	require.NotNil(t, fileNode)
	require.Equal(t, graphPath, fileNode.ID)
	require.Equal(t, graphPath, fileNode.FilePath)
	var definesLabel bool
	for _, e := range facts.Edges {
		if e.Kind == graph.EdgeDefines && e.From == graphPath && e.To == graphPath+"::Label" {
			require.Equal(t, graphPath, e.FilePath)
			definesLabel = true
		}
	}
	require.True(t, definesLabel)
	var locations []string
	require.NoError(t, json.Unmarshal([]byte(facts.File.Errors), &locations))
	require.Greater(t, len(locations), 1, "fixture must distinguish ordering of parse errors")
	native, err := parser.Extract(goExtractor, graphPath, content, idx.extractionOptionsValue())
	require.NoError(t, err)
	t.Cleanup(native.ReleaseTree)
	require.NotNil(t, native.Tree)
	require.Equal(t, native.Tree.ParseErrorLocations(), locations)

	// Rebuild the same raw relative name under another root. A relocated
	// parse must produce the exact pending receipt without reading the edit.
	otherRoot, otherSrc := rebuildInputFixture(t)
	require.NoError(t, os.WriteFile(filepath.Join(otherRoot, path), append([]byte{0xef, 0xbb, 0xbf}, content...), 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(root, path), []byte("package sample\nfunc Edited() {}\n"), 0o600))
	_, reused, err = idx.loadRebuildFile(ctx, otherSrc, nativeStore, input, recipe, bounds)
	require.NoError(t, err)
	require.False(t, reused)
	nativeEntry, err := nativeStore.read(key)
	require.NoError(t, err)
	require.Equal(t, nativeEntry, entry, "both paths must write the same immutable entry")
	var savedOwners []string
	for _, e := range facts.Edges {
		if e.Kind == graph.EdgeOwns {
			savedOwners = append(savedOwners, e.From)
		}
	}
	require.Equal(t, []string{"team::frozen"}, savedOwners)

	missingExe := filepath.Join(t.TempDir(), "missing-gortex")
	before := parser.ParseCount()
	cached, reused, err := idx.loadPendingRebuildFile(ctx, src, store, input, recipe, bounds, missingExe)
	require.NoError(t, err, "matching cache hit must not read the edited input or spawn a parser")
	require.True(t, reused)
	require.Equal(t, facts, cached)
	require.Equal(t, before, parser.ParseCount(), "pending reuse must not parse in the owner either")

	miss := rebuildParseStore{dir: t.TempDir(), limit: 1 << 20}
	got, reused, err := idx.loadPendingRebuildFile(ctx, src, miss, input, recipe, bounds, missingExe)
	require.ErrorIs(t, err, ErrPendingRebuildFile)
	require.ErrorIs(t, err, ErrBuildInputChanged)
	require.False(t, reused)
	require.Empty(t, got)
	_, err = miss.read(key)
	require.ErrorIs(t, err, os.ErrNotExist)
}

func TestPendingRebuildFileRefusesWithoutSaving(t *testing.T) {
	ctx := t.Context()
	root, src := rebuildInputFixture(t)
	exe, err := os.Executable()
	require.NoError(t, err)
	t.Setenv(crashWorkerEnv, "1")
	readInput := func(path, lang string) rebuildInput {
		t.Helper()
		require.NoError(t, os.WriteFile(filepath.Join(root, path), []byte("package sample\nfunc Example() {}\n"), 0o600))
		input, _, err := readRebuildInput(ctx, src, path, lang, 1<<20)
		require.NoError(t, err)
		return input
	}
	goInput, badInput := readInput("file.go", "go"), readInput("file.boom", "boom")

	cfg := config.Default().Index
	enabled := true
	cfg.Coverage.Ownership.Enabled = &enabled
	reg := parser.NewRegistry()
	reg.Register(languages.NewGoExtractor())
	reg.Register(panicExtractor{}) // The native route would panic instead of returning the worker's refusal.
	start := func(policy rebuildParsePolicy) *Indexer {
		t.Helper()
		idx := New(graph.New(), reg, cfg, zap.NewNop())
		idx.installRebuildParsePolicy(policy)
		idx.SetRootPath(root)
		t.Cleanup(idx.Close)
		return idx
	}
	ready := start(freezeRebuildParsePolicy(root, cfg))
	require.NoError(t, os.Mkdir(filepath.Join(root, "CODEOWNERS"), 0o700))
	unavailable := start(freezeRebuildParsePolicy(root, cfg))
	missingExe := filepath.Join(t.TempDir(), "missing-gortex")
	bounds := rebuildTransformBounds{FileBytes: 1 << 20}
	for _, test := range []struct {
		name   string
		idx    *Indexer
		input  rebuildInput
		bounds rebuildTransformBounds
		exe    string
		detail string
	}{
		{"spawn", ready, goInput, bounds, missingExe, "start parser worker"},
		{"extract", ready, badInput, bounds, exe, "no extractor for language boom"},
		{"ownership", unavailable, goInput, bounds, exe, "frozen parse policy unavailable"},
		{"transform", ready, goInput, rebuildTransformBounds{}, exe, "transform policy unavailable"},
	} {
		t.Run(test.name, func(t *testing.T) {
			store := rebuildParseStore{dir: t.TempDir(), limit: 1 << 20}
			got, reused, err := test.idx.loadPendingRebuildFile(ctx, src, store, test.input, rebuildDigest{4}, test.bounds, test.exe)
			require.ErrorIs(t, err, ErrPendingRebuildFile)
			require.ErrorContains(t, err, test.detail)
			require.False(t, reused)
			require.Empty(t, got)
			entries, err := os.ReadDir(store.dir)
			require.NoError(t, err)
			require.Empty(t, entries, "refusal must leave neither an entry nor partial files")
		})
	}
}

func TestPendingRebuildFileRequiresWorkerForGeneratedC(t *testing.T) {
	ctx := t.Context()
	root, src := rebuildInputFixture(t)
	exe, err := os.Executable()
	require.NoError(t, err)
	t.Setenv(crashWorkerEnv, "1")
	const path = "parser.c"
	content := generatedParserTestSource("\n/* \xff */\nTS_PUBLIC const TSLanguage *tree_sitter_demo(void) { return &language; }\n")
	require.NoError(t, os.WriteFile(filepath.Join(root, path), content, 0o600))
	input, _, err := readRebuildInput(ctx, src, path, "c", 1<<20)
	require.NoError(t, err)
	cfg := config.Default().Index
	reg := parser.NewRegistry()
	ext := languages.NewCExtractor()
	reg.Register(ext)
	idx := New(graph.New(), reg, cfg, zap.NewNop())
	idx.installRebuildParsePolicy(freezeRebuildParsePolicy(root, cfg))
	idx.SetRootPath(root)
	t.Cleanup(idx.Close)

	recipe := rebuildDigest{9}
	bounds := rebuildTransformBounds{FileBytes: 1 << 20}
	key := input.parseKey(idx.rebuildExtractorRecipe(recipe, input))
	store := rebuildParseStore{dir: t.TempDir(), limit: 1 << 20}
	badExe := filepath.Join(t.TempDir(), "missing-gortex")
	got, reused, err := idx.loadPendingRebuildFile(ctx, src, store, input, recipe, bounds, badExe)
	require.ErrorIs(t, err, ErrPendingRebuildFile)
	require.ErrorContains(t, err, "start parser worker")
	require.False(t, reused)
	require.Empty(t, got)
	_, err = store.read(key)
	require.ErrorIs(t, err, os.ErrNotExist)

	_, reused, err = idx.loadPendingRebuildFile(ctx, src, store, input, recipe, bounds, exe)
	require.NoError(t, err)
	require.False(t, reused)
	entry, err := store.read(key)
	require.NoError(t, err)
	facts, err := store_pebble.DecodeParseFacts(entry)
	require.NoError(t, err)
	require.Equal(t, contentHashForSource(content), facts.File.ContentHash)
	require.Equal(t, 2, facts.File.NodeCount)
	require.Equal(t, "tree_sitter_demo", facts.Nodes[1].Name)
	require.Equal(t, true, facts.Nodes[0].Meta[generatedParserProjectionMetaKey])

	// A frozen Git path need not be valid UTF-8, even if the host filesystem
	// does not allow such names. Exercise the transport without a disk path.
	input.Path = "vendor-\xff/parser.c"
	entry, err = idx.submitPendingRebuildFile(ctx, input.Path, ext, content, idx.frozenRebuildParsePolicy.Load(), exe, store.limit)
	require.NoError(t, err)
	facts, err = store_pebble.DecodeParseFacts(entry)
	require.NoError(t, err)
	require.Equal(t, input.Path, facts.File.FilePath)
	require.Equal(t, input.Path+"::tree_sitter_demo", facts.Nodes[1].ID)
	require.Equal(t, contentHashForSource(content), facts.File.ContentHash)
}

func TestPendingRebuildFileEffectiveLanguage(t *testing.T) {
	ctx := t.Context()
	root, src := rebuildInputFixture(t)
	exe, err := os.Executable()
	require.NoError(t, err)
	t.Setenv(crashWorkerEnv, "1")

	cfg := config.Default().Index
	reg := parser.NewRegistry()
	reg.Register(languages.NewTypeScriptExtractor())
	reg.Register(languages.NewJavaScriptExtractor())
	reg.Register(languages.NewCExtractor())
	reg.Register(languages.NewCppExtractor())
	idx := New(graph.New(), reg, cfg, zap.NewNop())
	idx.installRebuildParsePolicy(rebuildParsePolicy{})
	idx.SetRootPath(root)
	t.Cleanup(idx.Close)
	idx.transforms.transforms = append(idx.transforms.transforms, rebuildHeaderTestTransform{})

	recipe := rebuildDigest{1}
	bounds := rebuildTransformBounds{FileBytes: 1 << 20}
	for _, tc := range []struct {
		path, raw, prepared, walk, effective string
		noDefinitions, skipped               bool
	}{
		{path: "main.tsx", raw: "export function Entry(): number { return 1; }\n", walk: "typescript", effective: "typescript"},
		{path: "main.jsx", raw: "export function Entry() { return 1; }\n", walk: "javascript", effective: "javascript"},
		{path: "empty.js", raw: "// No declarations\n", walk: "javascript", effective: "javascript", noDefinitions: true},
		{path: "header.h", raw: "namespace demo {}\n", prepared: "int Entry(void) { return 1; }\n", walk: "c", effective: "cpp"},
		{path: "binary.h", raw: "namespace demo {}\x00", walk: "c", effective: "cpp", noDefinitions: true, skipped: true},
	} {
		t.Run(tc.path, func(t *testing.T) {
			file := filepath.Join(root, tc.path)
			require.NoError(t, os.WriteFile(file, []byte(tc.raw), 0o600))
			walk, ok := reg.DetectLanguage(tc.path)
			require.True(t, ok)
			require.Equal(t, tc.walk, walk)
			input, _, err := readRebuildInput(ctx, src, tc.path, walk, 1024)
			require.NoError(t, err)
			store := rebuildParseStore{dir: t.TempDir(), limit: 1 << 20}
			pending := rebuildParseStore{dir: t.TempDir(), limit: 1 << 20}
			key := input.parseKey(idx.rebuildExtractorRecipe(recipe, input))
			_, reused, err := idx.loadRebuildFile(ctx, src, store, input, recipe, bounds)
			require.NoError(t, err)
			require.False(t, reused)
			normalEntry, err := store.read(key)
			require.NoError(t, err)
			_, reused, err = idx.loadPendingRebuildFile(ctx, src, pending, input, recipe, bounds, exe)
			require.NoError(t, err)
			require.False(t, reused)
			pendingEntry, err := pending.read(key)
			require.NoError(t, err)
			require.Equal(t, normalEntry, pendingEntry)
			facts, err := store_pebble.DecodeParseFacts(pendingEntry)
			require.NoError(t, err)
			require.Equal(t, tc.path, facts.File.FilePath)
			require.Equal(t, tc.effective, facts.EffectiveLanguage)
			expectedContent := tc.raw
			if tc.prepared != "" {
				expectedContent = tc.prepared
			}
			require.Equal(t, contentHashForSource([]byte(expectedContent)), facts.File.ContentHash)
			if tc.noDefinitions {
				require.Empty(t, facts.Symbols)
			} else {
				require.NotEmpty(t, facts.Symbols)
			}
			if tc.skipped {
				require.Len(t, facts.Nodes, 1)
				require.Equal(t, true, facts.Nodes[0].Meta["skipped_due_to_binary"])
			}

			if tc.path == "header.h" {
				// The live edit no longer has the marker. Passing no source
				// proves neither replay path even probes the checkout.
				require.NoError(t, os.WriteFile(file, []byte(tc.prepared), 0o600))
				cached, reused, err := idx.loadRebuildFile(ctx, nil, store, input, recipe, bounds)
				require.NoError(t, err)
				require.True(t, reused)
				require.Equal(t, facts, cached)
				cached, reused, err = idx.loadPendingRebuildFile(ctx, nil, pending, input, recipe, bounds, "")
				require.NoError(t, err)
				require.True(t, reused)
				require.Equal(t, facts, cached)
			}
		})
	}
}

// Erase the C++ marker so a probe of prepared bytes would select C.
type rebuildHeaderTestTransform struct{}

func (rebuildHeaderTestTransform) name() string             { return "test-erase-language-marker" }
func (rebuildHeaderTestTransform) matches(path string) bool { return path == "header.h" }
func (rebuildHeaderTestTransform) asLanguage() string       { return "" }
func (rebuildHeaderTestTransform) apply(string, []byte) ([]byte, error) {
	return []byte("int Entry(void) { return 1; }\n"), nil
}
