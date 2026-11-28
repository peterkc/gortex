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
)

func TestRebuildProviderBodyCheckedReplay(t *testing.T) {
	const file = "handler.go"
	ctx, root := t.Context(), t.TempDir()
	src := []byte(`package p

import "net/http"

type Payload struct{ ID string }

func Serve(w http.ResponseWriter, r *http.Request) {
	WriteJSON(w, http.StatusCreated, Payload{ID: "p"})
}
`)
	path := filepath.Join(root, file)
	require.NoError(t, os.WriteFile(path, src, 0o600))
	native := graph.New()
	idx := newTestIndexer(native)
	idx.SetRootPath(root)
	t.Cleanup(idx.Close)
	ext, ok := idx.registry.GetByLanguage("go")
	require.True(t, ok)
	parsed, facts, err := idx.extractPreparedRebuildFileWithContractFacts(ctx, file, ext, src, src)
	require.NoError(t, err)
	native.AddBatch(parsed.Nodes, parsed.Edges)
	input := rebuildInput{Path: file, Language: "go", Size: int64(len(src))}
	job := rebuildFileStages{inputs: []rebuildInput{input}, recipes: map[string]rebuildDigest{"go": {1}}}
	store := rebuildParseStore{dir: t.TempDir(), limit: 1 << 20}
	wire, err := store_pebble.EncodeContractParseFacts(parsed, facts)
	require.NoError(t, err)
	require.NoError(t, store.write(input.parseKey(idx.rebuildExtractorRecipe(job.recipes[input.Language], input)), wire))
	reader := rebuildContractReceiptReader{idx: idx, job: job, store: store,
		census: &rebuildExtractorCensus{byPath: map[string]string{file: "go"}, originByGraphPath: map[string]string{file: file}}}
	b, err := store_pebble.OpenBuild(t.TempDir(), store_pebble.FullProfile)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, b.Close()) })
	require.NoError(t, b.AddParsedFile("", parsed))
	view := b.RebuildView()
	route := contracts.Contract{ID: "http::POST::/items/{id}", Type: contracts.ContractHTTP, Role: contracts.RoleProvider,
		FilePath: "routes.go", SymbolID: "routes.go::Register", Line: 220,
		Meta: map[string]any{"path": "/items/{id}", "handler_ident": "Serve", "response_expr": "old", "status_codes": []int{202}}}
	reg := contracts.NewRegistry()
	reg.Add(cloneContractRecords([]contracts.Contract{route})[0])
	idx.resolveProviderHandlers(reg)
	want := reg.ByID(route.ID)
	require.Len(t, want, 1)
	require.Equal(t, file+"::Serve", want[0].SymbolID)
	require.Equal(t, file+"::Payload", want[0].Meta["response_type"])
	require.IsType(t, []int{}, want[0].Meta["status_codes"])
	require.Equal(t, route.FilePath, want[0].FilePath)
	require.Equal(t, route.Line, want[0].Line)
	require.NoError(t, os.Remove(path))
	parses := parser.ParseCount()
	handler, err := reader.providerHandler(ctx, view, route)
	require.NoError(t, err)
	require.NotNil(t, handler)
	body, err := reader.providerBody(ctx, view, handler)
	require.NoError(t, err)
	bindRebuildProviderRecord(&route, handler.ID, body)
	require.Equal(t, want[0], route)
	require.Equal(t, parses, parser.ParseCount())

	b.AddNode(&graph.Node{ID: file + "::Added", Name: "Payload", Kind: graph.KindType, Language: "go", FilePath: file})
	_, err = reader.providerBody(ctx, view, handler)
	require.ErrorIs(t, err, errRebuildContractBodyUnproved)
	moved := *handler
	moved.StartLine = 0
	_, err = reader.providerBody(ctx, view, &moved)
	require.ErrorIs(t, err, graph.ErrContractProjectionStale)
}

