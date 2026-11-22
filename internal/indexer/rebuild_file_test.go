package indexer

import (
	"os"
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
	facts, err := store_pebble.DecodeParseFacts(saved)
	require.NoError(t, err)
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
	cachedBytes, err := store_pebble.EncodeParseFacts(cached)
	require.NoError(t, err)
	require.Equal(t, saved, cachedBytes)
	_, reused, err = idx.loadRebuildFile(ctx, src, store, waiting, recipe, rebuildTransformBounds{})
	require.ErrorIs(t, err, ErrBuildInputChanged)
	require.False(t, reused)
	_, err = store.read(waitingKey)
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
	require.ErrorIs(t, err, ErrBuildInputChanged, "the old entry cannot stand in for a v2 receipt")
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
			payload, err := store_pebble.EncodeParseFacts(store_pebble.ParseFacts{
				File: graph.FileMetaRow{FilePath: path}, EffectiveLanguage: tc.language,
			})
			require.NoError(t, err)
			require.NoError(t, store.write(key, payload))
			got, reused, err := idx.loadRebuildFile(ctx, nil, store, input, recipe, rebuildTransformBounds{})
			require.ErrorIs(t, err, errParseEntryCorrupt)
			require.Empty(t, got)
			require.False(t, reused)
		})
	}
}
