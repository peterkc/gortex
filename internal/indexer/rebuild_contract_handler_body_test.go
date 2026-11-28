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

func TestRebuildProviderBodyCheckedReplay(t *testing.T) {
	const file = "handler.go"
	ctx, root := t.Context(), t.TempDir()
	src := []byte(`package p

import "net/http"

type Payload struct{ ID string }

func Serve(w http.ResponseWriter, r *http.Request) {
	WriteJSON(w, http.StatusCreated, Payload{ID: "p"})
}

func Literal(w http.ResponseWriter, r *http.Request) {
	WriteJSON(w, http.StatusCreated, Payload{ID: "p"})
	WriteJSON(w, http.StatusOK, "ok")
}

func Missing(w http.ResponseWriter, r *http.Request) {
	WriteJSON(w, http.StatusCreated, ExternalPayload{})
}

type Item struct{}

func Qualified(w http.ResponseWriter, r *http.Request) {
	WriteJSON(w, http.StatusCreated, &other.Item{})
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
	b, err := store_pebble.OpenBuild(t.TempDir(), store_pebble.FullProfile)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, b.Close()) })
	reader := rebuildContractReceiptReader{idx: idx, job: job, store: store, linkView: b.RebuildView(),
		census: &rebuildExtractorCensus{byPath: map[string]string{file: "go"}, originByGraphPath: map[string]string{file: file}}}
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
	handler, err := reader.handlerPass().providerHandler(ctx, view, route)
	require.NoError(t, err)
	require.NotNil(t, handler)
	body, err := reader.providerBody(ctx, view, handler)
	require.NoError(t, err)
	bindRebuildProviderRecord(&route, handler.ID, body)
	require.Equal(t, want[0], route)
	require.Equal(t, parses, parser.ParseCount())

	qualified, err := view.Node(ctx, file+"::Qualified")
	require.NoError(t, err)
	require.NotNil(t, qualified)
	qualifiedBody, err := reader.providerBody(ctx, view, qualified)
	require.NoError(t, err)
	require.Equal(t, map[string]string{"Item": file + "::Item"}, qualifiedBody.ASTTypeLookups)
	require.Equal(t, file+"::Item", qualifiedBody.AST["response_type"])
	b.AddNode(&graph.Node{ID: file + "::OtherItem", Name: "Item", Kind: graph.KindType, Language: "go", FilePath: file})
	_, err = reader.providerBody(ctx, view, qualified)
	require.ErrorIs(t, err, errRebuildContractBodyUnproved)

	b.AddNode(&graph.Node{ID: file + "::Added", Name: "Payload", Kind: graph.KindType, Language: "go", FilePath: file})
	b.AddNode(&graph.Node{ID: file + "::string", Name: "string", Kind: graph.KindType, Language: "go", FilePath: file})
	literal, err := view.Node(ctx, file+"::Literal")
	require.NoError(t, err)
	require.NotNil(t, literal)
	literalBody, err := reader.providerBody(ctx, view, literal)
	require.NoError(t, err)
	require.Equal(t, "string", literalBody.AST["response_type"])
	require.Empty(t, literalBody.ASTTypeLookups)

	missing, err := view.Node(ctx, file+"::Missing")
	require.NoError(t, err)
	require.NotNil(t, missing)
	missingBody, err := reader.providerBody(ctx, view, missing)
	require.NoError(t, err)
	require.Equal(t, map[string]string{"ExternalPayload": "ExternalPayload"}, missingBody.ASTTypeLookups)
	b.AddNode(&graph.Node{ID: file + "::ExternalPayload", Name: "ExternalPayload", Kind: graph.KindType, Language: "go", FilePath: file})
	_, err = reader.providerBody(ctx, view, missing)
	require.ErrorIs(t, err, errRebuildContractBodyUnproved)

	_, err = reader.providerBody(ctx, view, handler)
	require.ErrorIs(t, err, errRebuildContractBodyUnproved)
	renamed := *handler
	renamed.Name = "renamed"
	_, err = reader.providerBody(ctx, view, &renamed)
	require.ErrorContains(t, err, "ambiguous or renamed handler")
	bad, err := view.Node(ctx, file+"::Payload")
	require.NoError(t, err)
	require.NotNil(t, bad)
	_, err = reader.providerBody(ctx, view, bad)
	require.ErrorContains(t, err, "missing body facts")
	moved := *handler
	moved.StartLine = 0
	_, err = reader.providerBody(ctx, view, &moved)
	require.ErrorIs(t, err, graph.ErrContractProjectionStale)
	b.EvictFile(file)
	b.AddNode(handler)
	_, err = reader.providerBody(ctx, view, handler)
	require.ErrorIs(t, err, errRebuildContractBodyUnproved, "the selected local type is now absent")
	b.AddNode(&renamed)
	err = b.WithRebuildContractRecords(ctx, func(records *store_pebble.RebuildContractRecords) error {
		broken := route
		broken.SymbolID = "register"
		broken.Meta = map[string]any{"handler_ident": renamed.Name}
		require.NoError(t, records.Add(store_pebble.RebuildContractRecord{Contract: broken}))
		return reader.bindRebuildProviderHandlers(ctx, view, records)
	})
	require.ErrorContains(t, err, "ambiguous or renamed handler")
}

func TestRebuildProviderBodyEmptyLookupName(t *testing.T) {
	const file = "handler.go"
	ctx := t.Context()
	idx := newTestIndexer(graph.New())
	idx.SetRootPath(t.TempDir())
	t.Cleanup(idx.Close)
	handler := &graph.Node{ID: file + "::Serve", Name: "Serve", Kind: graph.KindFunction, FilePath: file, Language: "go", StartLine: 1, EndLine: 1}
	nameless := &graph.Node{ID: file + "::Empty", Kind: graph.KindType, FilePath: file, Language: "go"}
	other := *nameless
	other.ID = file + "::Moved"
	for _, tc := range []struct {
		name          string
		saved, linked *graph.Node
	}{
		{"disappeared", nameless, nil},
		{"changed ID", nameless, &other},
		{"appeared", nil, nameless},
	} {
		t.Run(tc.name, func(t *testing.T) {
			// A synthetic checked lookup exercises the receipt reader even
			// when the Go grammar recovers an empty type tail differently.
			answer := ""
			nodes := []*graph.Node{handler}
			shapes := make(map[string]*contracts.Shape)
			if tc.saved != nil {
				answer = tc.saved.ID
				nodes = append(nodes, tc.saved)
				shapes[answer] = nil
			}
			body := contracts.HandlerFacts{
				Regex:          map[string]any{},
				AST:            map[string]any{"response_type": answer},
				ASTTypeLookups: map[string]string{"": answer},
				Bindings:       map[string]contracts.Binding{},
			}
			facts := &contracts.FileFacts{Shapes: shapes, Handlers: map[string]contracts.HandlerFacts{handler.ID: body}}
			parsed := store_pebble.ParseFacts{File: graph.FileMetaRow{FilePath: file, NodeCount: len(nodes)}, EffectiveLanguage: "go", Nodes: nodes}
			input := rebuildInput{Path: file, Language: "go"}
			job := rebuildFileStages{inputs: []rebuildInput{input}, recipes: map[string]rebuildDigest{"go": {1}}}
			store := rebuildParseStore{dir: t.TempDir(), limit: 1 << 20}
			wire, err := store_pebble.EncodeContractParseFacts(parsed, facts)
			require.NoError(t, err)
			require.NoError(t, store.write(input.parseKey(idx.rebuildExtractorRecipe(job.recipes[input.Language], input)), wire))
			b, err := store_pebble.OpenBuild(t.TempDir(), store_pebble.FullProfile)
			require.NoError(t, err)
			t.Cleanup(func() { require.NoError(t, b.Close()) })
			reader := rebuildContractReceiptReader{idx: idx, job: job, store: store, linkView: b.RebuildView(),
				census: &rebuildExtractorCensus{byPath: map[string]string{file: "go"}, originByGraphPath: map[string]string{file: file}}}
			require.NoError(t, b.AddParsedFile("", parsed))
			view := b.RebuildView()
			got, err := reader.providerBody(ctx, view, handler)
			require.NoError(t, err)
			require.Equal(t, body, got)
			b.EvictFile(file)
			b.AddNode(handler)
			if tc.linked != nil {
				b.AddNode(tc.linked)
			}
			_, err = reader.providerBody(ctx, view, handler)
			require.ErrorIs(t, err, errRebuildContractBodyUnproved)
			require.ErrorContains(t, err, `local type ""`)
		})
	}
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
	idx.registry.Register(languages.NewPythonExtractor())
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
	// The native router census includes Python mount-only files, even
	// when there is no local route. Save the checked empty answer.
	router := contracts.RouterFileFacts(file, src)
	facts := &contracts.FileFacts{Shapes: make(map[string]*contracts.Shape), Router: &router}
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
	b, err := store_pebble.OpenBuild(t.TempDir(), store_pebble.FullProfile)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, b.Close()) })
	reader := rebuildContractReceiptReader{idx: idx, job: job, store: store, linkView: b.RebuildView(),
		census: &rebuildExtractorCensus{byPath: map[string]string{file: "python"}, originByGraphPath: map[string]string{file: file}}}
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
	handler, err := reader.handlerPass().providerHandler(ctx, view, route)
	require.NoError(t, err)
	require.NotNil(t, handler)
	body, err := reader.providerBody(ctx, view, handler)
	require.NoError(t, err)
	bindRebuildProviderRecord(&route, handler.ID, body)
	require.Equal(t, want[0], route)
	// The live bucket still receives every hidden exact registration sibling.
	// A different line under the same All key is not part of that match.
	require.NoError(t, b.WithRebuildContractRecords(ctx, func(records *store_pebble.RebuildContractRecords) error {
		input := func(symbol, ident string, line int) store_pebble.RebuildContractRecord {
			return store_pebble.RebuildContractRecord{Contract: contracts.Contract{
				ID: route.ID, Type: route.Type, Role: route.Role, FilePath: route.FilePath,
				SymbolID: symbol, Line: line, Meta: map[string]any{"handler_ident": ident},
			}}
		}
		for _, item := range []store_pebble.RebuildContractRecord{
			input("register", "view", 5),
			input("register", "unused", 5),
			input("register", "unused", 6),
			input("register_unproved", "no_line", 7),
			input("register_unresolved", "missing", 8),
			input("register_unresolved", "missing_again", 8),
		} {
			require.NoError(t, records.Add(item))
		}
		require.NoError(t, records.ClearID("empty"))
		require.NoError(t, reader.bindRebuildProviderHandlers(ctx, view, records))
		var live []store_pebble.RebuildContractRecord
		require.NoError(t, records.VisitByID(route.ID, func(item store_pebble.RebuildContractRecord) error {
			live = append(live, item)
			return nil
		}))
		require.Len(t, live, 6)
		proved := store_pebble.RebuildContractRecord{Contract: want[0]}
		require.Equal(t, proved, live[0])
		require.Equal(t, proved, live[1])
		var symbols []string
		for i, item := range live {
			symbols = append(symbols, item.SymbolID)
			require.Equal(t, i == 3, item.BodyUnproved)
			require.NotContains(t, item.Meta, "handler_ident")
			require.Equal(t, route.ID, item.ID)
		}
		require.Equal(t, []string{file + "::view", file + "::view", "register", file + "::no_line", "register_unresolved", "register_unresolved"}, symbols)
		var ids []string
		require.NoError(t, records.VisitIDs(func(id string) error {
			ids = append(ids, id)
			return nil
		}))
		require.Equal(t, []string{"empty", route.ID}, ids)
		return nil
	}))
	require.NoError(t, b.WithRebuildContractRecords(ctx, func(records *store_pebble.RebuildContractRecords) error {
		// Two frozen tasks may successively match a live row, after a
		// symbol move. Stationary siblings must never take the first move.
		first := route
		first.SymbolID = "register"
		first.Meta = map[string]any{"handler_ident": "view"}
		second := first
		second.SymbolID = file + "::view"
		second.Meta = map[string]any{"handler_trail": "no_line"}
		consumer := first
		consumer.Role = contracts.RoleConsumer
		consumer.Meta = nil
		foreign := first
		foreign.RepoPrefix = "other"
		foreign.Meta = nil
		otherFile := first
		otherFile.FilePath = "other.py"
		otherFile.Meta = nil
		stationary := first
		stationary.SymbolID = "stationary"
		stationary.Meta = nil
		oracle := contracts.NewRegistry()
		for _, c := range []contracts.Contract{first, second, consumer, foreign, otherFile, stationary} {
			oracle.Add(cloneContractRecords([]contracts.Contract{c})[0])
			require.NoError(t, records.Add(store_pebble.RebuildContractRecord{Contract: c}))
		}
		require.NoError(t, os.WriteFile(path, src, 0o600))
		idx.resolveProviderHandlers(oracle)
		require.NoError(t, os.Remove(path))
		want := oracle.ByID(route.ID)
		require.Len(t, want, 6)
		require.NoError(t, reader.bindRebuildProviderHandlers(ctx, view, records))
		var got []store_pebble.RebuildContractRecord
		require.NoError(t, records.VisitByID(route.ID, func(item store_pebble.RebuildContractRecord) error {
			got = append(got, item)
			return nil
		}))
		require.Len(t, got, 6)
		for i := range 2 {
			require.Equal(t, file+"::no_line", got[i].SymbolID)
			require.True(t, got[i].BodyUnproved)
			require.NotContains(t, got[i].Meta, "handler_trail")
		}
		for i, item := range got {
			if item.BodyUnproved {
				// The site-dependent fallback body is deliberately omitted;
				// all registration fields and the elected symbol still match.
				expected, actual := want[i], item.Contract
				expected.Meta, actual.Meta = nil, nil
				require.Equal(t, expected, actual)
				continue
			}
			require.Equal(t, store_pebble.RebuildContractRecord{Contract: want[i]}, item)
		}
		for i, input := range []contracts.Contract{consumer, foreign, otherFile, stationary} {
			require.Equal(t, store_pebble.RebuildContractRecord{Contract: input}, got[i+2])
		}
		return nil
	}))

	// A hidden-only hint cannot open its bucket for cleanup.
	require.NoError(t, b.WithRebuildContractRecords(ctx, func(records *store_pebble.RebuildContractRecords) error {
		first := route
		first.SymbolID = "same"
		first.Meta = nil
		second := first
		second.Meta = map[string]any{"handler_trail": "unknown"}
		require.NoError(t, records.Add(store_pebble.RebuildContractRecord{Contract: first}))
		require.NoError(t, records.Add(store_pebble.RebuildContractRecord{Contract: second}))
		require.NoError(t, reader.bindRebuildProviderHandlers(ctx, view, records))
		var got []contracts.Contract
		require.NoError(t, records.VisitByID(route.ID, func(item store_pebble.RebuildContractRecord) error {
			got = append(got, item.Contract)
			return nil
		}))
		require.Equal(t, []contracts.Contract{first, second}, got)
		return nil
	}))
	for _, name := range []string{"ambiguous", "no_line"} {
		handler, err := view.Node(ctx, file+"::"+name)
		require.NoError(t, err)
		require.NotNil(t, handler)
		_, err = reader.providerBody(ctx, view, handler)
		require.ErrorIs(t, err, errRebuildContractBodyUnproved, name)
		require.NotErrorIs(t, err, graph.ErrContractProjectionStale, name)
	}
}
