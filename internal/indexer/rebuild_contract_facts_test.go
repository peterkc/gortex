package indexer

import (
	"bytes"
	"maps"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/zzet/gortex/internal/contracts"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/graph/store_pebble"
	"github.com/zzet/gortex/internal/parser"
	"github.com/zzet/gortex/internal/parser/languages"
)

func TestRebuildContractFactsNativeParity(t *testing.T) {
	ctx := t.Context()
	root := t.TempDir()
	g := graph.New()
	idx := newTestIndexer(g)
	idx.registry.Register(languages.NewTypeScriptExtractor())
	idx.SetRootPath(root)
	t.Cleanup(idx.Close)
	type captured struct {
		parsed store_pebble.ParseFacts
		facts  *contracts.FileFacts
		source []byte
	}
	files := map[string]captured{}
	resolve := func(records []contracts.Contract, questions []contracts.EndpointContractQuestion, store contracts.EndpointConstStore) []contracts.Contract {
		t.Helper()
		got, err := contracts.ResolveEndpointQuestions(records, questions, store)
		require.NoError(t, err)
		return got
	}
	capture := func(path, lang, text string) captured {
		t.Helper()
		absolute := filepath.Join(root, path)
		require.NoError(t, os.MkdirAll(filepath.Dir(absolute), 0o700))
		require.NoError(t, os.WriteFile(absolute, []byte(text), 0o600))
		ext, ok := idx.registry.GetByLanguage(lang)
		require.True(t, ok)
		src := []byte(text)
		parsed, facts, err := idx.extractPreparedRebuildFileWithContractFacts(ctx, path, ext, src, src)
		require.NoError(t, err)
		require.NotNil(t, facts)
		g.AddBatch(parsed.Nodes, parsed.Edges)
		require.NoError(t, g.BulkSetConstantValues("", parsed.Constants))
		f := captured{parsed, facts, src}
		files[path] = f
		return f
	}
	// Parse the route before its constant and handler exist. The consumer
	// receipt must retain the question, not a failed or stale answer.
	router := capture("app/routes.go", "go", `package app
func Register(r Router) { r.GET(Route, Handle) }
`)
	require.Empty(t, router.facts.Records)
	require.Len(t, router.facts.Questions, 1)
	require.Equal(t, []contracts.EndpointTerm{{Name: "Route"}}, router.facts.Questions[0].Expression.Terms)
	// Keep the router-context superset even before the constant exists. The
	// native census only admits this file once the route question resolves.
	require.NotNil(t, router.facts.Router)
	wantQuestionRouter := contracts.RouterFileFacts("app/routes.go", router.source)
	require.Equal(t, &wantQuestionRouter, router.facts.Router)
	routerFile := ContractFollowupFile{Path: "app/routes.go", Language: "go"}
	unresolved := resolve(router.facts.Records, router.facts.Questions, g)
	require.Empty(t, unresolved)
	require.False(t, contractRouterSupported(routerFile, unresolved))
	capture("app/constant.go", "go", "package app\nconst Route = \"/api/items\"\n")
	require.True(t, contractRouterSupported(routerFile, resolve(router.facts.Records, router.facts.Questions, g)))
	handler := capture("app/handler.go", "go", `package app
func Handle(w Writer, r RequestReader) {
	var payload Input
	json.NewDecoder(r.Body).Decode(&payload)
	WriteJSON(w, 200, Output{})
}
`)
	capture("other/handler.go", "go", `package other
func Handle(w Writer) { WriteJSON(w, 500, Other{}) }
`)
	capture("app/types.go", "go", `package app
type Input struct {
	Name string
}
type Output struct {
	Count int
}
type Duplicate struct {
	One bool
}
`)
	capture("other/types.go", "go", `package other
type Duplicate struct {
	Two int
}
`)
	// TypeScript uses the existing tree in the gRPC import gate and native
	// regex HTTP extraction and route-prefix facts for the literal routes.
	ts := capture("web/routes.ts", "typescript", `import express from 'express';
import { UsersClient } from './users_pb';
const router = express.Router();
router.get('/items', handler);
app.use('/api', router);
const client = new UsersClient('host');
client.getUser({});
export interface DTO {
  name: string;
}
`)
	_, byLang := idx.buildPerFileContractExtractors()
	for path, f := range files {
		var fileID string
		for _, n := range f.parsed.Nodes {
			if n.Kind == graph.KindFile {
				fileID = n.ID
				break
			}
		}
		var edges []*graph.Edge
		for _, e := range f.parsed.Edges {
			if e.From == fileID {
				edges = append(edges, e)
			}
		}
		// The oracle uses the native accepted-source extraction at the
		// complete graph turn. Reparse is allowed only in this oracle.
		tree := contracts.ParseTreeForLang(f.parsed.EffectiveLanguage, f.source)
		want := idx.runContractExtractorsForFile(path, f.source, f.parsed.Nodes, edges, byLang[f.parsed.EffectiveLanguage], tree)
		tree.Release()
		got := resolve(f.facts.Records, f.facts.Questions, g)
		require.Equal(t, want, got, path)
		for _, n := range f.parsed.Nodes {
			if n.Kind == graph.KindType || n.Kind == graph.KindInterface {
				shape, ok := f.facts.Shapes[n.ID]
				require.True(t, ok, n.ID)
				require.Equal(t, contracts.ExtractShape(n.FilePath, f.source, n.StartLine, n.EndLine), shape, n.ID)
			}
		}
	}
	require.NotNil(t, files["app/types.go"].facts.Shapes["app/types.go::Duplicate"])
	require.NotNil(t, files["other/types.go"].facts.Shapes["other/types.go::Duplicate"])
	require.NotEqual(t, files["app/types.go"].facts.Shapes["app/types.go::Duplicate"], files["other/types.go"].facts.Shapes["other/types.go::Duplicate"])
	wantRouter := contracts.RouterFileFacts("web/routes.ts", ts.source)
	require.Equal(t, &wantRouter, ts.facts.Router)
	var grpcFound bool
	for _, c := range ts.facts.Records {
		if c.Type == contracts.ContractGRPC {
			grpcFound = true
		}
	}
	require.True(t, grpcFound, "fixture must exercise the native TS gRPC AST import gate")

	routes := resolve(router.facts.Records, router.facts.Questions, g)
	require.Len(t, routes, 1)
	require.Equal(t, "http::GET::/api/items", routes[0].ID)
	reg := contracts.NewRegistry()
	nativeInput := routes[0]
	nativeInput.Meta = maps.Clone(nativeInput.Meta)
	reg.Add(nativeInput)
	idx.resolveProviderHandlers(reg)
	require.NoError(t, idx.contractInputError())
	resolved := reg.ByID(routes[0].ID)
	require.Len(t, resolved, 1)
	require.Equal(t, "app/handler.go::Handle", resolved[0].SymbolID, "native same-directory handler wins")
	got := routes[0]
	got.Meta = maps.Clone(got.Meta)
	got.SymbolID = resolved[0].SymbolID
	body, ok := handler.facts.Handlers[got.SymbolID]
	require.True(t, ok)
	body.EnrichProvider(&got)
	delete(got.Meta, "handler_ident")
	delete(got.Meta, "handler_trail")
	require.Equal(t, resolved[0], got)
	require.Equal(t, []int{200}, got.Meta["status_codes"])
	// Run the native linked type-name turn on both records. The DTOs live
	// in another file, but their detached shape facts are keyed by full ID.
	gotRegistry := contracts.NewRegistry()
	gotRegistry.Add(got)
	idx.upgradeContractBareTypeRefs(reg)
	idx.upgradeContractBareTypeRefs(gotRegistry)
	require.Equal(t, reg.ByID(got.ID), gotRegistry.ByID(got.ID))
	resolved = reg.ByID(got.ID)
	require.Equal(t, "app/types.go::Input", resolved[0].Meta["request_type"])
	require.Equal(t, "app/types.go::Output", resolved[0].Meta["response_type"])
	for _, name := range []string{"request_type", "response_type"} {
		id := resolved[0].Meta[name].(string)
		n := g.GetNode(id)
		require.NotNil(t, n)
		require.Equal(t, contracts.ExtractShape(n.FilePath, files[n.FilePath].source, n.StartLine, n.EndLine), files[n.FilePath].facts.Shapes[id])
		require.NotNil(t, files[n.FilePath].facts.Shapes[id])
	}

	// Change only the producer's constant values; reuse the same raw route
	// fact and check absence of the old route, then the ambiguous case.
	require.NoError(t, g.BulkSetConstantValues("", []graph.ConstantValueRow{{NodeID: "app/constant.go::Route", FilePath: "app/constant.go", Value: "/api/new"}}))
	routes = resolve(router.facts.Records, router.facts.Questions, g)
	require.Len(t, routes, 1)
	require.Equal(t, "http::GET::/api/new", routes[0].ID)
	g.AddNode(&graph.Node{ID: "other/constant.go::Route", Name: "Route", Kind: graph.KindConstant, FilePath: "other/constant.go"})
	require.NoError(t, g.BulkSetConstantValues("", []graph.ConstantValueRow{{NodeID: "other/constant.go::Route", FilePath: "other/constant.go", Value: "/api/ambiguous"}}))
	require.Empty(t, resolve(router.facts.Records, router.facts.Questions, g))
}

