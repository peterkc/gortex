package indexer

import (
	"bytes"
	"context"
	"io"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/zzet/gortex/internal/config"
	"github.com/zzet/gortex/internal/contracts"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/graph/store_pebble"
	"github.com/zzet/gortex/internal/parser"
	"github.com/zzet/gortex/internal/parser/languages"
)

func TestRebuildContractTransformedHandlerPrecomputation(t *testing.T) {
	ctx := t.Context()
	root, src := rebuildInputFixture(t)
	const handlerPath = "handler.go"
	const handlerID = "handler.go::handler"
	const routeID = "http::GET::/items"
	// The space before the call's opening paren makes this an AST-only
	// response fact. The second callable must use the same raw parse.
	raw := `package fixture

func handler(w Writer) {
	WriteJSON (w, 200, "ok")
}
func other(w Writer) {
	WriteJSON (w, 200, "ok")
}
`
	writeFile(t, filepath.Join(root, handlerPath), raw)
	g := graph.New()
	idx, bounds := rebuildContractTransformFixture(t, g, root)
	input, _, err := readRebuildInput(ctx, src, handlerPath, "go", int64(bounds.FileBytes))
	require.NoError(t, err)
	store := rebuildParseStore{dir: t.TempDir(), limit: 1 << 20}
	recipe := rebuildDigest{1}
	before := parser.ParseCount()
	base, facts, reused, err := idx.loadRebuildFileWithContractFacts(ctx, src, store, input, recipe, bounds)
	require.NoError(t, err)
	require.False(t, reused)
	require.Equal(t, before+2, parser.ParseCount(), "one prepared parse and one raw parse for both callables")
	require.Equal(t, len(raw)-4, base.File.Size, "ordinary graph rows come from the prepared file")
	require.NotContains(t, facts.Handlers[handlerID].Regex, "response_type")
	require.Equal(t, "string", facts.Handlers[handlerID].AST["response_type"])
	require.Equal(t, "string", facts.Handlers["handler.go::other"].AST["response_type"])
	entries, err := os.ReadDir(store.dir)
	require.NoError(t, err)
	require.Len(t, entries, 1, "only the detached receipt is stored")
	key := input.parseKey(idx.rebuildExtractorRecipe(recipe, input))
	wire, err := store.read(key)
	require.NoError(t, err)
	require.False(t, bytes.Contains(wire, []byte(raw)), "source bytes are not a receipt field")
	_, err = store_pebble.DecodeParseFacts(wire)
	require.Error(t, err, "a base-only reader cannot read the contract receipt")
	g.AddBatch(base.Nodes, base.Edges)

	registration := contracts.Contract{
		ID: routeID, Type: contracts.ContractHTTP, Role: contracts.RoleProvider,
		FilePath: "router.go", SymbolID: "router.go::register", Line: 3,
		Meta: map[string]any{"path": "/items", "method": "GET", "handler_ident": "handler", "schema_source": "none"},
	}
	native := contracts.NewRegistry()
	native.Add(cloneContractRecords([]contracts.Contract{registration})[0])
	idx.resolveProviderHandlers(native)
	require.NoError(t, idx.contractInputError())
	want := native.ByID(routeID)
	require.Len(t, want, 1)
	require.Equal(t, handlerID, want[0].SymbolID)
	require.Equal(t, registration.FilePath, want[0].FilePath)
	require.Equal(t, "string", want[0].Meta["response_type"], "native raw AST overlay")

	// A cache hit after deletion must use the receipt, not another transform,
	// a second live-source parse, or a winning handler picked during capture.
	require.NoError(t, os.Remove(filepath.Join(root, handlerPath)))
	before = parser.ParseCount()
	_, saved, reused, err := idx.loadRebuildFileWithContractFacts(ctx, nil, store, input, recipe, bounds)
	require.NoError(t, err)
	require.True(t, reused)
	require.Equal(t, facts, saved)
	idx.contractProviderValue = func(c contracts.Contract, _ []*graph.Node) (map[string]any, bool) {
		require.Equal(t, handlerID, c.SymbolID, "native winner selected at link time")
		require.Equal(t, handlerPath, c.FilePath)
		handler, ok := saved.Handlers[c.SymbolID]
		require.True(t, ok)
		handler.EnrichProvider(&c)
		return c.Meta, true
	}
	replay := contracts.NewRegistry()
	replay.Add(cloneContractRecords([]contracts.Contract{registration})[0])
	idx.resolveProviderHandlers(replay)
	require.NoError(t, idx.contractInputError())
	require.Equal(t, want, replay.ByID(routeID))
	require.Equal(t, before, parser.ParseCount(), "a hit and link replay do not parse")
}

