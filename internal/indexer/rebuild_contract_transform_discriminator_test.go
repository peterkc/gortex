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
	key := input.parseKey(idx.rebuildExtractorRecipe(rebuildContractCandidateRecipe(recipe), input))
	wire, err := store.read(key)
	require.NoError(t, err)
	require.False(t, bytes.Contains(wire, []byte(raw)), "source bytes are not a receipt field")
	_, err = store_pebble.DecodeParseFacts(wire)
	require.Error(t, err, "active v2 cannot read the candidate")
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
	raw := "<?php\nnamespace Routes;\n" + useLine + "\n" +
		"Route::get('/orders', [Orders::class, 'index']);\n" +
		"Route::get('/orders/all', [Orders::class, 'index']);\n"
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
	require.Len(t, facts.LaravelHandlerClasses, 2)
	require.Len(t, facts.Records, 2)
	require.Contains(t, facts.LaravelHandlerClasses, "4:Orders")
	require.Contains(t, facts.LaravelHandlerClasses, "5:Orders")

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
		require.Equal(t, "Orders", class)
		require.Equal(t, `Routes\Orders`, prepared(class, route.Line))
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
		require.Equal(t, `Acme\OrderController`, answer)
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
