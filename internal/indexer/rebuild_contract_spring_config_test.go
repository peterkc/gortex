package indexer

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/zzet/gortex/internal/contracts"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/graph/store_pebble"
	"github.com/zzet/gortex/internal/parser"
	"github.com/zzet/gortex/internal/parser/languages"
)

func TestRebuildSpringConfigUsesCheckedReceipt(t *testing.T) {
	ctx, root := t.Context(), t.TempDir()
	const file = "application.yml"
	src := []byte("server:\n  port: 8080\n")
	sourcePath := filepath.Join(root, file)
	require.NoError(t, os.WriteFile(sourcePath, src, 0o600))
	idx := newTestIndexer(graph.New())
	idx.SetRootPath(root)
	t.Cleanup(idx.Close)
	ext := languages.NewYAMLExtractor()
	idx.registry.Register(ext)
	parsed, facts, err := idx.extractPreparedRebuildFileWithContractFacts(ctx, file, ext, src, src)
	require.NoError(t, err)
	require.Len(t, facts.ConfigKeys, 1)
	input := rebuildInput{Path: file, Language: "yaml", Size: int64(len(src))}
	job := rebuildFileStages{inputs: []rebuildInput{input}, recipes: map[string]rebuildDigest{"yaml": {1}}}
	store := rebuildParseStore{dir: t.TempDir(), limit: 1 << 20}
	wire, err := store_pebble.EncodeContractParseFacts(parsed, facts)
	require.NoError(t, err)
	require.NoError(t, store.write(input.parseKey(idx.rebuildExtractorRecipe(job.recipes[input.Language], input)), wire))
	reader := rebuildContractReceiptReader{idx: idx, job: job, store: store, census: &rebuildExtractorCensus{
		byPath: map[string]string{file: "yaml"}, originByGraphPath: map[string]string{file: file},
	}}
	b, err := store_pebble.OpenBuild(t.TempDir(), store_pebble.FullProfile)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, b.Close()) })
	require.NoError(t, b.AddParsedFile("", parsed))
	require.NoError(t, os.Remove(sourcePath))
	parses := parser.ParseCount()
	var nodes []*graph.Node
	require.NoError(t, b.WithRebuildContractRecords(ctx, func(records *store_pebble.RebuildContractRecords) error {
		return reader.visitSpringConfig(ctx, records, func(batch store_pebble.RebuildContractBatch) error {
			nodes = append(nodes, batch.Nodes...)
			return nil
		})
	}))
	require.Equal(t, contracts.SpringFileFacts(file, src), nodes)
	require.Equal(t, parses, parser.ParseCount())
	delete(reader.census.originByGraphPath, file)
	err = b.WithRebuildContractRecords(ctx, func(records *store_pebble.RebuildContractRecords) error {
		return reader.visitSpringConfig(ctx, records, func(store_pebble.RebuildContractBatch) error {
			t.Fatal("emitted from an unadmitted config file")
			return nil
		})
	})
	require.ErrorIs(t, err, graph.ErrContractProjectionStale)
}
