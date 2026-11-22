package indexer

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/zzet/gortex/internal/config"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/parser"
	"github.com/zzet/gortex/internal/parser/languages"
	"go.uber.org/zap"
)

func TestRebuildTransformSpillAndRefusal(t *testing.T) {
	ctx := t.Context()
	root, src := rebuildInputFixture(t)
	const path = "source.go"
	raw := []byte("package p\n")
	require.NoError(t, os.WriteFile(filepath.Join(root, path), raw, 0o600))
	input, _, err := readRebuildInput(ctx, src, path, "go", 1024)
	require.NoError(t, err)
	exe, err := os.Executable()
	require.NoError(t, err)
	t.Setenv("GORTEX_REBUILD_TEST_TRANSFORM", "1")
	cfg := config.Default().Index
	cfg.Transforms = []config.TransformRule{{
		Name: "fixture", Command: []string{exe, "-test.run=^TestRebuildTransformChild$"},
	}}
	reg := parser.NewRegistry()
	reg.Register(languages.NewGoExtractor())
	idx := New(graph.New(), reg, cfg, zap.NewNop())
	idx.installRebuildParsePolicy(freezeRebuildParsePolicy(root, cfg))
	idx.SetRootPath(root)
	t.Cleanup(idx.Close)

	bounds := rebuildTransformBounds{Dir: t.TempDir(), CaptureBytes: 64, FileBytes: 64 << 10}
	legacy := idx.transforms.run(path, raw)
	want := rebuildTransformTestBytes()
	require.Equal(t, want, legacy, "native route must execute the transform")
	got, spilled, err := idx.transforms.runRebuild(ctx, path, raw, bounds)
	require.NoError(t, err)
	require.Equal(t, legacy, got)
	require.EqualValues(t, len(want), spilled, "spill receipt records bytes actually saved")
	entries, err := os.ReadDir(bounds.Dir)
	require.NoError(t, err)
	require.Empty(t, entries)

	store := rebuildParseStore{dir: t.TempDir(), limit: 1 << 20}
	recipe := rebuildDigest{2}
	bounds.FileBytes = 1024
	_, reused, err := idx.loadRebuildFile(ctx, src, store, input, recipe, bounds)
	require.ErrorIs(t, err, ErrBuildTransformBounds)
	require.ErrorContains(t, err, "limit 1024")
	require.False(t, reused)
	_, err = store.read(input.parseKey(idx.rebuildExtractorRecipe(recipe, input)))
	require.ErrorIs(t, err, os.ErrNotExist)
	entries, err = os.ReadDir(bounds.Dir)
	require.NoError(t, err)
	require.Empty(t, entries)
}

func rebuildTransformTestBytes() []byte {
	b := []byte("package p\nfunc Transformed() {}\n")
	return append(b, bytes.Repeat([]byte("// padding\n"), 512)...)
}

func TestRebuildTransformChild(t *testing.T) {
	if os.Getenv("GORTEX_REBUILD_TEST_TRANSFORM") != "1" {
		t.Skip("worker entry point")
	}
	if _, err := os.Stdout.Write(rebuildTransformTestBytes()); err != nil {
		os.Exit(2)
	}
	os.Exit(0)
}