func TestRebuildContractTransformedLaravelClassDiscriminator(t *testing.T) {
	const path = "routes.php"
	const useLine = `use Acme\OrderController as Orders;`
	raw := "<?php\nnamespace Routes {\n" + useLine + "\n" +
		"Route::get('/orders', [Orders::class, 'index']);\n}\n" +
		"namespace Admin {\nRoute::get('/admin', [Dashboard::class, 'index']);\n}\n" +
		"namespace PublicSite {\nRoute::get('/public', [Dashboard::class, 'index']);\n}\n"
	root, src := rebuildInputFixture(t)
	writeFile(t, filepath.Join(root, path), raw)
	idx, bounds := rebuildContractTransformFixture(t, graph.New(), root)
	input, _, err := readRebuildInput(t.Context(), src, path, "php", int64(bounds.FileBytes))
	require.NoError(t, err)
	store := rebuildParseStore{dir: t.TempDir(), limit: 1 << 20}
	recipe := rebuildDigest{1}
	before := parser.ParseCount()
	base, facts, reused, err := idx.loadRebuildFileWithContractFacts(t.Context(), src, store, input, recipe, bounds)
	require.NoError(t, err)
	require.False(t, reused)
	require.Equal(t, before+2, parser.ParseCount(), "one raw parse for both registration lines")
	wantClasses := map[string]string{
		"4:Orders":     `Acme\OrderController`,
		"7:Dashboard":  `Admin\Dashboard`,
		"10:Dashboard": `PublicSite\Dashboard`,
	}
	require.Equal(t, wantClasses, facts.LaravelHandlerClasses)
	require.Len(t, facts.Records, len(wantClasses))

	// The prepared detector still emits the unqualified hint. Only the raw
	// registration's namespace/import resolver owns the answer.
	nativeSrc := idx.contractFileSrc(path)
	require.Equal(t, []byte(raw), nativeSrc)
	native := languages.PHPClassNameResolver(nativeSrc)
	prepared := languages.PHPClassNameResolver(bytes.ReplaceAll([]byte(raw), []byte(useLine), bytes.Repeat([]byte(" "), len(useLine))))
	require.NotNil(t, native)
	require.NotNil(t, prepared)
	for _, route := range facts.Records {
		_, class := contracts.LaravelHandlerClassKey(route)
		if route.Line == 4 {
			require.Equal(t, "Orders", class)
			require.Equal(t, `Routes\Orders`, prepared(class, route.Line))
		}
	}

	// Reopen the same store with a fresh indexer. No live bytes are available.
	require.NoError(t, os.Remove(filepath.Join(root, path)))
	restarted, restartedBounds := rebuildContractTransformFixture(t, graph.New(), root)
	before = parser.ParseCount()
	cached, saved, reused, err := restarted.loadRebuildFileWithContractFacts(t.Context(), nil,
		rebuildParseStore{dir: store.dir, limit: store.limit}, input, recipe, restartedBounds)
	require.NoError(t, err)
	require.True(t, reused)
	require.Equal(t, base.File, cached.File)
	require.Equal(t, facts, saved)
	for _, route := range saved.Records {
		key, class := contracts.LaravelHandlerClassKey(route)
		answer, ok := saved.LaravelHandlerClasses[key]
		require.True(t, ok)
		require.Equal(t, wantClasses[key], answer)
		require.Equal(t, native(class, route.Line), answer)
	}
	require.Equal(t, before, parser.ParseCount(), "restart hit does not parse")
}

