package indexer

import (
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