func TestRebuildContractFactsRequiredTreeAndScanSkip(t *testing.T) {
	ctx := t.Context()
	idx := newTestIndexer(graph.New())
	t.Cleanup(idx.Close)
	file := func(path string, lang string) *parser.ExtractionResult {
		return &parser.ExtractionResult{Nodes: []*graph.Node{{ID: path, FilePath: path, Kind: graph.KindFile, Language: lang}}}
	}
	for _, tc := range []struct{ path, lang, text string }{
		{"x.go", "go", "package p\nfunc f() { r.GET(Route, f) }"},
		{"x.ts", "typescript", "import { SClient } from './s_pb'; const c = new SClient('host');"},
		{"x.py", "python", "import requests\nrequests.get(url)"},
		{"x.tf", "hcl", `resource "aws_lambda_function" "fn" {}`},
	} {
		t.Run(tc.path, func(t *testing.T) {
			src := []byte(tc.text)
			_, err := idx.captureRebuildContractFacts(ctx, tc.path, tc.lang, src, src, file(tc.path, tc.lang), false)
			require.ErrorIs(t, err, contracts.ErrContractFactTree)
		})
	}
	// No tree is supplied: if the oversized scan did not skip, this would
	// refuse by type. There must be no truncation or record extraction.
	large := append([]byte("package p\nfunc f() { r.GET(Route, f) }\n"), bytes.Repeat([]byte(" "), maxContractSourceBytes)...)
	facts, err := idx.captureRebuildContractFacts(ctx, "large.go", "go", large, large, file("large.go", "go"), false)
	require.NoError(t, err)
	require.True(t, facts.ScanSkipped)
	require.Empty(t, facts.Records)
	require.Empty(t, facts.Questions)
	cfg := []byte("app:\n  feature: private-value\n")
	configFacts, err := idx.captureRebuildContractFacts(ctx, "application.yaml", "yaml", cfg, cfg, file("application.yaml", "yaml"), false)
	require.NoError(t, err)
	require.Len(t, configFacts.ConfigKeys, 1)
	require.Equal(t, "app.feature", configFacts.ConfigKeys[0].Name)
	require.NotContains(t, configFacts.ConfigKeys[0].Meta, "value")
	// A transform may change the Go AST. Even when capture has a prepared
	// tree, the detached provider answer must come from the raw AST.
	ext, ok := idx.registry.GetByLanguage("go")
	require.True(t, ok)
	raw := []byte("package p\nfunc H() { WriteJSON(w, 200, \"ok\") }\n")
	prepared := []byte(strings.ReplaceAll(string(raw), `"ok"`, "42"))
	_, transformed, err := idx.extractPreparedRebuildFileWithContractFacts(ctx, "handler.go", ext, raw, prepared)
	require.NoError(t, err)
	require.Contains(t, transformed.Handlers, "handler.go::H")
}