func TestRebuildContractRawParseEligibility(t *testing.T) {
	for _, tc := range []struct {
		name, path, lang, raw string
		classes               map[string]string
	}{
		{name: "unchanged Go", path: "handler.go", lang: "go", raw: "package p\nfunc handler() {}\n"},
		{name: "changed Go without callable", path: "handler.go", lang: "go", raw: "package p\nconst val = \"ok\"\n"},
		{name: "unchanged Laravel", path: "routes.php", lang: "php", raw: "<?php\nnamespace Routes;\nRoute::get('/orders', [Orders::class, 'index']);\n",
			classes: map[string]string{"3:Orders": `Routes\Orders`}},
		{name: "changed PHP without hinted route", path: "routes.php", lang: "php", raw: "<?php\nuse Acme\\OrderController as Orders;\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root, src := rebuildInputFixture(t)
			writeFile(t, filepath.Join(root, tc.path), tc.raw)
			idx, bounds := rebuildContractTransformFixture(t, graph.New(), root)
			input, _, err := readRebuildInput(t.Context(), src, tc.path, tc.lang, int64(bounds.FileBytes))
			require.NoError(t, err)
			store := rebuildParseStore{dir: t.TempDir(), limit: 1 << 20}
			before := parser.ParseCount()
			_, facts, reused, err := idx.loadRebuildFileWithContractFacts(t.Context(), src, store, input, rebuildDigest{1}, bounds)
			require.NoError(t, err)
			require.False(t, reused)
			require.Equal(t, tc.classes, facts.LaravelHandlerClasses)
			require.Equal(t, before+1, parser.ParseCount(), "only the prepared parse")
		})
	}
}

func TestRebuildContractReceiptRefusesMissingOrMismatchedAcceptedBytes(t *testing.T) {
	for _, tc := range []string{"missing", "wrong digest"} {
		t.Run(tc, func(t *testing.T) {
			root, src := rebuildInputFixture(t)
			const rel = "handler.go"
			path := filepath.Join(root, rel)
			const raw = "package p\nfunc handler() { WriteJSON (w, 200, \"ok\") }\n"
			writeFile(t, path, raw)
			idx, bounds := rebuildContractTransformFixture(t, graph.New(), root)
			input, _, err := readRebuildInput(t.Context(), src, rel, "go", int64(bounds.FileBytes))
			require.NoError(t, err)
			if tc == "missing" {
				require.NoError(t, os.Remove(path))
			} else {
				// Same length, so only content binding can detect the edit.
				require.NoError(t, os.WriteFile(path, bytes.ReplaceAll([]byte(raw), []byte(`"ok"`), []byte(`"no"`)), 0o600))
			}
			store := rebuildParseStore{dir: t.TempDir(), limit: 1 << 20}
			before := parser.ParseCount()
			base, facts, reused, errInput := idx.loadRebuildFileWithContractFacts(t.Context(), src, store, input, rebuildDigest{1}, bounds)
			require.ErrorIs(t, errInput, ErrBuildInputChanged)
			require.Empty(t, base)
			require.Nil(t, facts)
			require.False(t, reused)
			require.Equal(t, before, parser.ParseCount())
			files, err := os.ReadDir(store.dir)
			require.NoError(t, err)
			require.Empty(t, files, "refusal cannot publish a receipt")

			var reply, acks bytes.Buffer
			require.NoError(t, writeRebuildBuildReply(&reply, buildRefusal(errInput)))
			err = receiveRebuildStages(t.Context(), "", rebuildBuildRequest{}, &reply, &acks,
				func(context.Context, store_pebble.StageTables) error {
					t.Fatal("refused input was offered")
					return nil
				})
			require.ErrorIs(t, err, ErrBuildInputChanged)
			require.Zero(t, acks.Len(), "the typed refusal is not an offer")
		})
	}
}

