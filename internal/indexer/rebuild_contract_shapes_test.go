package indexer

import (
	"errors"
	"iter"
	"os"
	"path/filepath"
	"slices"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/zzet/gortex/internal/contracts"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/graph/store_pebble"
)

func TestRebuildContractShapeBatchesFromCheckedReceipts(t *testing.T) {
	const path = "schema.go"
	ctx, root := t.Context(), t.TempDir()
	nativeGraph := graph.New()
	idx := newTestIndexer(nativeGraph)
	idx.SetRootPath(root)
	t.Cleanup(idx.Close)
	src := []byte("package p\ntype Payload struct {\n ID int `json:\"id\"`\n Label string `json:\"label,omitempty\"`\n internal bool\n}\nfunc Handler() {}\ntype Scalar string\n")
	name := filepath.Join(root, path)
	require.NoError(t, os.WriteFile(name, src, 0o600))
	ext, ok := idx.registry.GetByLanguage("go")
	require.True(t, ok)
	parsed, facts, err := idx.extractPreparedRebuildFileWithContractFacts(ctx, path, ext, src, src)
	require.NoError(t, err)
	input := rebuildInput{Path: path, Language: "go", Size: int64(len(src))}
	job := rebuildFileStages{inputs: []rebuildInput{input}, recipes: map[string]rebuildDigest{"go": {1}}}
	store := rebuildParseStore{dir: t.TempDir(), limit: 1 << 20}
	wire, err := store_pebble.EncodeContractParseFacts(parsed, facts)
	require.NoError(t, err)
	require.NoError(t, store.write(input.parseKey(idx.rebuildExtractorRecipe(job.recipes[input.Language], input)), wire))
	reader := rebuildContractReceiptReader{idx: idx, job: job, store: store,
		census: &rebuildExtractorCensus{byPath: map[string]string{path: "go"}, originByGraphPath: map[string]string{path: path}}}

	b, err := store_pebble.OpenBuild(t.TempDir(), store_pebble.FullProfile)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, b.Close()) })
	require.NoError(t, b.AddParsedFile("", parsed))
	view := b.RebuildView()
	nativeGraph.AddBatch(parsed.Nodes, parsed.Edges)
	var payload, handler, scalar string
	for _, n := range parsed.Nodes {
		switch {
		case n.Kind == graph.KindType && n.Name == "Payload":
			payload = n.ID
		case n.Kind == graph.KindFunction:
			handler = n.ID
		case n.Kind == graph.KindType && n.Name == "Scalar":
			scalar = n.ID
		}
	}
	require.NotEmpty(t, payload)
	require.NotEmpty(t, handler)
	require.NotEmpty(t, scalar)
	require.Contains(t, facts.Shapes, scalar)
	require.Nil(t, facts.Shapes[scalar], "accepted no-shape declaration")
	c := contracts.Contract{
		ID: "http::POST::/payload", Type: contracts.ContractHTTP, Role: contracts.RoleProvider,
		FilePath: path, SymbolID: handler, Line: 7,
		Meta: map[string]any{"request_type": payload, "response_type": scalar},
	}
	registry := contracts.NewRegistry()
	registry.Add(c)
	idx.snapshotContractShapes(registry)
	want, ok := nativeGraph.GetNode(payload).Meta["shape"].(*contracts.Shape)
	require.True(t, ok)
	require.NotNil(t, want)
	require.Len(t, want.Fields, 2)
	require.NoError(t, os.Remove(name))

	references := func(ids ...string) iter.Seq2[string, error] {
		return func(yield func(string, error) bool) {
			for _, id := range ids {
				if !yield(id, nil) {
					return
				}
			}
		}
	}
	for _, ids := range [][]string{{payload, payload}, {payload, "a.go::Missing"}, {"bare_name"}} {
		var rejected error
		for _, err := range reader.shapeBatches(ctx, view, references(ids...)) {
			if err != nil {
				rejected = err
			}
		}
		require.ErrorIs(t, rejected, graph.ErrContractProjectionStale, ids)
	}
	upstream := errors.New("test reference read failed")
	for batch, err := range reader.shapeBatches(ctx, view, func(yield func(string, error) bool) { yield("", upstream) }) {
		require.Zero(t, batch)
		require.ErrorIs(t, err, upstream)
	}
	original, err := view.Node(ctx, payload)
	require.NoError(t, err)
	require.NotNil(t, original)
	moved := *original
	moved.EndLine++
	b.AddNode(&moved)
	_, err = reader.shapeAt(ctx, view, payload)
	require.ErrorIs(t, err, graph.ErrContractProjectionStale)
	stamped := *original
	stamped.Meta = map[string]any{"shape": nil}
	b.AddNode(&stamped)
	_, err = reader.shapeAt(ctx, view, payload)
	require.ErrorIs(t, err, graph.ErrContractProjectionStale)
	b.AddNode(original)

	head, err := store_pebble.OpenHead(t.TempDir(), rebuildTestIncarnation, store_pebble.FullProfile)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, head.Close()) })
	manifest, err := store_pebble.PrepareManifest(ctx, t.TempDir(), []store_pebble.ManifestFile{{Path: path, Kind: 'f', Language: "go"}}, store_pebble.Witness{
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
	relations, err := b.PrepareRelations(ctx, t.TempDir(), fileBase, nil)
	require.NoError(t, err)
	relationBase, err := b.RetainRelations(ctx, fileBase, relations)
	require.NoError(t, err)
	_, err = head.PublishStage(ctx, relations)
	require.NoError(t, err)
	before := head.OpenSnapshot()
	defer before.Close()
	ordinary := before.GetNode(payload)
	require.NotNil(t, ordinary)
	require.NotContains(t, ordinary.Meta, "shape")

	ids := []string{"missing.go::Unknown", handler, payload, scalar}
	slices.Sort(ids)
	shapeRows := reader.shapeBatches(ctx, view, references(ids...))
	batches := func(yield func(store_pebble.RebuildContractBatch, error) bool) {
		if !yield(store_pebble.RebuildContractBatch{
			Nodes: []*graph.Node{contractGraphNode(c, true)},
			Edges: appendContractGraphEdges(nil, c, handler),
		}, nil) {
			return
		}
		for batch, err := range shapeRows {
			if !yield(batch, err) || err != nil {
				return
			}
		}
	}
	tables, err := b.PrepareContracts(ctx, t.TempDir(), relationBase, batches, store_pebble.ContractCoverage{})
	require.NoError(t, err)
	_, err = head.PublishStage(ctx, tables)
	require.NoError(t, err)
	after := head.OpenSnapshot()
	defer after.Close()
	shapes, err := after.ContractShapesContext(ctx, ids)
	require.NoError(t, err)
	require.Equal(t, map[string]*contracts.Shape{payload: want}, shapes)
	require.Equal(t, ordinary, after.GetNode(payload))
	require.Equal(t, ordinary, before.GetNode(payload))
	full, err := after.FullNodesByIDsContext(ctx, []string{payload})
	require.NoError(t, err)
	require.Equal(t, want, full[payload].Meta["shape"])
}
