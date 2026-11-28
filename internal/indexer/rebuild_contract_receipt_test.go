package indexer

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/graph/store_pebble"
)

func TestRebuildContractReceiptForLinkedDeclaration(t *testing.T) {
	const path = "schema.go"
	ctx, root := t.Context(), t.TempDir()
	idx := newTestIndexer(graph.New())
	idx.SetRootPath(root)
	t.Cleanup(idx.Close)
	src := []byte("package p\ntype Payload struct {\n ID int\n}\nfunc Handler() {}\n")
	name := filepath.Join(root, path)
	require.NoError(t, os.WriteFile(name, src, 0o600))
	ext, ok := idx.registry.GetByLanguage("go")
	require.True(t, ok)
	base, facts, err := idx.extractPreparedRebuildFileWithContractFacts(ctx, path, ext, src, src)
	require.NoError(t, err)
	wire, err := store_pebble.EncodeContractParseFacts(base, facts)
	require.NoError(t, err)
	input := rebuildInput{Path: path, Language: "go", Size: int64(len(src))}
	job := rebuildFileStages{inputs: []rebuildInput{input}, recipes: map[string]rebuildDigest{"go": {1}}}
	store := rebuildParseStore{dir: t.TempDir(), limit: 1 << 20}
	key := input.parseKey(idx.rebuildExtractorRecipe(job.recipes[input.Language], input))
	require.NoError(t, store.write(key, wire))
	require.NoError(t, os.Remove(name))
	b, err := store_pebble.OpenBuild(t.TempDir(), store_pebble.FullProfile)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, b.Close()) })
	reader := rebuildContractReceiptReader{idx: idx, job: job, store: store, linkView: b.RebuildView(),
		census: &rebuildExtractorCensus{byPath: map[string]string{path: "go"}, originByGraphPath: map[string]string{path: path}}}
	var matched int
	for _, n := range base.Nodes {
		if n.Kind != graph.KindType && n.Kind != graph.KindFunction {
			continue
		}
		matched++
		linked := *n
		linked.Meta = map[string]any{"linked": true}
		_, got, err := reader.forLinkedDeclaration(ctx, &linked)
		require.NoError(t, err)
		switch n.Kind {
		case graph.KindType:
			require.NotNil(t, facts.Shapes[n.ID])
			require.Equal(t, facts.Shapes[n.ID], got.Shapes[n.ID])
		case graph.KindFunction:
			require.Contains(t, got.Handlers, n.ID)
			require.Equal(t, facts.Handlers[n.ID], got.Handlers[n.ID])
		}
		linked.EndLine++
		_, _, err = reader.forLinkedDeclaration(ctx, &linked)
		require.ErrorIs(t, err, graph.ErrContractProjectionStale)
		linked.EndLine--
		linked.FilePath = "unadmitted.go"
		_, _, err = reader.forLinkedDeclaration(ctx, &linked)
		require.ErrorIs(t, err, graph.ErrContractProjectionStale)
	}
	require.Equal(t, 2, matched)
}
