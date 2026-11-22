package indexer

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/zzet/gortex/internal/indexer/merkle"
	"github.com/zzet/gortex/internal/indexer/source"
)

func rebuildInputFixture(t *testing.T) (string, *source.FilesystemSource) {
	t.Helper()
	root := t.TempDir()
	src, err := source.NewFilesystemSource(root)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, src.Close()) })
	return root, src
}

func TestRebuildInputKinds(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("creating a symlink requires a Windows privilege the test runner may not have")
	}
	root, src := rebuildInputFixture(t)
	const rel = "a.go"
	target := filepath.Join(root, rel)
	require.NoError(t, os.WriteFile(target, []byte("missing"), 0o600))
	regular, data, err := readRebuildInput(t.Context(), src, rel, "go", 32)
	require.NoError(t, err)
	require.Equal(t, "missing", string(data))
	require.Equal(t, int64(7), regular.Size)
	require.Equal(t, merkle.RegularFile, regular.Kind)

	require.NoError(t, os.Remove(target))
	require.NoError(t, os.Symlink("missing", target))
	link, data, err := readRebuildInput(t.Context(), src, rel, "go", 32)
	require.NoError(t, err)
	require.Equal(t, "missing", string(data))
	require.Equal(t, int64(7), link.Size)
	require.Equal(t, merkle.Symlink, link.Kind)
	require.Equal(t, regular.Content, link.Content)
	recipe := rebuildDigest{1}
	require.NotEqual(t, regular.parseKey(recipe), link.parseKey(recipe))
	regularRoot, err := rebuildInputRoot([]rebuildInput{regular})
	require.NoError(t, err)
	linkRoot, err := rebuildInputRoot([]rebuildInput{link})
	require.NoError(t, err)
	require.NotEmpty(t, regularRoot)
	require.NotEmpty(t, linkRoot)
	require.NotEqual(t, regularRoot, linkRoot)
	data, err = readFrozenRebuildInput(t.Context(), src, regular)
	require.ErrorIs(t, err, ErrBuildInputChanged)
	require.Nil(t, data)
}

func TestRebuildInputOrderAndRelocation(t *testing.T) {
	read := func(order []string) []rebuildInput {
		root, src := rebuildInputFixture(t)
		var inputs []rebuildInput
		for _, name := range order {
			require.NoError(t, os.WriteFile(filepath.Join(root, name), []byte("package x"), 0o600))
			input, data, err := readRebuildInput(t.Context(), src, name, "go", 32)
			require.NoError(t, err)
			require.Equal(t, "package x", string(data))
			inputs = append(inputs, input)
		}
		return inputs
	}
	a, b := read([]string{"a.go", "b.go"}), read([]string{"b.go", "a.go"})
	require.Len(t, a, 2)
	require.Len(t, b, 2)
	require.Equal(t, a[0], b[1])
	require.Equal(t, a[1], b[0])
	require.Equal(t, a[0].Content, a[1].Content)
	recipe := rebuildDigest{1}
	require.NotEqual(t, a[0].parseKey(recipe), a[1].parseKey(recipe))
	require.Equal(t, a[0].parseKey(recipe), b[1].parseKey(recipe))
	require.NotEqual(t, a[0].parseKey(recipe), a[0].parseKey(rebuildDigest{2}))
	rootA, err := rebuildInputRoot(a)
	require.NoError(t, err)
	rootB, err := rebuildInputRoot(b)
	require.NoError(t, err)
	require.NotEmpty(t, rootA)
	require.Equal(t, rootA, rootB)
	a[0].Language = "other"
	otherRoot, err := rebuildInputRoot(a)
	require.NoError(t, err)
	require.NotEqual(t, rootA, otherRoot)
	_, err = rebuildInputRoot([]rebuildInput{a[0], a[0]})
	require.ErrorContains(t, err, "duplicate build input path")
}

func TestRebuildFrozenInputSameMtime(t *testing.T) {
	root, src := rebuildInputFixture(t)
	const name = "a.go"
	p := filepath.Join(root, name)
	stamp := time.Unix(1_700_000_000, 0)
	require.NoError(t, os.WriteFile(p, []byte("old"), 0o600))
	require.NoError(t, os.Chtimes(p, stamp, stamp))
	in, data, err := readRebuildInput(t.Context(), src, name, "go", 16)
	require.NoError(t, err)
	require.Equal(t, "old", string(data))
	data, err = readFrozenRebuildInput(t.Context(), src, in)
	require.NoError(t, err)
	require.Equal(t, "old", string(data))

	for _, next := range []string{"new", "longer"} {
		require.NoError(t, os.WriteFile(p, []byte(next), 0o600))
		require.NoError(t, os.Chtimes(p, stamp, stamp))
		st, err := os.Stat(p)
		require.NoError(t, err)
		require.True(t, st.ModTime().Equal(stamp))
		data, err = readFrozenRebuildInput(t.Context(), src, in)
		require.ErrorIs(t, err, ErrBuildInputChanged)
		require.Nil(t, data)
	}
	data, err = readFrozenRebuildInput(t.Context(), src, rebuildInput{Path: "../outside", Language: "go"})
	require.Error(t, err)
	require.Nil(t, data)
}
