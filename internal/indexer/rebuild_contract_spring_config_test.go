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
		return reader.visitSpringConfig(ctx, b.RebuildView(), records, func(batch store_pebble.RebuildContractBatch) error {
			nodes = append(nodes, batch.Nodes...)
			return nil
		})
	}))
	require.Equal(t, contracts.SpringFileFacts(file, src), nodes)
	require.Equal(t, parses, parser.ParseCount())
	delete(reader.census.originByGraphPath, file)
	err = b.WithRebuildContractRecords(ctx, func(records *store_pebble.RebuildContractRecords) error {
		return reader.visitSpringConfig(ctx, b.RebuildView(), records, func(store_pebble.RebuildContractBatch) error {
			t.Fatal("emitted from an unadmitted config file")
			return nil
		})
	})
	require.ErrorIs(t, err, graph.ErrContractProjectionStale)
}

func TestRebuildSpringConfigLabelsSkippedConfigFile(t *testing.T) {
	for _, skipped := range []bool{false, true} {
		name := "no skipped file"
		if skipped {
			name = "checked size skip"
		}
		t.Run(name, func(t *testing.T) {
			ctx := t.Context()
			idx := newTestIndexer(graph.New())
			idx.SetRootPath(t.TempDir())
			t.Cleanup(idx.Close)
			b, err := store_pebble.OpenBuild(t.TempDir(), store_pebble.FullProfile)
			require.NoError(t, err)
			t.Cleanup(func() { require.NoError(t, b.Close()) })
			const javaFile, configFile = "Bean.java", "application.yml"
			b.AddBatch([]*graph.Node{
				{ID: javaFile, FilePath: javaFile, Kind: graph.KindFile, Language: "java"},
				{ID: javaFile + "::Bean", FilePath: javaFile, Kind: graph.KindType, Language: "java", StartLine: 1,
					Meta: map[string]any{"spring_config_keys": []string{"service.*"}}},
			}, nil)
			reader := rebuildContractReceiptReader{idx: idx, census: &rebuildExtractorCensus{
				byPath: map[string]string{javaFile: "java"}, originByGraphPath: map[string]string{javaFile: javaFile},
			}}
			var want []store_pebble.RebuildContractOmission
			if skipped {
				reader.census.originByGraphPath[configFile] = configFile
				reader.job.classifications = map[string]rebuildClassificationFact{configFile: {Language: "yaml", Size: 64, MaxFileSize: 32}}
				b.AddNode(&graph.Node{ID: configFile, FilePath: configFile, Kind: graph.KindFile, Language: "yaml"})
				want = []store_pebble.RebuildContractOmission{
					store_pebble.RebuildSpringConfigUnknownOmission("", configFile),
					store_pebble.RebuildSpringConfigUnknownOmission("", javaFile),
				}
			}
			var actual []store_pebble.RebuildContractOmission
			require.NoError(t, b.WithRebuildContractRecords(ctx, func(records *store_pebble.RebuildContractRecords) error {
				return reader.visitSpringConfig(ctx, b.RebuildView(), records, func(batch store_pebble.RebuildContractBatch) error {
					require.Empty(t, batch.Nodes)
					require.Empty(t, batch.Edges)
					actual = append(actual, batch.Omitted...)
					return nil
				})
			}))
			require.ElementsMatch(t, want, actual)
		})
	}
}
