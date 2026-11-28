package indexer

import (
	"iter"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/zzet/gortex/internal/contracts"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/graph/store_pebble"
	"github.com/zzet/gortex/internal/parser/languages"
)

func TestDeferredContractLanguagesRefusesIncompleteCensus(t *testing.T) {
	for _, census := range []*rebuildExtractorCensus{nil, {},
		{byPath: map[string]string{"a.go": "go"}, originByGraphPath: map[string]string{}, admitted: 2},
		{byPath: map[string]string{"a.go": ""}, originByGraphPath: map[string]string{}, admitted: 1},
	} {
		r := rebuildContractReceiptReader{census: census}
		_, err := r.deferredContractLanguages()
		require.ErrorIs(t, err, graph.ErrContractProjectionStale)
	}
}

func TestSelectRebuildContractRecordRefusesChangedOrUnprovedOwner(t *testing.T) {
	ctx := t.Context()
	b, err := store_pebble.OpenBuild(t.TempDir(), store_pebble.FullProfile)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, b.Close()) })
	r := rebuildContractReceiptReader{census: &rebuildExtractorCensus{
		byPath: map[string]string{"a.py": "python"}, originByGraphPath: map[string]string{"a.py": "a.py"},
	}}
	c := store_pebble.RebuildContractRecord{Contract: contracts.Contract{ID: "env::KEY", Type: contracts.ContractEnv, Role: contracts.RoleConsumer, FilePath: "a.py"}}
	view := b.RebuildView()
	for _, owner := range []string{"missing", "foreign"} {
		b.AddNode(&graph.Node{ID: "foreign", RepoPrefix: "other", FilePath: "a.py", Kind: graph.KindFile})
		keep, err := r.selectRebuildContractRecord(ctx, view, c, owner)
		require.ErrorIs(t, err, graph.ErrContractProjectionStale)
		require.False(t, keep)
	}
	b.AddNode(&graph.Node{ID: "moved", FilePath: "other.py", Kind: graph.KindFunction})
	for _, owner := range []string{"", "moved"} {
		keep, err := r.selectRebuildContractRecord(ctx, view, c, owner)
		require.NoError(t, err)
		require.False(t, keep)
	}
	c.FilePath = "absent.py"
	keep, err := r.selectRebuildContractRecord(ctx, view, c, "")
	require.ErrorIs(t, err, graph.ErrContractProjectionStale)
	require.False(t, keep)
}

func TestRebuildContractProducerRefusesDeferredLanguageMismatch(t *testing.T) {
	ctx := t.Context()
	b, err := store_pebble.OpenBuild(t.TempDir(), store_pebble.FullProfile)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, b.Close()) })
	idx := newTestIndexer(graph.New())
	t.Cleanup(idx.Close)
	r := rebuildContractReceiptReader{idx: idx, census: &rebuildExtractorCensus{
		byPath: map[string]string{}, originByGraphPath: map[string]string{},
	}}
	err = r.withRebuildContractProducer(ctx, b, func(func(contracts.Contract) error) error { return nil }, make(rebuildEarlyContractIDs),
		func(_ iter.Seq2[store_pebble.RebuildContractBatch, error], seal func(string, store_pebble.RelationsBase) (store_pebble.StageTables, error)) error {
			stage, err := seal(t.TempDir(), store_pebble.RelationsBase{})
			require.Zero(t, stage)
			return err
		})
	require.ErrorContains(t, err, "contract selection differs from retained relation coverage")
}

func TestSelectRebuildContractRecordProofGuards(t *testing.T) {
	ctx := t.Context()
	idx := newTestIndexer(graph.New())
	idx.registry.Register(languages.NewJavaExtractor())
	t.Cleanup(idx.Close)
	b, err := store_pebble.OpenBuild(t.TempDir(), store_pebble.FullProfile)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, b.Close()) })
	const file = "Bean.java"
	owner := &graph.Node{ID: file + "::Owner", Name: "Owner", FilePath: file, Kind: graph.KindType, Language: "java", StartLine: 1}
	target := &graph.Node{ID: file + "::T", Name: "T", FilePath: file, Kind: graph.KindType, Language: "java", StartLine: 2}
	e := &graph.Edge{From: owner.ID, To: target.ID, Kind: graph.EdgeProvides, FilePath: file, Line: 1,
		Meta: map[string]any{graph.MetaDIBinding: graph.DIBindingUseClass, graph.MetaDIProvidesFor: "T"}}
	di, ok := diContractFromEdge(e)
	require.True(t, ok)
	hinted := contracts.Contract{ID: "http::GET::/x", Type: contracts.ContractHTTP, Role: contracts.RoleProvider, FilePath: file, SymbolID: owner.ID, Line: 1,
		Meta: map[string]any{"handler_trail": "handler"}}
	plain := hinted
	plain.ID, plain.Meta = "http::GET::/y", map[string]any{}
	base := store_pebble.ParseFacts{EffectiveLanguage: "java", File: graph.FileMetaRow{FilePath: file, NodeCount: 2}, Nodes: []*graph.Node{owner, target}, Edges: []*graph.Edge{e}}
	facts := &contracts.FileFacts{Records: []contracts.Contract{hinted, plain}, Shapes: map[string]*contracts.Shape{owner.ID: nil, target.ID: nil}}
	wire, err := store_pebble.EncodeContractParseFacts(base, facts)
	require.NoError(t, err)
	input := rebuildInput{Path: file, Language: "java"}
	job := rebuildFileStages{inputs: []rebuildInput{input}, recipes: map[string]rebuildDigest{"java": {1}}}
	store := rebuildParseStore{dir: t.TempDir(), limit: 1 << 20}
	require.NoError(t, store.write(input.parseKey(idx.rebuildExtractorRecipe(job.recipes[input.Language], input)), wire))
	r := rebuildContractReceiptReader{idx: idx, job: job, store: store, census: &rebuildExtractorCensus{byPath: map[string]string{file: "java"}, originByGraphPath: map[string]string{file: file}}}
	b.AddNode(owner)
	view := b.RebuildView()
	selectRecord := func(c contracts.Contract) (bool, error) {
		return r.selectRebuildContractRecord(ctx, view, store_pebble.RebuildContractRecord{Contract: c}, owner.ID)
	}
	keep, err := selectRecord(hinted)
	require.NoError(t, err)
	require.False(t, keep, "an unchanged raw handler hint still needs linking")
	keep, err = selectRecord(plain)
	require.NoError(t, err)
	require.True(t, keep)
	keep, err = selectRecord(di)
	require.NoError(t, err)
	require.False(t, keep, "raw DI edge with missing target is not independent")
	moved := *target
	moved.StartLine++
	b.AddNode(&moved)
	keep, err = selectRecord(di)
	require.ErrorIs(t, err, graph.ErrContractProjectionStale)
	require.False(t, keep)
	b.AddNode(target)
	keep, err = selectRecord(di)
	require.NoError(t, err)
	require.True(t, keep)
}