func TestRebuildContractFactsRetainRequiredTrees(t *testing.T) {
	idx := newTestIndexer(graph.New())
	idx.SetRootPath(t.TempDir())
	t.Cleanup(idx.Close)
	for _, tc := range []struct {
		path string
		ext  parser.Extractor
		src  string
		id   string
	}{
		{"client.py", languages.NewPythonExtractor(), "import requests\ndef f():\n    return requests.get('/api/users')\n", "http::GET::/api/users"},
		{"client.rb", languages.NewRubyExtractor(), "require 'faraday'\nFaraday.get('/api/users')\n", "http::GET::/api/users"},
		{"client.php", languages.NewPHPExtractor(), "<?php\nuse GuzzleHttp\\Client;\n$client = new Client();\n$client->get('/api/users');\n", "http::GET::/api/users"},
		{"Client.java", languages.NewJavaExtractor(), `import org.springframework.web.client.RestTemplate;
class Client {
    String fetch() {
        RestTemplate rt = new RestTemplate();
        return rt.getForObject("/api/users", String.class);
    }
}`, "http::GET::/api/users"},
		{"Client.kt", languages.NewKotlinExtractor(), `import org.springframework.web.client.RestTemplate
fun fetch(): String {
    val rt = RestTemplate()
    return rt.getForObject("/api/users", String::class.java)
}`, "http::GET::/api/users"},
		{"Client.scala", languages.NewScalaExtractor(), `import sttp.client3._
val request = basicRequest.get(uri"/api/users")`, "http::GET::/api/users"},
		{"client.rs", languages.NewRustExtractor(), `use reqwest::Client;
async fn fetch(client: Client) { client.get("/api/users").send().await; }`, "http::GET::/api/users"},
		{"lambda.tf", languages.NewHCLExtractor(), `resource "aws_lambda_function" "processor" {
  environment {
    variables = { MAX_BATCH_SIZE = "100" }
  }
}`, ""},
		{"client.ts", languages.NewTypeScriptExtractor(), `import { UsersClient } from './users_pb';
const client = new UsersClient('host');`, "grpc::Users"},
		{"client.TSX", languages.NewTypeScriptExtractor(), `import { UsersClient } from './users_pb';
const client = new UsersClient('host');`, "grpc::Users"},
		{"routes.go", languages.NewGoExtractor(), `package p
func Register(r Router) { r.GET("/api/users", Register) }`, "http::GET::/api/users"},
	} {
		t.Run(tc.path, func(t *testing.T) {
			src := []byte(tc.src)
			before := parser.ParseCount()
			parsed, facts, err := idx.extractPreparedRebuildFileWithContractFacts(t.Context(), tc.path, tc.ext, src, src)
			require.NoError(t, err)
			require.Equal(t, before+1, parser.ParseCount(), "exactly one accepted parse")
			require.NotNil(t, facts)
			require.NotEmpty(t, facts.Records)
			if tc.id != "" {
				var ids []string
				for _, record := range facts.Records {
					ids = append(ids, record.ID)
				}
				require.Contains(t, ids, tc.id)
			}
			legacy, err := idx.extractPreparedRebuildFile(t.Context(), tc.path, tc.ext, src)
			require.NoError(t, err)
			require.Equal(t, legacy, parsed, "capture changes no ordinary parse facts")
			candidate, err := store_pebble.EncodeContractParseFacts(parsed, facts)
			require.NoError(t, err)
			decoded, decodedFacts, factBytes, err := store_pebble.DecodeContractParseFacts(candidate)
			require.NoError(t, err)
			require.Positive(t, factBytes)
			require.Equal(t, facts, decodedFacts)
			legacyBytes, err := store_pebble.EncodeParseFacts(legacy)
			require.NoError(t, err)
			decodedBytes, err := store_pebble.EncodeParseFacts(decoded)
			require.NoError(t, err)
			require.Equal(t, legacyBytes, decodedBytes)

			var fileID string
			for _, n := range legacy.Nodes {
				if n.Kind == graph.KindFile {
					fileID = n.ID
					break
				}
			}
			require.NotEmpty(t, fileID)
			var fileEdges []*graph.Edge
			for _, edge := range legacy.Edges {
				if edge.From == fileID {
					fileEdges = append(fileEdges, edge)
				}
			}
			_, byLanguage := idx.buildPerFileContractExtractors()
			want := idx.runContractExtractorsForFile(tc.path, src, legacy.Nodes, fileEdges, byLanguage[tc.ext.Language()], nil)
			got, err := contracts.ResolveEndpointQuestions(decodedFacts.Records, decodedFacts.Questions, nil)
			require.NoError(t, err)
			require.Equal(t, want, got, "complete decoded records must match the native nil-tree path")
		})
	}
}