func TestRebuildProviderBodyIgnoresUnreferencedNestedTypes(t *testing.T) {
	const file = "views.py"
	ctx, root := t.Context(), t.TempDir()
	src := []byte(`class First:
    class Meta:
        pass

class Second:
    class Meta:
        pass

class Payload:
    pass

def view() -> Payload:
    return Payload()

def ambiguous() -> Meta:
    return Meta()
`)
	path := filepath.Join(root, file)
	require.NoError(t, os.WriteFile(path, src, 0o600))
	native := graph.New()
	idx := newTestIndexer(native)
	idx.SetRootPath(root)
	t.Cleanup(idx.Close)
	// Use distinct nested declaration IDs, as a scoped extractor can do.
	// The current Python extractor coalesces the two short Meta IDs.
	nodes := []*graph.Node{
		{ID: file + "::First.Meta", Kind: graph.KindType, Name: "Meta", StartLine: 2, EndLine: 3},
		{ID: file + "::Second.Meta", Kind: graph.KindType, Name: "Meta", StartLine: 6, EndLine: 7},
		{ID: file + "::Payload", Kind: graph.KindType, Name: "Payload", StartLine: 9, EndLine: 10},
		{ID: file + "::view", Kind: graph.KindFunction, Name: "view", StartLine: 12, EndLine: 13},
		{ID: file + "::ambiguous", Kind: graph.KindFunction, Name: "ambiguous", StartLine: 15, EndLine: 16},
		{ID: file + "::no_line", Kind: graph.KindFunction, Name: "no_line"},
	}
	facts := &contracts.FileFacts{Shapes: make(map[string]*contracts.Shape)}
	for _, n := range nodes {
		n.FilePath, n.Language = file, "python"
		if n.Kind == graph.KindType {
			facts.Shapes[n.ID] = nil
		}
	}
	require.NoError(t, idx.captureRebuildContractBodies(ctx, file, "python", src, src, nil, &parser.ExtractionResult{Nodes: nodes}, true, facts))
	native.AddBatch(nodes, nil)
	input := rebuildInput{Path: file, Language: "python", Size: int64(len(src))}
	job := rebuildFileStages{inputs: []rebuildInput{input}, recipes: map[string]rebuildDigest{"python": {1}}}
	store := rebuildParseStore{dir: t.TempDir(), limit: 1 << 20}
	parsed := store_pebble.ParseFacts{File: graph.FileMetaRow{FilePath: file, NodeCount: len(nodes)}, EffectiveLanguage: "python", Nodes: nodes}
	wire, err := store_pebble.EncodeContractParseFacts(parsed, facts)
	require.NoError(t, err)
	require.NoError(t, store.write(input.parseKey(idx.rebuildExtractorRecipe(job.recipes[input.Language], input)), wire))
	reader := rebuildContractReceiptReader{idx: idx, job: job, store: store,
		census: &rebuildExtractorCensus{byPath: map[string]string{file: "python"}, originByGraphPath: map[string]string{file: file}}}
	b, err := store_pebble.OpenBuild(t.TempDir(), store_pebble.FullProfile)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, b.Close()) })
	require.NoError(t, b.AddParsedFile("", parsed))
	view := b.RebuildView()
	route := contracts.Contract{ID: "http::GET::/items", Type: contracts.ContractHTTP, Role: contracts.RoleProvider,
		FilePath: "routes.py", Line: 5, Meta: map[string]any{"handler_ident": "view"}}
	reg := contracts.NewRegistry()
	reg.Add(cloneContractRecords([]contracts.Contract{route})[0])
	idx.resolveProviderHandlers(reg)
	want := reg.ByID(route.ID)
	require.Len(t, want, 1)
	require.Equal(t, file+"::Payload", want[0].Meta["response_type"])
	require.NoError(t, os.Remove(path))
	handler, err := reader.providerHandler(ctx, view, route)
	require.NoError(t, err)
	require.NotNil(t, handler)
	body, err := reader.providerBody(ctx, view, handler)
	require.NoError(t, err)
	bindRebuildProviderRecord(&route, handler.ID, body)
	require.Equal(t, want[0], route)
	for _, name := range []string{"ambiguous", "no_line"} {
		handler, err := view.Node(ctx, file+"::"+name)
		require.NoError(t, err)
		require.NotNil(t, handler)
		_, err = reader.providerBody(ctx, view, handler)
		require.ErrorIs(t, err, errRebuildContractBodyUnproved, name)
		require.NotErrorIs(t, err, graph.ErrContractProjectionStale, name)
	}
}