func rebuildContractTransformFixture(t *testing.T, g graph.Store, root string) (*Indexer, rebuildTransformBounds) {
	t.Helper()
	exe, err := os.Executable()
	require.NoError(t, err)
	t.Setenv("GORTEX_REBUILD_CONTRACT_TRANSFORM", "1")
	idx := newTestIndexer(g)
	idx.registry.Register(languages.NewPHPExtractor())
	t.Cleanup(idx.Close)
	idx.SetRootPath(root)
	idx.transforms = newTransformPipeline([]config.TransformRule{{
		Name: "rewrite-contract-fixture", Extensions: []string{".go", ".php"},
		Command: []string{exe, "-test.run=^TestRebuildContractTransformChild$"},
	}}, idx.registry, idx.logger)
	return idx, rebuildTransformBounds{Dir: t.TempDir(), CaptureBytes: 1024, FileBytes: 1024}
}

func TestRebuildContractTransformChild(t *testing.T) {
	if os.Getenv("GORTEX_REBUILD_CONTRACT_TRANSFORM") != "1" {
		t.Skip("worker entry point")
	}
	raw, err := io.ReadAll(os.Stdin)
	if err != nil {
		os.Exit(2)
	}
	useLine := []byte(`use Acme\OrderController as Orders;`)
	out := bytes.ReplaceAll(raw, useLine, bytes.Repeat([]byte(" "), len(useLine)))
	out = bytes.ReplaceAll(out, []byte(`"ok"`), []byte("42"))
	if _, err := os.Stdout.Write(out); err != nil {
		os.Exit(2)
	}
	os.Exit(0)
}

func TestRebuildContractRawUTF16NativeParity(t *testing.T) {
	const handler = "handler.go::handler"
	const goSource = "package p\nfunc handler() { WriteJSON (w, 200, \"ok\") }\n"
	const phpSource = "<?php\nnamespace Routes;\nuse Acme\\OrderController as Orders;\nRoute::get('/orders', [Orders::class, 'index']);\n"
	for _, tc := range []struct {
		name, path, lang, source string
		withBOM                  bool
	}{
		{"Go BOM", "handler.go", "go", goSource, true},
		{"Go unmarked", "handler.go", "go", goSource + "// Enough code units for the BOM-less UTF-16 sniff.\n", false},
		{"PHP BOM", "routes.php", "php", phpSource, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root, src := rebuildInputFixture(t)
			raw := utf16LENoBOM(t, tc.source)
			if tc.withBOM {
				raw = utf16LEWithBOM(t, tc.source)
			}
			require.NoError(t, os.WriteFile(filepath.Join(root, tc.path), raw, 0o600))
			g := graph.New()
			idx := newTestIndexer(g)
			idx.registry.Register(languages.NewPHPExtractor())
			idx.SetRootPath(root)
			t.Cleanup(idx.Close)
			require.Equal(t, []byte(tc.source), idx.transforms.run(tc.path, raw), "native preparation must decode this fixture")
			input, _, err := readRebuildInput(t.Context(), src, tc.path, tc.lang, 1024)
			require.NoError(t, err)
			store := rebuildParseStore{dir: t.TempDir(), limit: 1 << 20}
			recipe := rebuildDigest{1}
			before := parser.ParseCount()
			base, _, reused, err := idx.loadRebuildFileWithContractFacts(t.Context(), src, store, input, recipe, rebuildTransformBounds{FileBytes: 1024})
			require.NoError(t, err)
			require.False(t, reused)
			require.Equal(t, before+1, parser.ParseCount(), "the raw guard declines before tree-sitter")
			_, saved, reused, err := idx.loadRebuildFileWithContractFacts(t.Context(), nil, store, input, recipe, rebuildTransformBounds{})
			require.NoError(t, err)
			require.True(t, reused)
			nativeSrc := idx.contractFileSrc(tc.path)
			require.Equal(t, raw, nativeSrc)
			if tc.lang == "php" {
				require.Nil(t, languages.PHPClassNameResolver(nativeSrc))
				require.Len(t, saved.Records, 1)
				key, nativeClass := contracts.LaravelHandlerClassKey(saved.Records[0])
				require.Equal(t, "Orders", nativeClass)
				require.Equal(t, nativeClass, saved.LaravelHandlerClasses[key])
				return
			}
			f := saved.Handlers[handler]
			require.Nil(t, f.AST)
			require.Nil(t, f.Bindings)
			g.AddBatch(base.Nodes, base.Edges)
			const id = "http::GET::/"
			registration := contracts.Contract{
				ID:       id,
				Role:     contracts.RoleProvider,
				Type:     contracts.ContractHTTP,
				FilePath: "router.go",
				Line:     1,
				Meta:     map[string]any{"handler_ident": "handler", "path": "/"},
			}
			native := contracts.NewRegistry()
			native.Add(cloneContractRecords([]contracts.Contract{registration})[0])
			idx.resolveProviderHandlers(native)
			want := native.ByID(id)
			require.Len(t, want, 1)
			require.Equal(t, handler, want[0].SymbolID)
			require.NotContains(t, want[0].Meta, "response_type")
			idx.contractProviderValue = func(c contracts.Contract, _ []*graph.Node) (map[string]any, bool) {
				f.EnrichProvider(&c)
				return c.Meta, true
			}
			replay := contracts.NewRegistry()
			replay.Add(cloneContractRecords([]contracts.Contract{registration})[0])
			idx.resolveProviderHandlers(replay)
			require.Equal(t, want, replay.ByID(id))
		})
	}
}

