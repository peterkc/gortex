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

func TestRebuildContractProducerSpringSelection(t *testing.T) {
	ctx, root := t.Context(), t.TempDir()
	const javaFile, configFile, di = "Bean.java", "application.yml", "di::Service"
	g := graph.New()
	idx := newTestIndexer(g)
	idx.SetRootPath(root)
	idx.registry.Register(languages.NewJavaExtractor())
	idx.registry.Register(languages.NewYAMLExtractor())
	t.Cleanup(idx.Close)
	b, err := store_pebble.OpenBuild(t.TempDir(), store_pebble.FullProfile)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, b.Close()) })
	head, err := store_pebble.OpenHead(t.TempDir(), rebuildTestIncarnation, store_pebble.FullProfile)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, head.Close()) })
	sources := []struct{ file, lang, text string }{
		{javaFile, "java", `package p;
@Configuration
class Bean {
    @Bean
    public Service make() { return null; }
}
class Client {
    @Value("${service.port}") int port;
    public Client(Service service) {}
}
class Existing {
    public Existing(Service service) {}
}
class ZBean {
    public Service alternate() { return null; }
}
class Service {}
`},
		{configFile, "yaml", "service:\n  port: 8080\n"},
	}
	store := rebuildParseStore{dir: t.TempDir(), limit: 16 << 20}
	job := rebuildFileStages{recipes: map[string]rebuildDigest{"java": {1}, "yaml": {2}}}
	census := &rebuildExtractorCensus{byPath: map[string]string{}, originByGraphPath: map[string]string{}}
	var manifestFiles []store_pebble.ManifestFile
	var bean *graph.Edge
	var alternate, consumer *graph.Node
	native := contracts.NewRegistry()
	for _, src := range sources {
		raw := []byte(src.text)
		require.NoError(t, os.WriteFile(filepath.Join(root, src.file), raw, 0o600))
		ext, ok := idx.registry.GetByLanguage(src.lang)
		require.True(t, ok)
		parsed, facts, err := idx.extractPreparedRebuildFileWithContractFacts(ctx, src.file, ext, raw, raw)
		require.NoError(t, err)
		input := rebuildInput{Path: src.file, Language: src.lang, Size: int64(len(raw))}
		wire, err := store_pebble.EncodeContractParseFacts(parsed, facts)
		require.NoError(t, err)
		require.NoError(t, store.write(input.parseKey(idx.rebuildExtractorRecipe(job.recipes[input.Language], input)), wire))
		job.inputs = append(job.inputs, input)
		census.admitted++
		census.byPath[src.file], census.originByGraphPath[src.file] = parsed.EffectiveLanguage, src.file
		manifestFiles = append(manifestFiles, store_pebble.ManifestFile{Path: src.file, Language: src.lang, Kind: 'f', Size: input.Size})
		require.NoError(t, b.AddParsedFile("", parsed))
		g.AddBatch(parsed.Nodes, parsed.Edges)
		native.AddAllScoped(cloneContractRecords(facts.Records), "", "", "")
		for _, edge := range parsed.Edges {
			if _, ok := springBeanFromEdge(edge); ok {
				require.Nil(t, bean)
				bean = edge
			}
		}
		for _, node := range parsed.Nodes {
			if node.Kind == graph.KindMethod && node.Name == "alternate" {
				alternate = node
			}
			if node.Kind == graph.KindMethod && enclosingClassID(node) == javaFile+"::Client" {
				consumer = node
			}
		}
	}
	require.NotNil(t, bean)
	require.NotNil(t, alternate)
	manifest, err := store_pebble.PrepareManifest(ctx, t.TempDir(), manifestFiles, store_pebble.Witness{
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

	r := rebuildContractReceiptReader{idx: idx, job: job, store: store, census: census}
	view := b.RebuildView()
	keep, err := r.proveSpringBeanFactory(ctx, view, bean)
	require.NoError(t, err)
	require.True(t, keep)
	changedBean := *bean
	changedBean.Origin = graph.OriginASTInferred
	if changedBean.Origin == bean.Origin {
		changedBean.Origin = graph.OriginASTResolved
	}
	keep, err = r.proveSpringBeanFactory(ctx, view, &changedBean)
	require.NoError(t, err)
	require.False(t, keep, "an exact DI projection alone does not prove the current Spring factory rule")
	require.NotNil(t, consumer)
	ref, _ := springBeanFromEdge(bean)
	call := springBeanCall(javaFile+"::Client", ref)
	keep, err = r.proveSpringBeanConsumer(ctx, view, consumer.ID, call)
	require.NoError(t, err)
	require.True(t, keep)
	changedMethod := *consumer
	changedMethod.Meta = map[string]any{"params_src": "Service other", "receiver": "Client"}
	b.AddNode(&changedMethod)
	keep, err = r.proveSpringBeanConsumer(ctx, view, consumer.ID, call)
	require.NoError(t, err)
	require.False(t, keep)
	b.AddNode(consumer)
	class, err := view.Node(ctx, call.From)
	require.NoError(t, err)
	require.NotNil(t, class)
	wrong := *class
	wrong.Kind = graph.KindVariable
	b.AddNode(&wrong)
	keep, err = r.proveSpringBeanConsumer(ctx, view, consumer.ID, call)
	require.NoError(t, err)
	require.False(t, keep)
	b.AddNode(class)

	// Model the quiet linked cut with a DI sibling not present in parse facts.
	// The canonical native DI ID is still backed by Bean.make, so an ID-only
	// proof would incorrectly emit both alternate-factory Calls.
	unproved := *bean
	unproved.From, unproved.To, unproved.Line = javaFile+"::ZBean", alternate.ID, alternate.StartLine
	b.AddEdge(&unproved)
	g.AddEdge(&unproved)
	// Existing Calls belong to Relations, even if the planned prune drops one.
	beanRef, _ := springBeanFromEdge(bean)
	incumbent := springBeanCall(javaFile+"::Existing", beanRef)
	incumbent.Meta = map[string]any{"ordinary": true}
	b.AddEdge(incumbent)
	g.AddEdge(incumbent)
	// A second live config reader has a hint absent from its raw declaration.
	original := g.GetNode(javaFile + "::ZBean")
	require.NotNil(t, original)
	changed := *original
	changed.Meta = map[string]any{"spring_config_keys": []string{"service.port"}}
	b.AddNode(&changed)
	g.AddNode(&changed)
	idx.extractDIContracts(native)
	idx.commitContracts(native)
	require.Len(t, native.ByID(di), 2)
	require.Equal(t, javaFile+"::Bean", g.GetNode(di).Meta["symbol_id"], "canonical DI owner")
	early := make(rebuildEarlyContractIDs)
	for n := range g.NodesByKind(graph.KindContract) {
		if n.Language == "contract" {
			early[n.ID] = n.FilePath
		}
	}
	const key = "cfg::spring::service.port"
	wantCall := springBeanCall(javaFile+"::Client", beanRef)
	var wantConfig *graph.Edge
	for _, e := range g.GetOutEdges(javaFile + "::Client") {
		if e.Kind == graph.EdgeReadsConfig {
			wantConfig = e
		}
	}
	require.Contains(t, g.GetOutEdges(wantCall.From), wantCall, "native kept bean Call")
	require.NotNil(t, wantConfig)
	require.Equal(t, key, wantConfig.To)
	require.NotNil(t, g.GetNode(key))
	var wantO []store_pebble.RebuildContractOmission
	unprovedRecord, ok := diContractFromEdge(&unproved)
	require.True(t, ok)
	wantO = append(wantO, rebuildContractRecordOmission(unprovedRecord))
	for _, cls := range []string{"Client", "Existing"} {
		var found *graph.Edge
		for _, e := range g.GetOutEdges(javaFile + "::" + cls) {
			if e.Kind == graph.EdgeCalls && e.To == alternate.ID && e.Meta["via"] == "spring.Bean" {
				found = e
			}
		}
		require.NotNil(t, found)
		wantO = append(wantO, store_pebble.RebuildContractEdgeOmission("", contracts.ContractDI, di, found))
	}
	var unprovedConfig *graph.Edge
	for _, e := range g.GetOutEdges(changed.ID) {
		if e.Kind == graph.EdgeReadsConfig {
			unprovedConfig = e
		}
	}
	require.NotNil(t, unprovedConfig)
	wantO = append(wantO, store_pebble.RebuildContractEdgeOmission("", contracts.ContractType(graph.EdgeReadsConfig), key, unprovedConfig))
	for _, src := range sources {
		require.NoError(t, os.Remove(filepath.Join(root, src.file)))
	}
	parsedBefore := parser.ParseCount()
	var stage store_pebble.StageTables
	_, err = r.withRebuildContractSources(ctx, b, t.TempDir(), rebuildGoManifest{Path: "go.mod"}, 1, func(read func(func(contracts.Contract) error) error) error {
		return r.withRebuildContractProducer(ctx, b, read, early, func(preflight iter.Seq2[store_pebble.RebuildContractBatch, error], seal func(string, store_pebble.RelationsBase) (store_pebble.StageTables, error)) error {
			var direct []*graph.Edge
			var omitted []store_pebble.RebuildContractOmission
			for batch, err := range preflight {
				require.NoError(t, err)
				omitted = append(omitted, batch.Omitted...)
				for _, e := range batch.Edges {
					if e.Kind == graph.EdgeCalls || e.Kind == graph.EdgeReadsConfig {
						direct = append(direct, e)
					}
				}
			}
			require.ElementsMatch(t, []*graph.Edge{wantCall, wantConfig}, direct)
			require.ElementsMatch(t, wantO, omitted)
			// Simulate the terminal D72 removal at the callback boundary.
			require.Equal(t, 2, b.RemoveEdgesExact([]*graph.Edge{&unproved, incumbent}))
			b.AddNode(original)
			relations, err := b.PrepareRelations(ctx, t.TempDir(), fileBase, r.extractorOf)
			require.NoError(t, err)
			relations.Witness.Relations = &store_pebble.RelationCoverage{DeferredLanguages: []string{"java", "yaml"}}
			base, err := b.RetainRelations(ctx, fileBase, relations)
			require.NoError(t, err)
			_, err = head.PublishStage(ctx, relations)
			require.NoError(t, err)
			stage, err = seal(t.TempDir(), base)
			return err
		})
	})
	require.NoError(t, err)
	require.Equal(t, parsedBefore, parser.ParseCount())
	require.EqualValues(t, len(wantO), stage.Witness.Contracts.OmittedRecords)
	_, err = head.PublishStage(ctx, stage)
	require.NoError(t, err)
	snap := head.OpenSnapshot()
	defer snap.Close()
	require.Equal(t, g.GetNode(di), snap.GetNode(di))
	require.Equal(t, g.GetNode(key), snap.GetNode(key))
	var actual []*graph.Edge
	for _, cls := range []string{"Client", "Existing", "ZBean"} {
		for _, e := range snap.GetOutEdges(javaFile + "::" + cls) {
			if e.Kind == graph.EdgeReadsConfig || e.Kind == graph.EdgeCalls && e.Meta["via"] == "spring.Bean" {
				actual = append(actual, e)
			}
			require.NotEqual(t, graph.EdgeIdentityFor(incumbent), graph.EdgeIdentityFor(e))
		}
	}
	require.ElementsMatch(t, []*graph.Edge{wantCall, wantConfig}, actual)
	for _, id := range []string{di, key} {
		partial, err := snap.ContractCoverageContext(ctx, contracts.ReadScope{FilePaths: []string{javaFile}, ContractIDs: []string{id}}, true)
		require.True(t, partial)
		require.ErrorIs(t, err, contracts.ErrContractCoverageIncomplete)
	}
}
