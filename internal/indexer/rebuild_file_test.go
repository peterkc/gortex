package indexer

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/zeebo/blake3"
	"github.com/zzet/gortex/internal/config"
	"github.com/zzet/gortex/internal/contracts"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/graph/store_pebble"
	"github.com/zzet/gortex/internal/parser"
	"github.com/zzet/gortex/internal/parser/languages"
	"github.com/zzet/gortex/internal/resolver"
	"go.uber.org/zap"
)

func TestRebuildFileFactsRemainReachableAfterSourceEdit(t *testing.T) {
	ctx := t.Context()
	root, src := rebuildInputFixture(t)
	old := []byte("package demo\nconst Value = 7\nfunc Before() int { return Value }\n")
	new := []byte("package demo\nconst Value = 8\nfunc After() int { return Value }\n")
	const parsedPath, waitingPath = "parsed.go", "waiting.go"
	prepare := func(name string) rebuildInput {
		t.Helper()
		require.NoError(t, os.WriteFile(filepath.Join(root, name), old, 0o600))
		input, _, err := readRebuildInput(ctx, src, name, "go", 1<<20)
		require.NoError(t, err)
		return input
	}
	parsed, waiting := prepare(parsedPath), prepare(waitingPath)
	store := rebuildParseStore{dir: t.TempDir(), limit: 1 << 20}
	recipe := rebuildDigest{1}
	reg := parser.NewRegistry()
	reg.Register(languages.NewGoExtractor())
	idx := New(graph.New(), reg, config.Default().Index, zap.NewNop())
	idx.installRebuildParsePolicy(freezeRebuildParsePolicy(root, config.Default().Index))
	idx.SetRootPath(root)
	t.Cleanup(idx.Close)
	parsedKey := parsed.parseKey(idx.rebuildExtractorRecipe(recipe, parsed))
	waitingKey := waiting.parseKey(idx.rebuildExtractorRecipe(recipe, waiting))

	_, err := store.read(parsedKey)
	require.ErrorIs(t, err, os.ErrNotExist)
	_, reused, err := idx.loadRebuildFile(ctx, src, store, parsed, recipe, rebuildTransformBounds{})
	require.NoError(t, err)
	require.False(t, reused)
	saved, err := store.read(parsedKey)
	require.NoError(t, err)
	facts := decodeRebuildTestParseFacts(t, saved)
	require.Equal(t, parsedPath, facts.File.FilePath)
	require.Equal(t, "go", facts.EffectiveLanguage)
	require.Equal(t, len(old), facts.File.Size)
	require.Positive(t, facts.File.NodeCount)
	require.Positive(t, len(facts.Edges))
	require.Positive(t, len(facts.Symbols))
	require.Contains(t, facts.Constants, graph.ConstantValueRow{FilePath: parsedPath, NodeID: parsedPath + "::Value", Value: "7"})
	names := make([]string, 0, facts.File.NodeCount)
	for _, n := range facts.Nodes {
		names = append(names, n.Name)
	}
	require.Contains(t, names, "Before")
	require.NotContains(t, names, "After")
	_, err = store.read(waitingKey)
	require.ErrorIs(t, err, os.ErrNotExist)

	for _, name := range []string{parsedPath, waitingPath} {
		require.NoError(t, os.WriteFile(filepath.Join(root, name), new, 0o600))
	}
	cached, reused, err := idx.loadRebuildFile(ctx, src, store, parsed, recipe, rebuildTransformBounds{})
	require.NoError(t, err)
	require.True(t, reused)
	require.Equal(t, facts, cached)
	_, reused, err = idx.loadRebuildFile(ctx, src, store, waiting, recipe, rebuildTransformBounds{})
	require.ErrorIs(t, err, ErrBuildInputChanged)
	require.False(t, reused)
	_, err = store.read(waitingKey)
	require.ErrorIs(t, err, os.ErrNotExist)
}