func TestRebuildContractRawUTF16ShortUnmarkedNativeSkip(t *testing.T) {
	const path = "handler.go"
	const source = "package p\nfunc handler() { WriteJSON (w, 200, \"ok\") }\n"
	ctx := t.Context()
	root, src := rebuildInputFixture(t)
	raw := utf16LENoBOM(t, source)
	absolute := filepath.Join(root, path)
	require.NoError(t, os.WriteFile(absolute, raw, 0o600))
	idx := newTestIndexer(graph.New())
	idx.SetRootPath(root)
	t.Cleanup(idx.Close)
	ext, ok := idx.registry.GetByLanguage("go")
	require.True(t, ok)
	prepared := idx.transforms.run(path, raw)
	require.Equal(t, raw, prepared, "the unmarked sample is below the native detection threshold")
	before := parser.ParseCount()
	native, skipped, err := idx.extractFileWithRawLease(nil, nil, nil, absolute, path, "go", ext, prepared)
	require.NoError(t, err)
	require.NotNil(t, native)
	defer native.ReleaseTree()
	require.True(t, skipped)
	require.Len(t, native.Nodes, 1)
	require.Equal(t, true, native.Nodes[0].Meta["skipped_due_to_binary"])
	require.Equal(t, before, parser.ParseCount(), "native never enters tree-sitter")

	input, _, err := readRebuildInput(ctx, src, path, "go", 1024)
	require.NoError(t, err)
	store := rebuildParseStore{dir: t.TempDir(), limit: 1 << 20}
	base, facts, reused, err := idx.loadRebuildFileWithContractFacts(ctx, src, store, input, rebuildDigest{1}, rebuildTransformBounds{FileBytes: 1024})
	require.NoError(t, err)
	require.False(t, reused)
	require.Equal(t, native.Nodes, base.Nodes)
	require.True(t, facts.ScanSkipped)
	require.Empty(t, facts.Records)
	require.Empty(t, facts.Handlers)
	require.Equal(t, before, parser.ParseCount(), "rebuild also skips before either parse")
}

func TestRebuildContractRawParseNativeSkips(t *testing.T) {
	for _, tc := range []struct {
		path, raw string
		noAST     bool
	}{
		{path: "main.GO", raw: "package p\nfunc H() { WriteJSON (w, 200, \"raw\") }\n"},
		{path: "main.go", noAST: true},
	} {
		t.Run(tc.path, func(t *testing.T) {
			idx := newTestIndexer(graph.New())
			t.Cleanup(idx.Close)
			prepared := []byte("package p\nfunc H() { WriteJSON (w, 200, 42) }\n")
			before := parser.ParseCount()
			_, facts, err := idx.extractPreparedRebuildFileWithContractFacts(t.Context(), tc.path, languages.NewGoExtractor(), []byte(tc.raw), prepared)
			require.NoError(t, err)
			require.Equal(t, before+1, parser.ParseCount())
			h, ok := facts.Handlers[tc.path+"::H"]
			require.True(t, ok)
			if tc.noAST {
				require.Nil(t, h.AST)
			}
			require.Empty(t, h.AST)
			require.Nil(t, h.Bindings)
		})
	}
}

