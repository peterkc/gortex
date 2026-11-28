package indexer

import (
	"iter"
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
	b, err := store_pebble.OpenBuild(t.TempDir(), store_pebble.FullProfile)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, b.Close()) })
	reader := rebuildContractReceiptReader{idx: idx, job: job, store: store, linkView: b.RebuildView(), census: &rebuildExtractorCensus{
		byPath: map[string]string{file: "yaml"}, originByGraphPath: map[string]string{file: file},
	}}
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
			reader := rebuildContractReceiptReader{idx: idx, linkView: b.RebuildView(), census: &rebuildExtractorCensus{
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

func TestRebuildSpringConfigCheckedSkipSeals(t *testing.T) {
	for _, mode := range []string{"legacy", "isolated", "forged-path", "forged-digest", "forged-repo", "no-authority"} {
		t.Run(mode, func(t *testing.T) {
			ctx := t.Context()
			idx := newTestIndexer(graph.New())
			idx.SetRootPath(t.TempDir())
			t.Cleanup(idx.Close)
			b, err := store_pebble.OpenBuild(t.TempDir(), store_pebble.FullProfile)
			require.NoError(t, err)
			t.Cleanup(func() { require.NoError(t, b.Close()) })
			head, err := store_pebble.OpenHead(t.TempDir(), rebuildTestIncarnation, store_pebble.FullProfile)
			require.NoError(t, err)
			t.Cleanup(func() { require.NoError(t, head.Close()) })
			const config, goFile, other = "application.yml", "main.go", "values.yml"
			fact := rebuildClassificationFact{Language: "yaml", Size: 64, MaxFileSize: 32}
			idx.registry.Register(languages.NewYAMLExtractor())
			idx.config.MaxFileSize = fact.MaxFileSize
			require.NoError(t, idx.checkRebuildClassification(config, fact, idx.newContentAdmissionGate()))
			isolate := mode == "isolated"
			n := rebuildCheckedSkipNode(config, fact)
			require.NotNil(t, n)
			if isolate {
				require.NoError(t, b.AddRebuildHeldSkip(n))
				require.NoError(t, idx.checkRebuildClassification(other, fact, idx.newContentAdmissionGate()))
				require.NoError(t, b.AddRebuildHeldSkip(rebuildCheckedSkipNode(other, fact)))
			} else {
				b.AddNode(n)
			}
			parsed := store_pebble.ParseFacts{EffectiveLanguage: "go", File: graph.FileMetaRow{FilePath: goFile, Size: 1, NodeCount: 1}, Nodes: []*graph.Node{{ID: goFile, FilePath: goFile, Kind: graph.KindFile, Language: "go"}}}
			require.NoError(t, b.AddParsedFile("", parsed))
			manifest, err := store_pebble.PrepareManifest(ctx, t.TempDir(), []store_pebble.ManifestFile{{Path: goFile, Language: "go", Kind: 'f', Size: 1}}, store_pebble.Witness{
				Input: store_pebble.HeadDigest{1}, Recipe: store_pebble.HeadDigest{2}, Stage: store_pebble.StageManifest,
				Profile: store_pebble.FullProfile, PlatformCatalogRevision: graph.PlatformCatalogRevision,
			}, rebuildTestIncarnation)
			require.NoError(t, err)
			_, err = head.PublishStage(ctx, manifest)
			require.NoError(t, err)
			files, err := b.PrepareFileGraph(ctx, t.TempDir(), manifest.Witness, rebuildTestIncarnation)
			require.NoError(t, err)
			fileBase, err := b.RetainFileGraph(ctx, files)
			require.NoError(t, err)
			_, err = head.PublishStage(ctx, files)
			require.NoError(t, err)
			if isolate {
				require.NoError(t, b.IsolateRebuildLinks(ctx))
			}
			r := rebuildContractReceiptReader{idx: idx, linkView: b.RebuildView(), job: rebuildFileStages{classifications: map[string]rebuildClassificationFact{config: fact}},
				census: &rebuildExtractorCensus{admitted: 1, byPath: map[string]string{goFile: "go"}, originByGraphPath: map[string]string{goFile: goFile, config: config}}}
			if isolate {
				r.job.classifications[other] = fact
				r.census.originByGraphPath[other] = other
			}
			want := store_pebble.RebuildSpringConfigUnknownOmission("", config)
			forged := mode == "forged-path" || mode == "forged-digest" || mode == "forged-repo"
			var tables store_pebble.StageTables
			err = r.withRebuildContractProducer(ctx, b, func(func(contracts.Contract) error) error { return nil }, make(rebuildEarlyContractIDs),
				func(preflight iter.Seq2[store_pebble.RebuildContractBatch, error], seal func(string, store_pebble.RelationsBase) (store_pebble.StageTables, error)) error {
					var omitted []store_pebble.RebuildContractOmission
					for batch, err := range preflight {
						require.NoError(t, err)
						omitted = append(omitted, batch.Omitted...)
					}
					require.Equal(t, []store_pebble.RebuildContractOmission{want}, omitted)
					langs, err := r.deferredContractLanguages()
					require.NoError(t, err)
					if isolate {
						require.NoError(t, b.RestoreRebuildDeclarations(ctx))
					}
					relations, err := b.PrepareRelations(ctx, t.TempDir(), fileBase, r.extractorOf)
					require.NoError(t, err)
					relations.Witness.Relations = &store_pebble.RelationCoverage{DeferredLanguages: langs}
					base, err := b.RetainRelations(ctx, fileBase, relations)
					require.NoError(t, err)
					_, err = head.PublishStage(ctx, relations)
					require.NoError(t, err)
					if forged || mode == "no-authority" {
						o := want
						switch mode {
						case "forged-path":
							o = store_pebble.RebuildSpringConfigUnknownOmission("", "application-other.yml")
						case "forged-digest":
							o = store_pebble.RebuildSpringConfigHeldOutOmission("", config)
						case "forged-repo":
							o = store_pebble.RebuildSpringConfigUnknownOmission("other", config)
						}
						batches := func(yield func(store_pebble.RebuildContractBatch, error) bool) {
							yield(store_pebble.RebuildContractBatch{Omitted: []store_pebble.RebuildContractOmission{o}}, nil)
						}
						if forged {
							tables, err = b.PrepareContractsWithCheckedSizeSkips(ctx, t.TempDir(), base, batches, store_pebble.ContractCoverage{OmittedRecords: 1}, r.checkedSpringConfigSizeSkip)
						} else {
							tables, err = b.PrepareContracts(ctx, t.TempDir(), base, batches, store_pebble.ContractCoverage{OmittedRecords: 1})
						}
						return err
					}
					tables, err = seal(t.TempDir(), base)
					return err
				})
			if forged || mode == "no-authority" {
				require.ErrorContains(t, err, "contract omission lacks admitted file scope")
				require.Zero(t, tables)
				return
			}
			require.NoError(t, err)
			require.EqualValues(t, 1, tables.Witness.Contracts.OmittedRecords)
			_, err = head.PublishStage(ctx, tables)
			require.NoError(t, err)
			snap := head.OpenSnapshot()
			defer snap.Close()
			partial, err := snap.ContractCoverageContext(ctx, contracts.ReadScope{FilePaths: []string{config}, Types: []contracts.ContractType{contracts.ContractType(graph.EdgeReadsConfig)}}, true)
			require.ErrorIs(t, err, contracts.ErrContractCoverageIncomplete)
			require.True(t, partial)
		})
	}
}

func TestRebuildSpringConfigWrapperRefusesUnprovedEdges(t *testing.T) {
	for _, mode := range []string{"non-java reader", "unproved target", "both proved"} {
		t.Run(mode, func(t *testing.T) {
			ctx := t.Context()
			idx := newTestIndexer(graph.New())
			idx.SetRootPath(t.TempDir())
			idx.registry.Register(languages.NewJavaExtractor())
			t.Cleanup(idx.Close)
			b, err := store_pebble.OpenBuild(t.TempDir(), store_pebble.FullProfile)
			require.NoError(t, err)
			t.Cleanup(func() { require.NoError(t, b.Close()) })
			file, lang := "Reader.java", "java"
			if mode == "non-java reader" {
				file, lang = "reader.go", "go"
				b.AddNode(&graph.Node{ID: "Marker.java", FilePath: "Marker.java", Kind: graph.KindFile, Language: "java"})
			}
			reader := &graph.Node{ID: file + "::Reader", Name: "Reader", FilePath: file, Kind: graph.KindType, Language: lang, StartLine: 1,
				Meta: map[string]any{"spring_config_keys": []string{"service.port"}}}
			target := &graph.Node{ID: "cfg::spring::service.port", Name: "service.port", FilePath: file, Kind: graph.KindConfigKey, StartLine: 1}
			parsed := store_pebble.ParseFacts{EffectiveLanguage: lang, File: graph.FileMetaRow{FilePath: file}, Nodes: []*graph.Node{
				{ID: file, FilePath: file, Language: lang, Kind: graph.KindFile}, reader,
			}}
			if mode != "unproved target" {
				parsed.Nodes = append(parsed.Nodes, target)
			}
			parsed.File.NodeCount = len(parsed.Nodes)
			facts := &contracts.FileFacts{
				Shapes: map[string]*contracts.Shape{reader.ID: nil}, Handlers: map[string]contracts.HandlerFacts{},
			}
			wire, err := store_pebble.EncodeContractParseFacts(parsed, facts)
			require.NoError(t, err)
			input := rebuildInput{Path: file, Language: lang}
			job := rebuildFileStages{inputs: []rebuildInput{input}, recipes: map[string]rebuildDigest{lang: {1}}}
			store := rebuildParseStore{dir: t.TempDir(), limit: 1 << 20}
			require.NoError(t, store.write(input.parseKey(idx.rebuildExtractorRecipe(job.recipes[lang], input)), wire))
			r := rebuildContractReceiptReader{idx: idx, job: job, store: store, linkView: b.RebuildView(), census: &rebuildExtractorCensus{byPath: map[string]string{file: lang}, originByGraphPath: map[string]string{file: file}}}
			b.AddBatch(parsed.Nodes, nil)
			b.AddNode(target)
			var edges []*graph.Edge
			var omissions []store_pebble.RebuildContractOmission
			require.NoError(t, b.WithRebuildContractRecords(ctx, func(records *store_pebble.RebuildContractRecords) error {
				return r.visitSpringConfig(ctx, b.RebuildView(), records, func(batch store_pebble.RebuildContractBatch) error {
					edges = append(edges, batch.Edges...)
					omissions = append(omissions, batch.Omitted...)
					return nil
				})
			}))
			e := contracts.SpringConfigReadEdge(reader, target.ID, "service.port")
			if mode == "both proved" {
				require.Equal(t, []*graph.Edge{e}, edges)
				require.Empty(t, omissions)
			} else {
				require.Empty(t, edges)
				require.Equal(t, []store_pebble.RebuildContractOmission{store_pebble.RebuildContractEdgeOmission("", contracts.ContractType(graph.EdgeReadsConfig), target.ID, e)}, omissions)
			}
		})
	}
}