func TestRebuildFileBindsCloneExtractionRecipe(t *testing.T) {
	ctx := t.Context()
	root, src := rebuildInputFixture(t)
	const path = "a.go"
	file := filepath.Join(root, path)
	original := []byte("package p\nfunc A(){" + strings.Repeat(";", 73) + "}\nfunc B(){" + strings.Repeat(";", 74) + "}\nfunc Tiny(){}\n")
	require.NoError(t, os.WriteFile(file, original, 0o600))
	input, _, err := readRebuildInput(ctx, src, path, "go", 1<<20)
	require.NoError(t, err)
	newIndexer := func(toggle *bool, threshold float64) *Indexer {
		cfg := config.Default().Index
		cfg.Coverage.Clones.Enabled = toggle
		cfg.Coverage.Clones.Threshold = threshold
		reg := parser.NewRegistry()
		reg.Register(languages.NewGoExtractor())
		idx := New(graph.New(), reg, cfg, zap.NewNop())
		idx.installRebuildParsePolicy(freezeRebuildParsePolicy(root, cfg))
		idx.SetRootPath(root)
		t.Cleanup(idx.Close)
		return idx
	}
	yes, no := true, false
	defaultOn := newIndexer(nil, 0)
	explicitOn := newIndexer(&yes, 0.95)
	off := newIndexer(&no, 0)
	base := rebuildDigest{1}
	onRecipe := defaultOn.rebuildExtractorRecipe(base, input)
	require.Equal(t, defaultOn.rebuildBaseExtractorRecipe(base, input), explicitOn.rebuildBaseExtractorRecipe(base, input))
	require.NotEqual(t, onRecipe, explicitOn.rebuildExtractorRecipe(base, input), "the contract dependency stamp records explicit clone settings")
	require.NotEqual(t, onRecipe, off.rebuildExtractorRecipe(base, input))
	store := rebuildParseStore{dir: t.TempDir(), limit: 1 << 20}
	facts, reused, err := defaultOn.loadRebuildFile(ctx, src, store, input, base, rebuildTransformBounds{})
	require.NoError(t, err)
	require.False(t, reused)
	require.Len(t, facts.Shingles, 2)
	require.Len(t, facts.Shingles[path+"::A"], 7)
	require.Equal(t, facts.Shingles[path+"::A"], facts.Shingles[path+"::B"])
	counts := make(map[string]any)
	for _, n := range facts.Nodes {
		if n.Kind == graph.KindFunction {
			counts[n.ID] = n.Meta[cloneTokensMetaKey]
		}
	}
	require.Equal(t, map[string]any{path + "::A": 79, path + "::B": 80, path + "::Tiny": nil}, counts)
	encoded, err := store_pebble.EncodeParseFacts(facts)
	require.NoError(t, err)

	require.NoError(t, os.WriteFile(file, []byte("package p\nfunc Replacement(){}\n"), 0o600))
	cached, reused, err := newIndexer(nil, 0).loadRebuildFile(ctx, src, store, input, base, rebuildTransformBounds{})
	require.NoError(t, err)
	require.True(t, reused)
	replayed, err := store_pebble.EncodeParseFacts(cached)
	require.NoError(t, err)
	require.Equal(t, encoded, replayed)
	_, reused, err = off.loadRebuildFile(ctx, src, store, input, base, rebuildTransformBounds{})
	require.ErrorIs(t, err, ErrBuildInputChanged)
	require.False(t, reused)

	require.NoError(t, os.WriteFile(file, original, 0o600))
	disabled, reused, err := off.loadRebuildFile(ctx, src, store, input, base, rebuildTransformBounds{})
	require.NoError(t, err)
	require.False(t, reused)
	require.Empty(t, disabled.Shingles)
	for _, n := range disabled.Nodes {
		require.NotContains(t, n.Meta, cloneBodyMetaKey)
		require.NotContains(t, n.Meta, cloneTokensMetaKey)
	}
}

func TestRebuildFileOversizeRefusalPrecedesCanceledWrite(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	root, src := rebuildInputFixture(t)
	const path = "routes.go"
	writeFile(t, filepath.Join(root, path), "package p\n")
	input, _, err := readRebuildInput(ctx, src, path, "go", 1024)
	require.NoError(t, err)
	idx := newTestIndexer(graph.New())
	t.Cleanup(idx.Close)
	payload := encodeRebuildTestParseFacts(t, store_pebble.ParseFacts{
		File: graph.FileMetaRow{FilePath: path}, EffectiveLanguage: "go",
	})
	store := rebuildParseStore{dir: t.TempDir(), limit: len(payload) - 1}
	recipe := rebuildDigest{1}
	_, _, reused, err := idx.loadRebuildFileWith(ctx, src, store, input, recipe, rebuildTransformBounds{},
		func(context.Context, rebuildDigest, string, parser.Extractor, []byte, []byte) ([]byte, error) {
			cancel()
			return payload, nil
		})
	require.ErrorIs(t, err, errParseEntryTooLarge)
	require.False(t, reused)
	_, err = store.read(input.parseKey(idx.rebuildExtractorRecipe(recipe, input)))
	require.ErrorIs(t, err, os.ErrNotExist)
}