func TestRebuildContractRawTreeRefusal(t *testing.T) {
	idx := newTestIndexer(graph.New())
	t.Cleanup(idx.Close)
	ctx := t.Context()
	raw := []byte("package p\nfunc H() {}\n")
	prepared := []byte("package p\nfunc H() { println(0) }\n")
	before := parser.ParseCount()
	canceled, cancel := context.WithCancel(ctx)
	cancel()
	err := idx.captureRebuildContractBodies(canceled, "main.go", "go", raw, prepared, nil, &parser.ExtractionResult{}, true, &contracts.FileFacts{})
	require.ErrorIs(t, err, contracts.ErrContractFactTree)
	require.ErrorIs(t, err, context.Canceled)
	require.Equal(t, before, parser.ParseCount())

	tree := contracts.ParseTreeForLang("go", prepared)
	require.NotNil(t, tree)
	defer tree.Release()
	err = idx.captureRebuildContractBodies(ctx, "main.go", "go", raw, raw, nil, &parser.ExtractionResult{Tree: tree}, true, &contracts.FileFacts{})
	require.ErrorIs(t, err, contracts.ErrContractFactTree)
	require.ErrorContains(t, err, "main.go: go handler facts")

	const path = "routes.php"
	raw = []byte("<?php\nRoute::get('/orders', [Orders::class, 'index']);\n")
	route := contracts.Contract{
		Role: contracts.RoleProvider, Type: contracts.ContractHTTP, FilePath: path, Line: 2,
		Meta: map[string]any{"handler_ident": "index", "handler_class": "Orders"},
	}
	f := &contracts.FileFacts{Records: []contracts.Contract{route}}
	err = idx.captureRebuildContractBodies(ctx, path, "php", raw, raw, nil, &parser.ExtractionResult{}, false, f)
	require.ErrorIs(t, err, contracts.ErrContractFactTree)
	require.ErrorContains(t, err, "routes.php: php handler facts")
	require.Nil(t, languages.PHPClassNameResolverWithTree(nil))
	require.Nil(t, languages.PHPClassNameResolverWithTree(tree))

	// An already-enriched provider does not enter the native class pass even
	// if its prepared record still carries the class hint.
	f.Records[0].Meta["schema_source"] = "extracted"
	before = parser.ParseCount()
	require.NoError(t, idx.captureRebuildContractBodies(ctx, path, "php", raw, bytes.TrimSpace(raw), nil, &parser.ExtractionResult{}, false, f))
	require.Nil(t, f.LaravelHandlerClasses)
	require.Equal(t, before, parser.ParseCount())
	f.Records[0].ID = "http::GET::/orders"
	reg := contracts.NewRegistry()
	reg.Add(f.Records[0])
	idx.resolveProviderHandlers(reg)
	require.Equal(t, before, parser.ParseCount(), "native also skips class qualification")
}

