package indexer

import (
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/zzet/gortex/internal/contracts"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/graph/store_pebble"
	"github.com/zzet/gortex/internal/parser/languages"
)

func TestRebuildContractAffectedUsesHeldScopeNotCandidates(t *testing.T) {
	ctx := t.Context()
	idx := newTestIndexer(graph.New())
	idx.registry.Register(languages.NewPythonExtractor())
	t.Cleanup(idx.Close)
	b, err := store_pebble.OpenBuild(t.TempDir(), store_pebble.FullProfile)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, b.Close()) })
	const source, models, supportedID, heldID = "routes.go", "models.py", "http::GET::/keep", "http::GET::/held"
	typ := &graph.Node{ID: models + "::Payload", Name: "Payload", FilePath: models, Kind: graph.KindType, Language: "python", StartLine: 1}
	handler := &graph.Node{ID: models + "::Serve", Name: "Serve", FilePath: models, Kind: graph.KindFunction, Language: "python", StartLine: 3}
	constant := &graph.Node{ID: models + "::Route", Name: "Route", FilePath: models, Kind: graph.KindConstant, Language: "python", StartLine: 4}
	held := store_pebble.ParseFacts{EffectiveLanguage: "python", File: graph.FileMetaRow{FilePath: models, NodeCount: 3},
		Nodes: []*graph.Node{typ, handler, constant}, Constants: []graph.ConstantValueRow{{NodeID: constant.ID, FilePath: models, Value: "/held"}}}
	require.NoError(t, b.AddRebuildDeferredFile("", held, func(string) (bool, error) { return true, nil }))
	file := &graph.Node{ID: source, FilePath: source, Kind: graph.KindFile, Language: "go"}
	retained := store_pebble.ParseFacts{EffectiveLanguage: "go", File: graph.FileMetaRow{FilePath: source, NodeCount: 1}, Nodes: []*graph.Node{file}}
	require.NoError(t, b.AddParsedFile("", retained))
	require.NoError(t, b.IsolateRebuildLinks(ctx))
	view := b.RebuildView()
	c := contracts.Contract{ID: supportedID, Type: contracts.ContractHTTP, Role: contracts.RoleProvider, Line: 1, FilePath: source,
		Meta: map[string]any{"handler_ident": "Serve", "response_type": typ.ID}}
	q := contracts.EndpointContractQuestion{Kind: contracts.EndpointGoRoute, Method: "GET", Record: contracts.Contract{
		Type: contracts.ContractHTTP, Role: contracts.RoleProvider, FilePath: source, Line: 2, Meta: map[string]any{},
	}, Expression: contracts.EndpointExpression{Terms: []contracts.EndpointTerm{{Name: "Route"}}}}
	input := rebuildInput{Path: source, Language: "go"}
	job := rebuildFileStages{inputs: []rebuildInput{input}, recipes: map[string]rebuildDigest{"go": {1}}}
	store := rebuildParseStore{dir: t.TempDir(), limit: 1 << 20}
	wire, err := store_pebble.EncodeContractParseFacts(retained, &contracts.FileFacts{Records: []contracts.Contract{c}, Questions: []contracts.EndpointContractQuestion{q}})
	require.NoError(t, err)
	require.NoError(t, store.write(input.parseKey(idx.rebuildExtractorRecipe(job.recipes[input.Language], input)), wire))
	marks := 0
	r := rebuildContractReceiptReader{idx: idx, job: job, store: store, isolate: true, linkView: view,
		census: &rebuildExtractorCensus{admitted: 2, byPath: map[string]string{source: "go", models: "python"}, originByGraphPath: map[string]string{source: source, models: models}},
		markIsolationAffected: func(path string, kind contracts.ContractType, language string) error {
			require.Equal(t, source, path)
			require.Equal(t, contracts.ContractHTTP, kind)
			require.Equal(t, "python", language)
			marks++
			return nil
		},
	}
	require.NoError(t, r.visitFilesAtParseCut(ctx, view, func(_ string, rows []contracts.Contract, _ *contracts.FileFacts) error {
		require.Len(t, rows, 1, "held constant cannot answer the endpoint question")
		require.Equal(t, supportedID, rows[0].ID)
		require.NotEqual(t, heldID, rows[0].ID)
		return nil
	}))
	require.Equal(t, 1, marks)
	pass := r.handlerPass()
	winner, err := pass.providerHandler(ctx, view, c)
	require.NoError(t, err)
	require.Nil(t, winner, "held handler cannot win election")
	require.Equal(t, 2, marks)
	require.NoError(t, b.WithRebuildContractRecords(ctx, func(records *store_pebble.RebuildContractRecords) error {
		require.NoError(t, records.Add(store_pebble.RebuildContractRecord{Contract: c}))
		return records.WithContractSelection(func(s *store_pebble.RebuildContractSelection) error {
			require.NoError(t, s.FreezeCompleteIDs())
			var omitted []store_pebble.RebuildContractOmission
			require.NoError(t, r.selectRebuildShapeRefs(ctx, view, records, s, func(batch store_pebble.RebuildContractBatch) error {
				omitted = append(omitted, batch.Omitted...)
				return nil
			}))
			require.Len(t, omitted, 1)
			require.Equal(t, models, omitted[0].FilePath)
			require.Equal(t, supportedID, omitted[0].ID)
			return nil
		})
	}))
	require.Equal(t, 3, marks)
	shape, err := r.shapeLookup(ctx, view)(typ.ID)
	require.NoError(t, err)
	require.Nil(t, shape, "held declaration is attribution, not a shape answer")

	// Independent raw inventory has no live shape or handler candidates.
	const heldSource = "api.py"
	heldInput := rebuildInput{Path: heldSource, Language: "python"}
	r.job.inputs = []rebuildInput{heldInput}
	r.job.recipes["python"] = rebuildDigest{2}
	r.census.byPath[heldSource] = "python"
	r.census.originByGraphPath[heldSource] = heldSource
	r.census.admitted++
	heldRecord := contracts.Contract{ID: heldID, Type: contracts.ContractHTTP, Role: contracts.RoleProvider, FilePath: heldSource, Line: 1,
		Meta: map[string]any{"response_type": typ.ID}}
	meta := graph.FileMetaRow{FilePath: heldSource}
	wire, err = store_pebble.EncodeContractParseFacts(store_pebble.ParseFacts{EffectiveLanguage: "python", File: meta}, &contracts.FileFacts{Records: []contracts.Contract{heldRecord}})
	require.NoError(t, err)
	require.NoError(t, store.write(heldInput.parseKey(idx.rebuildExtractorRecipe(r.job.recipes["python"], heldInput)), wire))
	var rawO []store_pebble.RebuildContractOmission
	require.NoError(t, r.visitHeldOutContractOmissions(ctx, func(batch store_pebble.RebuildContractBatch) error {
		require.Empty(t, batch.Nodes)
		require.Empty(t, batch.Edges)
		require.Empty(t, batch.Shapes)
		rawO = append(rawO, batch.Omitted...)
		return nil
	}))
	require.Len(t, rawO, 2)
	require.Equal(t, heldSource, rawO[0].FilePath)
	require.Equal(t, models, rawO[1].FilePath)
	require.Equal(t, heldID, rawO[1].ID)
	require.NotEqual(t, rawO[0].RecordKey, rawO[1].RecordKey)
}