func TestRebuildFileRefusesUnboundEffectiveLanguage(t *testing.T) {
	ctx := t.Context()
	root, src := rebuildInputFixture(t)
	const path = "header.h"
	file := filepath.Join(root, path)
	require.NoError(t, os.WriteFile(file, []byte("int c(void);\n"), 0o600))
	input, _, err := readRebuildInput(ctx, src, path, "c", 1024)
	require.NoError(t, err)
	cfg := config.Default().Index
	reg := parser.NewRegistry()
	reg.Register(languages.NewCExtractor())
	reg.Register(languages.NewGoExtractor())
	idx := New(graph.New(), reg, cfg, zap.NewNop())
	idx.installRebuildParsePolicy(rebuildParsePolicy{})
	idx.SetRootPath(root)
	t.Cleanup(idx.Close)

	recipe := rebuildDigest{1}
	key := input.parseKey(idx.rebuildExtractorRecipe(recipe, input))
	// The previous recipe was r/v1, the base digest and zero owners. C
	// does not append the Go-only Temporal list.
	oldRecipe := blake3.Sum256(append(append([]byte{'r', 1}, recipe[:]...), 0))
	oldKey := input.parseKey(oldRecipe)
	require.NotEqual(t, oldKey, key)
	legacy, err := store_pebble.EncodeParseFacts(store_pebble.ParseFacts{File: graph.FileMetaRow{FilePath: path}})
	require.NoError(t, err)
	store := rebuildParseStore{dir: t.TempDir(), limit: 1 << 20}
	require.NoError(t, store.write(oldKey, legacy))
	require.NoError(t, os.WriteFile(file, []byte("namespace edited {}\n"), 0o600))
	got, reused, err := idx.loadRebuildFile(ctx, src, store, input, recipe, rebuildTransformBounds{})
	require.ErrorIs(t, err, ErrBuildInputChanged, "the old entry cannot stand in for a contract receipt")
	require.Empty(t, got)
	require.False(t, reused)
	_, err = store.read(key)
	require.ErrorIs(t, err, os.ErrNotExist)

	for _, tc := range []struct {
		name, language string
	}{
		{"legacy under new key", ""},
		{"unregistered extractor", "cpp"},
		{"impossible refinement", "go"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			store := rebuildParseStore{dir: t.TempDir(), limit: 1 << 20}
			payload := legacy
			if tc.language != "" {
				payload = encodeRebuildTestParseFacts(t, store_pebble.ParseFacts{
					File: graph.FileMetaRow{FilePath: path}, EffectiveLanguage: tc.language,
				})
			}
			require.NoError(t, store.write(key, payload))
			got, reused, err := idx.loadRebuildFile(ctx, nil, store, input, recipe, rebuildTransformBounds{})
			require.ErrorIs(t, err, errParseEntryCorrupt)
			require.Empty(t, got)
			require.False(t, reused)
		})
	}
}

// Base-only extraction is an oracle for the unchanged embedded v2 bytes.
func (idx *Indexer) extractPreparedRebuildFile(ctx context.Context, rel string, ext parser.Extractor, prepared []byte) (store_pebble.ParseFacts, error) {
	return idx.extractPreparedRebuildFileInto(ctx, rel, ext, prepared, prepared, nil)
}

// Synthetic graph fixtures assert graph semantics; provide the same explicit
// empty contract answers that capture would produce for those rows.
func encodeRebuildTestParseFacts(t testing.TB, base store_pebble.ParseFacts) []byte {
	t.Helper()
	facts := &contracts.FileFacts{Shapes: make(map[string]*contracts.Shape), Handlers: make(map[string]contracts.HandlerFacts)}
	for _, n := range base.Nodes {
		switch n.Kind {
		case graph.KindType, graph.KindInterface:
			facts.Shapes[n.ID] = nil
		case graph.KindFunction, graph.KindMethod:
			h := contracts.HandlerFacts{Regex: make(map[string]any), AST: make(map[string]any)}
			if strings.HasSuffix(n.FilePath, ".go") {
				h.Bindings = make(map[string]contracts.Binding)
			}
			facts.Handlers[n.ID] = h
		}
	}
	if contracts.RouterFactsRequired(base.File.FilePath, base.EffectiveLanguage, nil, nil) {
		facts.Router = &contracts.RouterFacts{}
	}
	wire, err := store_pebble.EncodeContractParseFacts(base, facts)
	require.NoError(t, err)
	return wire
}

func decodeRebuildTestParseFacts(t testing.TB, payload []byte) store_pebble.ParseFacts {
	t.Helper()
	base, _, _, err := store_pebble.DecodeContractParseFacts(payload)
	require.NoError(t, err)
	return base
}