func TestRebuildContractFactsTSParseErrorParity(t *testing.T) {
	idx := newTestIndexer(graph.New())
	idx.SetRootPath(t.TempDir())
	t.Cleanup(idx.Close)
	ext := languages.NewTypeScriptExtractor()
	src := []byte("export function f() { const x = ; }\n")
	plain, err := idx.extractPreparedRebuildFile(t.Context(), "broken.ts", ext, src)
	require.NoError(t, err)
	captured, _, err := idx.extractPreparedRebuildFileWithContractFacts(t.Context(), "broken.ts", ext, src, src)
	require.NoError(t, err)
	require.Empty(t, plain.File.Errors)
	require.Equal(t, plain, captured)
	retained, err := ext.WithRetainedTree().Extract("broken.ts", src)
	require.NoError(t, err)
	defer retained.ReleaseTree()
	require.Nil(t, retained.Tree)
	require.True(t, retained.ContractTree.HasParseErrors(), "fixture must contain a real parse error")
}

func TestRebuildContractFactsKotlinRouterPrefix(t *testing.T) {
	idx := newTestIndexer(graph.New())
	idx.SetRootPath(t.TempDir())
	t.Cleanup(idx.Close)
	const path = "UserController.kt"
	src := []byte(`@RestController
@RequestMapping("/api")
class UserController {
    @GetMapping("/users")
    fun list(): String { return "users" }
}`)
	_, facts, err := idx.extractPreparedRebuildFileWithContractFacts(t.Context(), path, languages.NewKotlinExtractor(), src, src)
	require.NoError(t, err)
	require.NotNil(t, facts.Router)
	reg := contracts.NewRegistry()
	for _, record := range facts.Records {
		reg.Add(record)
	}
	contracts.JoinRouterPrefixFacts(reg, map[string]contracts.RouterFacts{path: *facts.Router})
	require.NotEmpty(t, reg.ByID("http::GET::/api/users"))
}

func TestRebuildContractFactsNoHiddenFallback(t *testing.T) {
	idx := newTestIndexer(graph.New())
	t.Cleanup(idx.Close)
	before := parser.ParseCount()
	// Terraform's marker prefilter says no tree is required. The native
	// Extract (nil tree) would still parse; capture must reach ExtractWithTree.
	got := idx.collectContractRecordsForFile("empty.tf", []byte(`variable "key" {}`), nil, nil,
		[]contracts.Extractor{&contracts.TerraformExtractor{}}, nil, &contracts.EndpointFactCapture{})
	require.Empty(t, got)
	require.Equal(t, before, parser.ParseCount())
	// Isolate the handler-tree refusal from the earlier HTTP/Topic check.
	src := []byte("package p\nfunc f() {}\n")
	result := &parser.ExtractionResult{Nodes: []*graph.Node{{ID: "x.go::f", Name: "f", Kind: graph.KindFunction, FilePath: "x.go", StartLine: 2}}}
	_, err := idx.captureRebuildContractFacts(t.Context(), "x.go", "go", src, src, result, false)
	require.ErrorIs(t, err, contracts.ErrContractFactTree)
}