func TestRebuildContractRawGrammarFollowsPath(t *testing.T) {
	idx := newTestIndexer(graph.New())
	t.Cleanup(idx.Close)
	ctx := t.Context()
	t.Run("Go path", func(t *testing.T) {
		const path = "handler.go"
		raw := []byte("package p\nfunc handler() { value := \"ok\"; WriteJSON (w, 200, value) }\n")
		handler := &graph.Node{ID: path + "::handler", Kind: graph.KindFunction, Name: "handler",
			FilePath: path, StartLine: 2, EndLine: 2, Language: "php"}
		nodes := []*graph.Node{handler}
		f := &contracts.FileFacts{Shapes: map[string]*contracts.Shape{}}
		before := parser.ParseCount()
		require.NoError(t, idx.captureRebuildContractBodies(ctx, path, "php", raw, raw, nil,
			&parser.ExtractionResult{Nodes: nodes}, true, f))
		require.Equal(t, before+1, parser.ParseCount(), "the prepared extractor is not Go")
		base := store_pebble.ParseFacts{
			Nodes:             nodes,
			File:              graph.FileMetaRow{FilePath: path, NodeCount: 1},
			EffectiveLanguage: "php",
		}
		wire, err := store_pebble.EncodeContractParseFacts(base, f)
		require.NoError(t, err)
		_, saved, _, err := store_pebble.DecodeContractParseFacts(wire)
		require.NoError(t, err)
		want := contracts.Contract{SymbolID: handler.ID, FilePath: path, Role: contracts.RoleProvider}
		got := want
		tree := contracts.ParseTreeForLang(detectLangFromPath(handler.FilePath), raw)
		require.NotNil(t, tree)
		defer tree.Release()
		contracts.EnrichHTTPContractWithTree(&want, splitLines(raw), nodes, "go", tree)
		require.Equal(t, "string", want.Meta["response_type"])
		saved.Handlers[handler.ID].EnrichProvider(&got)
		require.Equal(t, want, got)
		require.Equal(t, contracts.MakeBodyFacts(tree, handler).VarBinding("value"), saved.Handlers[handler.ID].VarBinding("value"))
	})
	t.Run("PHP path", func(t *testing.T) {
		const path = "routes.php"
		raw := []byte("<?php\nnamespace Routes;\nuse Acme\\OrderController as Orders;\n")
		route := contracts.Contract{
			ID:       "http::GET::/orders",
			Role:     contracts.RoleProvider,
			Type:     contracts.ContractHTTP,
			FilePath: path,
			Line:     4,
			Meta:     map[string]any{"handler_ident": "index", "handler_class": "Orders"},
		}
		f := &contracts.FileFacts{
			Records:  []contracts.Contract{route},
			Shapes:   map[string]*contracts.Shape{},
			Handlers: map[string]contracts.HandlerFacts{},
		}
		before := parser.ParseCount()
		require.NoError(t, idx.captureRebuildContractBodies(ctx, path, "go", raw, raw, nil, &parser.ExtractionResult{}, false, f))
		require.Equal(t, before+1, parser.ParseCount(), "the prepared extractor is not PHP")
		base := store_pebble.ParseFacts{File: graph.FileMetaRow{FilePath: path}, EffectiveLanguage: "go"}
		wire, err := store_pebble.EncodeContractParseFacts(base, f)
		require.NoError(t, err)
		_, saved, _, err := store_pebble.DecodeContractParseFacts(wire)
		require.NoError(t, err)
		resolve := languages.PHPClassNameResolver(raw)
		require.NotNil(t, resolve)
		key, class := contracts.LaravelHandlerClassKey(route)
		require.Equal(t, `Acme\OrderController`, saved.LaravelHandlerClasses[key])
		require.Equal(t, resolve(class, route.Line), saved.LaravelHandlerClasses[key])
	})
}

func TestRebuildContractReceiptIgnoresOwnerScope(t *testing.T) {
	const path = "routes.go"
	src := []byte(`package p
const VALUE = "value"
func routes(r Router) {
	r.GET("/literal", handler)
	r.GET(EXTERNAL, handler)
}
func handler() {}
`)
	idx := newTestIndexer(graph.New())
	t.Cleanup(idx.Close)
	encode := func() []byte {
		base, facts, err := idx.extractPreparedRebuildFileWithContractFacts(t.Context(), path, languages.NewGoExtractor(), src, src)
		require.NoError(t, err)
		require.NotEmpty(t, base.Constants)
		require.NotEmpty(t, facts.Records)
		require.NotEmpty(t, facts.Questions)
		wire, err := store_pebble.EncodeContractParseFacts(base, facts)
		require.NoError(t, err)
		return wire
	}
	want := encode()
	idx.SetRepoPrefix("repo")
	idx.SetWorkspaceID("work")
	idx.SetProjectID("project")
	require.Equal(t, want, encode())
}