func TestRebuildFileCodegenRelations(t *testing.T) {
	for _, changed := range []bool{false, true} {
		name := "unchanged"
		if changed {
			name = "changed after parse"
		}
		t.Run(name, func(t *testing.T) {
			ctx := t.Context()
			root, src := rebuildInputFixture(t)
			const file = "generated.go"
			writeFile(t, filepath.Join(root, file), "// Code generated by protoc. DO NOT EDIT.\n// source: api/schema.proto\npackage p\nfunc F() {}\n")
			input, _, err := readRebuildInput(ctx, src, file, "go", 1<<20)
			require.NoError(t, err)
			cfg := config.Default().Index
			enabled := true
			cfg.Coverage.Codegen.Enabled = &enabled
			reg := parser.NewRegistry()
			reg.Register(languages.NewGoExtractor())
			idx := New(graph.New(), reg, cfg, zap.NewNop())
			t.Cleanup(idx.Close)
			idx.installRebuildParsePolicy(freezeRebuildParsePolicy(root, cfg))
			idx.SetRootPath(root)
			store := rebuildParseStore{dir: t.TempDir(), limit: 1 << 20}
			facts, reused, err := idx.loadRebuildFile(ctx, src, store, input, rebuildDigest{1}, rebuildTransformBounds{})
			require.NoError(t, err)
			require.False(t, reused)
			require.Equal(t, "go", facts.EffectiveLanguage)
			want := &graph.Edge{From: file, To: "external::generator-source:api/schema.proto", Kind: graph.EdgeGeneratedBy,
				FilePath: file, Origin: graph.OriginASTResolved, Meta: map[string]any{"tool": "protoc", "source": "api/schema.proto"}}
			var generated []*graph.Edge
			for _, e := range facts.Edges {
				if e.Kind == graph.EdgeGeneratedBy {
					generated = append(generated, e)
				}
			}
			require.Equal(t, []*graph.Edge{want}, generated)

			build, dir := openRebuildTestFileStore(t)
			require.NoError(t, build.AddParsedFile("", facts))
			w := store_pebble.Witness{Input: store_pebble.HeadDigest{1}, Recipe: store_pebble.HeadDigest{2},
				Stage: store_pebble.StageManifest, Profile: store_pebble.FullProfile, PlatformCatalogRevision: graph.PlatformCatalogRevision}
			manifest, err := store_pebble.PrepareManifest(ctx, t.TempDir(), []store_pebble.ManifestFile{{Path: file, Kind: 'f', Language: "go"}}, w, rebuildTestIncarnation)
			require.NoError(t, err)
			fileGraph, err := build.PrepareFileGraph(ctx, t.TempDir(), manifest.Witness, rebuildTestIncarnation)
			require.NoError(t, err)
			base, err := build.RetainFileGraph(ctx, fileGraph)
			require.NoError(t, err)
			head, err := store_pebble.OpenHead(filepath.Join(t.TempDir(), "head"), rebuildTestIncarnation, w.Profile)
			require.NoError(t, err)
			t.Cleanup(func() { require.NoError(t, head.Close()) })
			for _, stage := range []store_pebble.StageTables{manifest, fileGraph} {
				_, err := head.PublishStage(ctx, stage)
				require.NoError(t, err)
			}

			receipts := func(yield func(store_pebble.ParseFacts, error) bool) { yield(facts, nil) }
			opts := store_pebble.RebuildLinkOptions{ScratchParent: dir, SpillBytes: 1 << 20}
			require.NoError(t, resolver.RunRebuildMain(ctx, build, opts, resolver.RebuildMainInputs{
				SelectCallers: func(page []*graph.Edge) ([]*graph.Edge, error) { return page, nil },
				Receipts:      receipts,
			}))
			require.NoError(t, runRebuildArgOf(ctx, build, receipts))
			require.NoError(t, runRebuildReturnsTo(ctx, build, receipts))
			extractor := func(path string) string {
				if path == file {
					return facts.EffectiveLanguage
				}
				return ""
			}
			require.NoError(t, build.SettleRebuildPlatformCalls(ctx, extractor))
			if changed {
				rewritten := *want
				rewritten.Context = "changed by linker"
				build.AddEdge(&rewritten)
			}
			relations, err := build.PrepareRelations(ctx, t.TempDir(), base, extractor)
			if changed {
				require.ErrorIs(t, err, store_pebble.ErrRelationsSeal)
				var refused *store_pebble.RebuildUnknownProvenanceError
				require.ErrorAs(t, err, &refused)
				require.Equal(t, graph.EdgeIdentityFor(want), refused.First)
				require.Empty(t, relations)
				snap := head.OpenSnapshot()
				t.Cleanup(func() { require.NoError(t, snap.Close()) })
				var pending *store_pebble.ErrStagePending
				require.ErrorAs(t, snap.RequireStage(store_pebble.StageRelations), &pending)
				return
			}
			require.NoError(t, err)
			_, err = head.PublishStage(ctx, relations)
			require.NoError(t, err)
			snap := head.OpenSnapshot()
			t.Cleanup(func() { require.NoError(t, snap.Close()) })
			require.Nil(t, snap.GetNode(want.To))
			require.Equal(t, []*graph.Edge{want}, snap.GetInEdges(want.To))
			generated = nil
			for _, e := range snap.GetOutEdges(file) {
				if e.Kind == graph.EdgeGeneratedBy {
					generated = append(generated, e)
				}
			}
			require.Equal(t, []*graph.Edge{want}, generated)
		})
	}
}
