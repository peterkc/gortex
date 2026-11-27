package indexer

import (
	"bytes"
	"io"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/zzet/gortex/internal/config"
	"github.com/zzet/gortex/internal/contracts"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/parser/languages"
)

// D82 discriminator, not a production regression test yet: the primary
// route's prepared tree must reproduce a native raw-source lazy handler read.
func TestRebuildContractTransformedHandlerPrecomputation(t *testing.T) {
	ctx := t.Context()
	root := t.TempDir()
	const handlerPath = "handler.go"
	const handlerID = "handler.go::handler"
	const routeID = "http::GET::/items"
	// A space before the open paren is valid Go but deliberately misses
	// goWriteJSONRe. Only the native AST overlay recognizes this response.
	raw := []byte("package fixture\n\nfunc handler(w Writer) {\n\tWriteJSON (w, 200, \"ok\")\n}\n")
	writeFile(t, filepath.Join(root, handlerPath), string(raw))
	exe, err := os.Executable()
	require.NoError(t, err)
	t.Setenv("GORTEX_REBUILD_CONTRACT_TRANSFORM", "1")
	g := graph.New()
	idx := newTestIndexer(g)
	t.Cleanup(idx.Close)
	idx.SetRootPath(root)
	idx.transforms = newTransformPipeline([]config.TransformRule{{
		Name: "change-response-type", Extensions: []string{".go"},
		Command: []string{exe, "-test.run=^TestRebuildContractTransformChild$"},
	}}, idx.registry, idx.logger)
	prepared, _, err := idx.transforms.runRebuild(ctx, handlerPath, raw, rebuildTransformBounds{
		Dir: t.TempDir(), CaptureBytes: 1024, FileBytes: 1024,
	})
	require.NoError(t, err)
	require.Equal(t, bytes.ReplaceAll(raw, []byte(`"ok"`), []byte("42")), prepared)
	// Hold the worker's only language parse of the handler file. The native
	// lazy handler pass must use these same prepared graph rows.
	ext, ok := idx.registry.GetByLanguage("go")
	require.True(t, ok)
	result, err := ext.Extract(handlerPath, prepared)
	require.NoError(t, err)
	require.NotNil(t, result)
	defer result.ReleaseTree()
	require.NotNil(t, result.Tree)
	g.AddBatch(result.Nodes, result.Edges)
	require.NotNil(t, g.GetNode(handlerID))

	// The registration is in a sibling file. Reach the actual native lazy
	// turn, which chooses the handler from the complete graph and reads raw
	// source via contractFileSrc, not its transformed parse tree.
	reg := contracts.NewRegistry()
	reg.Add(contracts.Contract{
		ID: routeID, Type: contracts.ContractHTTP, Role: contracts.RoleProvider,
		FilePath: "router.go", SymbolID: "router.go::register", Line: 3,
		Meta: map[string]any{"path": "/items", "method": "GET", "handler_ident": "handler", "schema_source": "none"},
	})
	idx.resolveProviderHandlers(reg)
	require.NoError(t, idx.contractInputError())
	native := reg.ByID(routeID)
	require.Len(t, native, 1)
	require.Equal(t, handlerID, native[0].SymbolID)
	require.Equal(t, "router.go", native[0].FilePath)
	require.Equal(t, "string", native[0].Meta["response_type"], "native raw AST overlay")

	// Use the prepared tree with the correct raw lines. Even this mixed-input
	// candidate cannot reproduce the native answer. Raw regex alone does not
	// recover the AST overlay either.
	candidate := func() contracts.Contract {
		return contracts.Contract{ID: routeID, Type: contracts.ContractHTTP, Role: contracts.RoleProvider,
			FilePath: handlerPath, SymbolID: handlerID, Line: 3,
			Meta: map[string]any{"path": "/items", "method": "GET"}}
	}
	regex := candidate()
	contracts.EnrichHTTPContract(&regex, splitLines(raw), result.Nodes, "go")
	require.NotContains(t, regex.Meta, "response_type", "raw regex does not recover the AST-only fact")
	got := candidate()
	contracts.EnrichHTTPContractWithTree(&got, splitLines(raw), result.Nodes, "go", result.Tree)
	require.Equal(t, "int", got.Meta["response_type"], "prepared AST overlay")
	require.Equal(t, native[0].Meta["response_type"], got.Meta["response_type"], "D82 primary one-parse parity")
}

// This diagnostic isolates the native class-name fact from linked winner
// selection. Its second resolver call is a test oracle for the prepared AST,
// not a permitted parse on the build path.
func TestRebuildContractTransformedLaravelClassDiscriminator(t *testing.T) {
	const path = "routes.php"
	const useLine = `use Acme\OrderController as Orders;`
	raw := []byte("<?php\nnamespace Routes;\n" + useLine + "\nRoute::get('/orders', [Orders::class, 'index']);\n")
	// An admitted command transform can erase a use statement without
	// changing either file length or the registration's line/byte offsets.
	prepared := bytes.Replace(raw, []byte(useLine), bytes.Repeat([]byte(" "), len(useLine)), 1)
	require.NotEqual(t, raw, prepared)
	require.Len(t, prepared, len(raw))

	root := t.TempDir()
	writeFile(t, filepath.Join(root, path), string(raw))
	idx := newTestIndexer(graph.New())
	idx.SetRootPath(root)
	t.Cleanup(idx.Close)
	_, facts, err := idx.extractPreparedRebuildFileWithContractFacts(t.Context(), path, languages.NewPHPExtractor(), raw, prepared)
	require.NoError(t, err)
	require.NotNil(t, facts)
	var routes []contracts.Contract
	for _, record := range facts.Records {
		if record.ID == "http::GET::/orders" && record.Role == contracts.RoleProvider {
			routes = append(routes, record)
		}
	}
	require.Len(t, routes, 1)
	route := routes[0]
	require.Equal(t, 4, route.Line)
	require.Equal(t, "index", route.Meta["handler_ident"])
	require.NotEqual(t, "partial", route.Meta["schema_source"])
	require.NotEqual(t, "extracted", route.Meta["schema_source"])
	class, ok := route.Meta["handler_class"].(string)
	require.True(t, ok)
	require.Equal(t, "Orders", class)

	// Native resolveProviderHandlers resolves this class and line against
	// contractFileSrc (raw), not the per-file detector's prepared source.
	nativeSrc := idx.contractFileSrc(path)
	require.Equal(t, raw, nativeSrc)
	nativeResolve := languages.PHPClassNameResolver(nativeSrc)
	preparedResolve := languages.PHPClassNameResolver(prepared)
	require.NotNil(t, nativeResolve)
	require.NotNil(t, preparedResolve)
	require.Equal(t, `Acme\OrderController`, nativeResolve(class, route.Line))
	require.Equal(t, `Routes\Orders`, preparedResolve(class, route.Line))
}

func TestRebuildContractTransformChild(t *testing.T) {
	if os.Getenv("GORTEX_REBUILD_CONTRACT_TRANSFORM") != "1" {
		t.Skip("worker entry point")
	}
	raw, err := io.ReadAll(os.Stdin)
	if err != nil {
		os.Exit(2)
	}
	if _, err := os.Stdout.Write(bytes.ReplaceAll(raw, []byte(`"ok"`), []byte("42"))); err != nil {
		os.Exit(2)
	}
	os.Exit(0)
}
