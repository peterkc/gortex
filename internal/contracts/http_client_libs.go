package contracts

import (
	"fmt"
	"strings"

	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/parser"
	sitter "github.com/zzet/gortex/internal/parser/tsitter"
	javasrc "github.com/zzet/gortex/internal/parser/tsitter/java"
	kotlinsrc "github.com/zzet/gortex/internal/parser/tsitter/kotlin"
	phpsrc "github.com/zzet/gortex/internal/parser/tsitter/php"
	pysrc "github.com/zzet/gortex/internal/parser/tsitter/python"
	rubysrc "github.com/zzet/gortex/internal/parser/tsitter/ruby"
	rustsrc "github.com/zzet/gortex/internal/parser/tsitter/rust"
	scalasrc "github.com/zzet/gortex/internal/parser/tsitter/scala"
)

// HTTP client-library consumer detection.
//
// This pass broadens HTTP *consumer* detection beyond the JS/TS axios/fetch
// heuristics to the major client libraries of Python, Rust, Ruby, PHP, Java,
// Kotlin and Scala. The defining constraint is PRECISION: a bare substring of
// the call text (`requests.get(...)`, `client.get(...)`) is not enough — that
// would flag a local variable named `requests`, a `surf::get` call (a library
// we deliberately do not register), or any `obj.get(...)` accessor. Instead a
// call is treated as an HTTP consumer only when its receiver resolves — via the
// file's parsed import / use / require statements — to one of the registered
// client libraries below.
//
// Matching strategy (per file):
//
//  1. Parse the file's imports with the language grammar and keep only the
//     libraries whose import path is actually present (activeLibs). No
//     registered import ⇒ no consumer contract from this pass at all. This is
//     what rejects `surf::get` (surf is unregistered), a local `requests`
//     variable in a file that never `import requests`, and a bare `client.get`
//     with no client-library import.
//  2. Bind call receivers to a library:
//     - a *module / global / crate / builder* receiver name (`requests`,
//       `httpx`, `Faraday`, `Net::HTTP`, `reqwest`, `basicRequest`) matched
//       against the active library's module set, honouring import aliases; and
//     - a *typed variable* whose declared / constructed type is one of the
//       active library's client types (`Client`, `RestTemplate`, `WebClient`,
//       `OkHttpClient`), resolved from the file's params, let-bindings and
//       constructor expressions.
//  3. Map the call's method suffix to an HTTP method via httpClientVerb
//     (`.get`→GET, `.post`→POST, …, `.getForObject`→GET; the generic
//     `.request("METHOD", url)` / `.exchange(url, METHOD, …)` forms read the
//     verb out of an argument).
//  4. Resolve the URL argument through ResolveEndpointArg(forRoute=true) so a
//     string literal AND a const reference both resolve and a filesystem-y
//     literal is guarded out, then mint a consumer Contract with the same
//     canonical `http::<METHOD>::<path>` ID the other HTTP consumers use so the
//     matcher pairs it with a provider.

// clientLibrary describes one HTTP client library for a language.
type clientLibrary struct {
	// name is the framework label stamped on the contract's Meta["framework"].
	name string
	// importTokens are substrings of an import / use / require path that
	// activate this library. Matched against the file's *resolved* import
	// statements only — never against arbitrary call text — so a same-named
	// local identifier cannot activate the library.
	importTokens []string
	// modules are receiver identifiers that are themselves a callable HTTP
	// surface once the library is active: a module (`requests`, `httpx`), a
	// crate (`reqwest`), a global constant (`Faraday`, `Net::HTTP`), or a
	// builder value (`basicRequest`). A call `<recv>.<verb>(url)` with recv in
	// this set (or an alias of it) is a consumer.
	modules []string
	// types are client TYPE names whose instances are an HTTP surface
	// (`Client`, `RestTemplate`, `OkHttpClient`). A local variable whose
	// declared / constructed type is one of these is treated as a receiver.
	types []string
}

// httpClientLibraries is the per-language registry of recognised HTTP client
// libraries. Deliberately conservative: only libraries whose call surface can
// be bound precisely to a resolved import are listed, so that unregistered
// crates such as surf / hyper / isahc never mint a false consumer contract.
var httpClientLibraries = map[string][]clientLibrary{
	"python": {
		{name: "requests", importTokens: []string{"requests"}, modules: []string{"requests"}},
		{name: "httpx", importTokens: []string{"httpx"}, modules: []string{"httpx"}, types: []string{"Client", "AsyncClient"}},
		{name: "aiohttp", importTokens: []string{"aiohttp"}, modules: []string{"aiohttp"}, types: []string{"ClientSession"}},
		{name: "urllib3", importTokens: []string{"urllib3", "urllib.request"}, modules: []string{"urllib3"}, types: []string{"PoolManager"}},
	},
	"rust": {
		// reqwest only — surf / hyper / isahc are intentionally excluded so a
		// `surf::get(...)` call is never a consumer.
		{name: "reqwest", importTokens: []string{"reqwest"}, modules: []string{"reqwest"}, types: []string{"Client"}},
	},
	"ruby": {
		{name: "Faraday", importTokens: []string{"faraday"}, modules: []string{"Faraday"}, types: []string{"Faraday"}},
		{name: "HTTParty", importTokens: []string{"httparty"}, modules: []string{"HTTParty"}},
		{name: "Net::HTTP", importTokens: []string{"net/http"}, modules: []string{"Net::HTTP"}},
		{name: "RestClient", importTokens: []string{"rest-client", "rest_client", "restclient"}, modules: []string{"RestClient"}},
	},
	"php": {
		{name: "Guzzle", importTokens: []string{"GuzzleHttp", "Guzzle"}, modules: []string{"Guzzle"}, types: []string{"Client"}},
	},
	"java":   jvmHTTPClientLibraries,
	"kotlin": jvmHTTPClientLibraries,
	"scala": {
		{name: "sttp", importTokens: []string{"sttp.client", "sttp"}, modules: []string{"basicRequest", "quickRequest", "emptyRequest"}},
	},
}

// jvmHTTPClientLibraries is shared by Java and Kotlin (same import paths and
// client types). OkHttp is registered for completeness, but its idiomatic call
// shape (`client.newCall(request)`) carries the URL on the Request builder
// rather than the verb call, so in practice the RestTemplate / WebClient shapes
// (`recv.getForObject("/url", …)`) are what this pass mints.
var jvmHTTPClientLibraries = []clientLibrary{
	{name: "okhttp3", importTokens: []string{"okhttp3", "okhttp"}, types: []string{"OkHttpClient"}},
	{name: "RestTemplate", importTokens: []string{"web.client.RestTemplate", "RestTemplate"}, types: []string{"RestTemplate"}},
	{name: "WebClient", importTokens: []string{"function.client.WebClient", "WebClient"}, types: []string{"WebClient"}},
}

// httpClientVerb maps a (lower-cased) client method name to its HTTP method.
// Covers the universal `.get/.post/...` shapes plus the Spring RestTemplate
// `getForObject` / `postForEntity` idioms. Method names not present here fall
// through to the generic request/exchange handling or are ignored.
func httpClientVerb(method string) (string, bool) {
	switch strings.ToLower(method) {
	case "get", "getforobject", "getforentity", "getasync":
		return "GET", true
	case "post", "postforobject", "postforentity", "postforlocation", "postasync":
		return "POST", true
	case "put", "putforobject", "putasync":
		return "PUT", true
	case "delete", "deleteasync":
		return "DELETE", true
	case "patch", "patchforobject", "patchasync":
		return "PATCH", true
	case "head":
		return "HEAD", true
	case "options":
		return "OPTIONS", true
	}
	return "", false
}

// clientLibAdapter encapsulates the grammar-specific parsing for one language:
// how to obtain the grammar, collect imports, collect variable→type bindings,
// and walk method-call sites.
type clientLibAdapter struct {
	grammar func() *sitter.Language
	// collectImports returns the import / use / require path strings in the file.
	collectImports func(root *sitter.Node, src []byte) []string
	// collectVarTypes maps a local variable / parameter name to the bare type
	// name it was declared or constructed with (e.g. client→Client, rt→RestTemplate).
	collectVarTypes func(root *sitter.Node, src []byte) map[string]string
	// walkCalls invokes emit for every method-call site, passing the receiver
	// text, the bare method name, the positional argument nodes (unwrapped), and
	// the 1-based call line.
	walkCalls func(root *sitter.Node, src []byte, emit clientCallEmitter)
}

type clientCallEmitter func(recv, method string, args []*sitter.Node, line int)

var clientLibAdapters = map[string]*clientLibAdapter{
	"python": {grammar: pysrc.GetLanguage, collectImports: pythonImports, collectVarTypes: pythonVarTypes, walkCalls: pythonWalkCalls},
	"rust":   {grammar: rustsrc.GetLanguage, collectImports: rustImports, collectVarTypes: rustVarTypes, walkCalls: rustWalkCalls},
	"ruby":   {grammar: rubysrc.GetLanguage, collectImports: rubyImports, collectVarTypes: rubyVarTypes, walkCalls: rubyWalkCalls},
	"php":    {grammar: phpsrc.GetLanguage, collectImports: phpImports, collectVarTypes: phpVarTypes, walkCalls: phpWalkCalls},
	"java":   {grammar: javasrc.GetLanguage, collectImports: javaImports, collectVarTypes: javaVarTypes, walkCalls: javaWalkCalls},
	"kotlin": {grammar: kotlinsrc.GetLanguage, collectImports: kotlinImports, collectVarTypes: kotlinVarTypes, walkCalls: kotlinWalkCalls},
	"scala":  {grammar: scalasrc.GetLanguage, collectImports: scalaImports, collectVarTypes: scalaVarTypes, walkCalls: scalaWalkCalls},
}

// detectClientLibConsumers scans a non-Go source file for HTTP client-library
// consumer calls, gated by the file's resolved imports. Returns nil when the
// language is unsupported, the file imports no registered library, or no call
// binds to a library. The supplied tree is used when present (production); a
// fresh tree is parsed otherwise (the regex-path test harness supplies nil).
func (h *HTTPExtractor) detectClientLibConsumers(
	filePath, lang string,
	src []byte,
	lines []string,
	fileNodes []*graph.Node,
	suppliedTree *parser.ParseTree,
	store EndpointConstStore,
	repoPrefix string,
	base int,
) []Contract {
	libs := httpClientLibraries[lang]
	ad := clientLibAdapters[lang]
	if len(libs) == 0 || ad == nil {
		return nil
	}
	// Cheap textual prefilter: bail before parsing when the file does not even
	// mention a registered import token. This is a perf gate only — the real
	// gate is the parsed-import check below.
	if !srcMentionsClientLib(src, libs) {
		return nil
	}

	// Use the supplied tree when it is for this language; otherwise parse one.
	tree := suppliedTree
	var own *parser.ParseTree
	if tree == nil || tree.Tree() == nil || (tree.Lang() != "" && tree.Lang() != lang) {
		own = buildClientLibTree(lang, src)
		tree = own
	}
	if own != nil {
		defer own.Release()
	}
	if tree == nil || tree.Tree() == nil {
		return nil
	}
	root := tree.Tree().RootNode()
	if root == nil {
		return nil
	}

	active := activeClientLibraries(libs, ad.collectImports(root, src))
	if len(active) == 0 {
		return nil
	}

	env := buildClientReceiverEnv(active, ad.collectVarTypes(root, src))
	if len(env) == 0 {
		return nil
	}

	var out []Contract
	capture, _ := store.(*EndpointFactCapture)
	var candidates []EndpointContractQuestion
	seen := map[string]bool{}
	ad.walkCalls(root, src, func(recv, method string, args []*sitter.Node, line int) {
		libName, ok := env[recv]
		if !ok {
			return
		}
		httpMethod, urlArg := clientCallTarget(method, args, src)
		if urlArg == nil {
			return
		}
		// Only capture lowers detached terms; the ordinary path retains its
		// resolve → filter → dedup → owner/enrich order.
		if capture != nil {
			q := endpointExpression(urlArg, src)
			if len(q.Terms) > 0 {
				c := Contract{
					Type: ContractHTTP, Role: RoleConsumer,
					SymbolID: findEnclosingSymbol(fileNodes, line), FilePath: filePath, Line: line,
					Meta: map[string]any{"method": httpMethod, "framework": libName}, Confidence: 0.9,
				}
				EnrichHTTPContractWithTree(&c, lines, fileNodes, lang, suppliedTree)
				candidates = append(candidates, EndpointContractQuestion{Position: base, Kind: EndpointHTTPClient, Expression: q, Record: c})
			}
			return
		}
		path, ok := ResolveEndpointArg(urlArg, src, filePath, repoPrefix, store, true)
		if !ok || path == "" {
			return
		}
		// Reject filesystem / static-asset literals the same way the regex
		// consumer path does — only rooted "/..." literals are gated.
		if strings.HasPrefix(path, "/") &&
			(!IsLikelyHTTPRouteLiteral(path, "") || IsStaticAssetPath(path)) {
			return
		}
		normPath, origNames := NormalizeHTTPPathWithParams(path)
		contractID := fmt.Sprintf("http::%s::%s", httpMethod, normPath)
		// One contract per (id, line) so a chained call walked twice does not
		// double-emit.
		dedup := contractID + "@" + fmt.Sprint(line)
		if seen[dedup] {
			return
		}
		seen[dedup] = true

		c := Contract{
			ID:         contractID,
			Type:       ContractHTTP,
			Role:       RoleConsumer,
			SymbolID:   findEnclosingSymbol(fileNodes, line),
			FilePath:   filePath,
			Line:       line,
			Meta: map[string]any{
				"method":    httpMethod,
				"path":      normPath,
				"framework": libName,
			},
			Confidence: 0.9,
		}
		if len(origNames) > 0 {
			c.Meta["path_param_names"] = origNames
		}
		// Enrich through the same pipeline the regex consumers use. Pass the
		// ORIGINAL supplied tree (nil in the regex-path test harness) — only Go
		// has a BodyFacts factory, so a self-built non-Go tree would be a no-op
		// for enrichment anyway, and passing it could only diverge from the
		// regex path's behaviour.
		EnrichHTTPContractWithTree(&c, lines, fileNodes, lang, suppliedTree)
		out = append(out, c)
	})
	if capture != nil {
		for _, q := range candidates {
			if q.Expression.HasQuestion() {
				capture.Questions = append(capture.Questions, candidates...)
				return nil
			}
		}
		// The whole subpass is literal; native output needs no late lookup.
		for i := range candidates {
			candidates[i].Position = 0
		}
		return resolveEndpointQuestions(nil, candidates, nil)
	}
	return out
}

func completeEndpointHTTPClient(c *Contract, path string) bool {
	if strings.HasPrefix(path, "/") && (!IsLikelyHTTPRouteLiteral(path, "") || IsStaticAssetPath(path)) {
		return false
	}
	normPath, origNames := NormalizeHTTPPathWithParams(path)
	c.ID = fmt.Sprintf("http::%s::%s", c.Meta["method"], normPath)
	c.Meta["path"] = normPath
	if len(origNames) > 0 {
		c.Meta["path_param_names"] = origNames
	}
	if params := pathParamsFromTemplate(normPath); len(params) > 0 {
		c.Meta["path_params"] = params
	}
	return true
}

// clientCallTarget resolves a client method call to its HTTP method and URL
// argument node. The generic `request(METHOD, url)` / `exchange(url, METHOD …)`
// forms read the verb out of an argument; everything else maps the method
// suffix and uses the first argument as the URL.
func clientCallTarget(method string, args []*sitter.Node, src []byte) (string, *sitter.Node) {
	if len(args) == 0 {
		return "", nil
	}
	switch strings.ToLower(method) {
	case "request", "run_request":
		// recv.request("POST", "/url", …) — verb first, URL second.
		if len(args) >= 2 {
			if v := verbLiteral(args[0], src); v != "" {
				return v, args[1]
			}
		}
		return "", nil
	case "exchange":
		// Spring RestTemplate exchange(url, HttpMethod.X, …) — URL first.
		return "ANY", args[0]
	}
	if hm, ok := httpClientVerb(method); ok {
		return hm, args[0]
	}
	return "", nil
}

// verbLiteral extracts an HTTP verb from an argument node that may be a string
// literal ("POST"), a Ruby symbol (:post), or a Rust/Java member like
// Method::POST / HttpMethod.POST.
func verbLiteral(n *sitter.Node, src []byte) string {
	if n == nil {
		return ""
	}
	raw := strings.TrimSpace(n.Content(src))
	raw = strings.Trim(raw, `"'`+"`")
	raw = strings.TrimPrefix(raw, ":") // ruby symbol
	if i := strings.LastIndexAny(raw, ":."); i >= 0 {
		raw = raw[i+1:] // Method::POST / HttpMethod.POST
	}
	switch strings.ToUpper(raw) {
	case "GET", "POST", "PUT", "DELETE", "PATCH", "HEAD", "OPTIONS":
		return strings.ToUpper(raw)
	}
	return ""
}

// srcMentionsClientLib reports whether the raw source mentions any of the
// libraries' import tokens. A cheap pre-parse gate; the authoritative check is
// the parsed-import activeClientLibraries pass.
func srcMentionsClientLib(src []byte, libs []clientLibrary) bool {
	s := string(src)
	for _, lib := range libs {
		for _, tok := range lib.importTokens {
			if strings.Contains(s, tok) {
				return true
			}
		}
		for _, m := range lib.modules {
			if strings.Contains(s, m) {
				return true
			}
		}
		for _, t := range lib.types {
			if strings.Contains(s, t) {
				return true
			}
		}
	}
	return false
}

// activeClientLibraries keeps only the libraries whose import token appears in
// the file's resolved import paths.
func activeClientLibraries(libs []clientLibrary, imports []string) []clientLibrary {
	if len(imports) == 0 {
		return nil
	}
	var out []clientLibrary
	for _, lib := range libs {
		if importMatchesLib(lib, imports) {
			out = append(out, lib)
		}
	}
	return out
}

func importMatchesLib(lib clientLibrary, imports []string) bool {
	for _, imp := range imports {
		for _, tok := range lib.importTokens {
			if importPathMatches(imp, tok) {
				return true
			}
		}
	}
	return false
}

// importPathMatches reports whether an import path activates a token. A token
// matches when it equals the path, is a dotted / scoped / slashed segment
// prefix or suffix of it, or (for a dotted token like "urllib.request") is a
// substring on a separator boundary. Kept strict enough that "requests" does
// not match an unrelated "myrequests" module.
func importPathMatches(importPath, token string) bool {
	if importPath == token {
		return true
	}
	// Normalise scope/namespace separators to dots so one rule covers
	// rust `reqwest::Client`, php `GuzzleHttp\Client`, java `okhttp3.OkHttpClient`,
	// ruby `net/http`, scala `sttp.client3`.
	norm := func(s string) string {
		s = strings.ReplaceAll(s, "::", ".")
		s = strings.ReplaceAll(s, "\\", ".")
		s = strings.ReplaceAll(s, "/", ".")
		return s
	}
	ip := norm(importPath)
	tk := norm(token)
	if ip == tk {
		return true
	}
	segs := strings.Split(ip, ".")
	// segment-boundary prefix: token is the leading segment(s).
	if strings.HasPrefix(ip, tk+".") {
		return true
	}
	// token equals any single segment (e.g. "RestTemplate" in a fully-qualified
	// import, or "reqwest" in "reqwest.Client").
	for _, s := range segs {
		if s == tk {
			return true
		}
	}
	// dotted token (e.g. "web.client.RestTemplate") as a trailing run.
	if strings.HasSuffix(ip, "."+tk) {
		return true
	}
	return false
}

// buildClientReceiverEnv maps a receiver identifier to the library name it
// belongs to, from the active libraries' module names plus any local variable
// whose declared / constructed type is one of the libraries' client types.
func buildClientReceiverEnv(active []clientLibrary, varTypes map[string]string) map[string]string {
	env := map[string]string{}
	typeToLib := map[string]string{}
	for _, lib := range active {
		for _, m := range lib.modules {
			env[m] = lib.name
		}
		for _, t := range lib.types {
			typeToLib[t] = lib.name
		}
	}
	for v, typ := range varTypes {
		if libName, ok := typeToLib[bareTypeName(typ)]; ok {
			env[v] = libName
		}
	}
	return env
}

// bareTypeName strips a qualifier / generic / reference decoration off a type
// name: `reqwest::Client` → Client, `Client<Foo>` → Client, `&Client` → Client.
func bareTypeName(typ string) string {
	typ = strings.TrimSpace(typ)
	typ = strings.TrimLeft(typ, "&*")
	if i := strings.IndexAny(typ, "<("); i >= 0 {
		typ = typ[:i]
	}
	if i := strings.LastIndexAny(typ, ":.\\/"); i >= 0 {
		typ = typ[i+1:]
	}
	return strings.TrimSpace(typ)
}

// buildClientLibTree parses src with the language's grammar. Used only when the
// caller did not supply a tree (the regex-path test harness).
func buildClientLibTree(lang string, src []byte) *parser.ParseTree {
	ad := clientLibAdapters[lang]
	if ad == nil || ad.grammar == nil || len(src) == 0 {
		return nil
	}
	tree, err := parser.ParseFile(src, ad.grammar())
	if err != nil || tree == nil {
		return nil
	}
	return parser.NewParseTree(tree, src, lang)
}

// walkNodes invokes fn on n and every descendant, named children only.
func walkNodes(n *sitter.Node, fn func(*sitter.Node)) {
	if n == nil {
		return
	}
	fn(n)
	for i := 0; i < int(n.NamedChildCount()); i++ {
		walkNodes(n.NamedChild(i), fn)
	}
}

// nodeLine returns the 1-based start line of a node.
func nodeLine(n *sitter.Node) int {
	if n == nil {
		return 0
	}
	return int(n.StartPoint().Row) + 1
}

// positionalArgs returns the named children of an argument-list node, skipping
// keyword / named-argument wrappers identified by skip.
func positionalArgs(argList *sitter.Node, skip func(*sitter.Node) bool) []*sitter.Node {
	if argList == nil {
		return nil
	}
	var out []*sitter.Node
	for i := 0; i < int(argList.NamedChildCount()); i++ {
		ch := argList.NamedChild(i)
		if ch == nil {
			continue
		}
		if skip != nil && skip(ch) {
			continue
		}
		out = append(out, ch)
	}
	return out
}

// ---------------------------------------------------------------------------
// Python
// ---------------------------------------------------------------------------

func pythonImports(root *sitter.Node, src []byte) []string {
	var out []string
	walkNodes(root, func(n *sitter.Node) {
		switch n.Type() {
		case "import_statement":
			for i := 0; i < int(n.NamedChildCount()); i++ {
				ch := n.NamedChild(i)
				switch ch.Type() {
				case "dotted_name", "identifier":
					out = append(out, ch.Content(src))
				case "aliased_import":
					if name := ch.ChildByFieldName("name"); name != nil {
						out = append(out, name.Content(src))
					}
				}
			}
		case "import_from_statement":
			if mod := n.ChildByFieldName("module_name"); mod != nil {
				out = append(out, mod.Content(src))
			}
		}
	})
	return out
}

func pythonVarTypes(root *sitter.Node, src []byte) map[string]string {
	out := map[string]string{}
	walkNodes(root, func(n *sitter.Node) {
		if n.Type() != "assignment" {
			return
		}
		left := n.ChildByFieldName("left")
		right := n.ChildByFieldName("right")
		if left == nil || right == nil || left.Type() != "identifier" {
			return
		}
		// x = httpx.Client(...) / x = ClientSession(...) — the constructed type
		// is the callee's trailing name.
		if right.Type() == "call" {
			if fn := right.ChildByFieldName("function"); fn != nil {
				out[left.Content(src)] = calleeTrailingName(fn, src)
			}
		}
	})
	return out
}

// calleeTrailingName returns the trailing identifier of a call's function
// expression: `httpx.Client` → Client, `Client` → Client.
func calleeTrailingName(fn *sitter.Node, src []byte) string {
	switch fn.Type() {
	case "identifier":
		return fn.Content(src)
	case "attribute":
		if a := fn.ChildByFieldName("attribute"); a != nil {
			return a.Content(src)
		}
	}
	return bareTypeName(fn.Content(src))
}

func pythonWalkCalls(root *sitter.Node, src []byte, emit clientCallEmitter) {
	walkNodes(root, func(n *sitter.Node) {
		if n.Type() != "call" {
			return
		}
		fn := n.ChildByFieldName("function")
		if fn == nil || fn.Type() != "attribute" {
			return
		}
		obj := fn.ChildByFieldName("object")
		attr := fn.ChildByFieldName("attribute")
		if obj == nil || attr == nil || obj.Type() != "identifier" {
			return
		}
		args := positionalArgs(n.ChildByFieldName("arguments"), func(c *sitter.Node) bool {
			return c.Type() == "keyword_argument"
		})
		emit(obj.Content(src), attr.Content(src), args, nodeLine(n))
	})
}

// ---------------------------------------------------------------------------
// Rust
// ---------------------------------------------------------------------------

func rustImports(root *sitter.Node, src []byte) []string {
	var out []string
	walkNodes(root, func(n *sitter.Node) {
		if n.Type() != "use_declaration" {
			return
		}
		if arg := n.ChildByFieldName("argument"); arg != nil {
			out = append(out, arg.Content(src))
		}
	})
	return out
}

func rustVarTypes(root *sitter.Node, src []byte) map[string]string {
	out := map[string]string{}
	bind := func(pattern, typ *sitter.Node) {
		if pattern == nil || typ == nil || pattern.Type() != "identifier" {
			return
		}
		out[pattern.Content(src)] = bareTypeName(typ.Content(src))
	}
	walkNodes(root, func(n *sitter.Node) {
		switch n.Type() {
		case "parameter":
			bind(n.ChildByFieldName("pattern"), n.ChildByFieldName("type"))
		case "let_declaration":
			pattern := n.ChildByFieldName("pattern")
			if typ := n.ChildByFieldName("type"); typ != nil {
				bind(pattern, typ)
				return
			}
			// let client = reqwest::Client::new(); — infer the constructed type.
			if val := n.ChildByFieldName("value"); val != nil && pattern != nil && pattern.Type() == "identifier" {
				if t := rustConstructedType(val, src); t != "" {
					out[pattern.Content(src)] = t
				}
			}
		}
	})
	return out
}

// rustConstructedType pulls the type out of a `Type::new()` / `Type::builder()`
// constructor expression.
func rustConstructedType(val *sitter.Node, src []byte) string {
	target := val
	for target != nil && target.Type() == "call_expression" {
		target = target.ChildByFieldName("function")
	}
	if target == nil {
		return ""
	}
	if target.Type() == "scoped_identifier" {
		if p := target.ChildByFieldName("path"); p != nil {
			return bareTypeName(p.Content(src))
		}
	}
	return ""
}

func rustWalkCalls(root *sitter.Node, src []byte, emit clientCallEmitter) {
	walkNodes(root, func(n *sitter.Node) {
		if n.Type() != "call_expression" {
			return
		}
		fn := n.ChildByFieldName("function")
		args := rustArgs(n.ChildByFieldName("arguments"))
		if fn == nil {
			return
		}
		switch fn.Type() {
		case "field_expression":
			// client.post(...) — value is the receiver, field the method.
			recv := fn.ChildByFieldName("value")
			field := fn.ChildByFieldName("field")
			if recv == nil || field == nil || recv.Type() != "identifier" {
				return
			}
			emit(recv.Content(src), field.Content(src), args, nodeLine(n))
		case "scoped_identifier":
			// reqwest::get(...) — path is the crate, name the method.
			path := fn.ChildByFieldName("path")
			name := fn.ChildByFieldName("name")
			if path == nil || name == nil || path.Type() != "identifier" {
				return
			}
			emit(path.Content(src), name.Content(src), args, nodeLine(n))
		}
	})
}

func rustArgs(argList *sitter.Node) []*sitter.Node {
	return positionalArgs(argList, nil)
}

// ---------------------------------------------------------------------------
// Ruby
// ---------------------------------------------------------------------------

func rubyImports(root *sitter.Node, src []byte) []string {
	var out []string
	walkNodes(root, func(n *sitter.Node) {
		if n.Type() != "call" {
			return
		}
		method := n.ChildByFieldName("method")
		if method == nil || (method.Content(src) != "require" && method.Content(src) != "require_relative") {
			return
		}
		if args := n.ChildByFieldName("arguments"); args != nil {
			for i := 0; i < int(args.NamedChildCount()); i++ {
				if v, ok := crossLangStringLiteral(args.NamedChild(i), src); ok {
					out = append(out, v)
				}
			}
		}
	})
	return out
}

func rubyVarTypes(root *sitter.Node, src []byte) map[string]string {
	out := map[string]string{}
	walkNodes(root, func(n *sitter.Node) {
		if n.Type() != "assignment" {
			return
		}
		left := n.ChildByFieldName("left")
		right := n.ChildByFieldName("right")
		if left == nil || right == nil || left.Type() != "identifier" {
			return
		}
		// conn = Faraday.new(...) — receiver constant is the "type".
		if right.Type() == "call" {
			if recv := right.ChildByFieldName("receiver"); recv != nil {
				out[left.Content(src)] = bareTypeName(recv.Content(src))
			}
		}
	})
	return out
}

func rubyWalkCalls(root *sitter.Node, src []byte, emit clientCallEmitter) {
	walkNodes(root, func(n *sitter.Node) {
		if n.Type() != "call" {
			return
		}
		recv := n.ChildByFieldName("receiver")
		method := n.ChildByFieldName("method")
		if recv == nil || method == nil {
			return
		}
		args := positionalArgs(n.ChildByFieldName("arguments"), func(c *sitter.Node) bool {
			return c.Type() == "pair"
		})
		emit(recv.Content(src), method.Content(src), args, nodeLine(n))
	})
}

// ---------------------------------------------------------------------------
// PHP
// ---------------------------------------------------------------------------

func phpImports(root *sitter.Node, src []byte) []string {
	var out []string
	walkNodes(root, func(n *sitter.Node) {
		if n.Type() == "namespace_use_clause" || n.Type() == "qualified_name" {
			out = append(out, n.Content(src))
		}
	})
	return out
}

func phpVarTypes(root *sitter.Node, src []byte) map[string]string {
	out := map[string]string{}
	walkNodes(root, func(n *sitter.Node) {
		if n.Type() != "assignment_expression" {
			return
		}
		left := n.ChildByFieldName("left")
		right := n.ChildByFieldName("right")
		if left == nil || right == nil || left.Type() != "variable_name" {
			return
		}
		// $client = new Client(...) — the created type.
		if right.Type() == "object_creation_expression" {
			if name := firstNamedChild(right); name != nil {
				out[left.Content(src)] = bareTypeName(name.Content(src))
			}
		}
	})
	return out
}

func phpWalkCalls(root *sitter.Node, src []byte, emit clientCallEmitter) {
	walkNodes(root, func(n *sitter.Node) {
		if n.Type() != "member_call_expression" {
			return
		}
		obj := n.ChildByFieldName("object")
		name := n.ChildByFieldName("name")
		if obj == nil || name == nil {
			return
		}
		var args []*sitter.Node
		if argList := n.ChildByFieldName("arguments"); argList != nil {
			for i := 0; i < int(argList.NamedChildCount()); i++ {
				arg := argList.NamedChild(i)
				if arg == nil || arg.Type() != "argument" {
					continue
				}
				if inner := firstNamedChild(arg); inner != nil {
					args = append(args, inner)
				}
			}
		}
		emit(obj.Content(src), name.Content(src), args, nodeLine(n))
	})
}

// ---------------------------------------------------------------------------
// Java
// ---------------------------------------------------------------------------

func javaImports(root *sitter.Node, src []byte) []string {
	var out []string
	walkNodes(root, func(n *sitter.Node) {
		if n.Type() == "import_declaration" {
			out = append(out, strings.TrimSuffix(strings.TrimSpace(n.Content(src)), ";"))
		}
	})
	return out
}

func javaVarTypes(root *sitter.Node, src []byte) map[string]string {
	out := map[string]string{}
	walkNodes(root, func(n *sitter.Node) {
		switch n.Type() {
		case "local_variable_declaration", "field_declaration":
			typ := n.ChildByFieldName("type")
			if typ == nil {
				return
			}
			for i := 0; i < int(n.NamedChildCount()); i++ {
				d := n.NamedChild(i)
				if d == nil || d.Type() != "variable_declarator" {
					continue
				}
				if name := d.ChildByFieldName("name"); name != nil {
					out[name.Content(src)] = bareTypeName(typ.Content(src))
				}
			}
		case "formal_parameter":
			typ := n.ChildByFieldName("type")
			name := n.ChildByFieldName("name")
			if typ != nil && name != nil {
				out[name.Content(src)] = bareTypeName(typ.Content(src))
			}
		}
	})
	return out
}

func javaWalkCalls(root *sitter.Node, src []byte, emit clientCallEmitter) {
	walkNodes(root, func(n *sitter.Node) {
		if n.Type() != "method_invocation" {
			return
		}
		obj := n.ChildByFieldName("object")
		name := n.ChildByFieldName("name")
		if obj == nil || name == nil {
			return
		}
		args := positionalArgs(n.ChildByFieldName("arguments"), nil)
		emit(obj.Content(src), name.Content(src), args, nodeLine(n))
	})
}

// ---------------------------------------------------------------------------
// Kotlin
// ---------------------------------------------------------------------------

func kotlinImports(root *sitter.Node, src []byte) []string {
	var out []string
	walkNodes(root, func(n *sitter.Node) {
		if n.Type() == "import_header" {
			out = append(out, strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(n.Content(src)), "import")))
		}
	})
	return out
}

func kotlinVarTypes(root *sitter.Node, src []byte) map[string]string {
	out := map[string]string{}
	walkNodes(root, func(n *sitter.Node) {
		if n.Type() != "property_declaration" {
			return
		}
		// val client = OkHttpClient() — the constructed type is the call's
		// simple_identifier callee.
		var name string
		if vd := findChildOfType(n, "variable_declaration"); vd != nil {
			if id := findChildOfType(vd, "simple_identifier"); id != nil {
				name = id.Content(src)
			}
		}
		if name == "" {
			return
		}
		if call := findChildOfType(n, "call_expression"); call != nil {
			if callee := firstNamedChild(call); callee != nil && callee.Type() == "simple_identifier" {
				out[name] = bareTypeName(callee.Content(src))
			}
		}
	})
	return out
}

func kotlinWalkCalls(root *sitter.Node, src []byte, emit clientCallEmitter) {
	walkNodes(root, func(n *sitter.Node) {
		if n.Type() != "call_expression" {
			return
		}
		nav := firstNamedChild(n)
		if nav == nil || nav.Type() != "navigation_expression" {
			return
		}
		recv := firstNamedChild(nav)
		suffix := findChildOfType(nav, "navigation_suffix")
		if recv == nil || suffix == nil || recv.Type() != "simple_identifier" {
			return
		}
		methodNode := findChildOfType(suffix, "simple_identifier")
		if methodNode == nil {
			return
		}
		var args []*sitter.Node
		if cs := findChildOfType(n, "call_suffix"); cs != nil {
			if va := findChildOfType(cs, "value_arguments"); va != nil {
				for i := 0; i < int(va.NamedChildCount()); i++ {
					arg := va.NamedChild(i)
					if arg == nil || arg.Type() != "value_argument" {
						continue
					}
					if inner := firstNamedChild(arg); inner != nil {
						args = append(args, inner)
					}
				}
			}
		}
		emit(recv.Content(src), methodNode.Content(src), args, nodeLine(n))
	})
}

// ---------------------------------------------------------------------------
// Scala
// ---------------------------------------------------------------------------

func scalaImports(root *sitter.Node, src []byte) []string {
	var out []string
	walkNodes(root, func(n *sitter.Node) {
		if n.Type() == "import_declaration" {
			out = append(out, strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(n.Content(src)), "import")))
		}
	})
	return out
}

func scalaVarTypes(root *sitter.Node, src []byte) map[string]string {
	out := map[string]string{}
	walkNodes(root, func(n *sitter.Node) {
		if n.Type() != "val_definition" && n.Type() != "var_definition" {
			return
		}
		pattern := n.ChildByFieldName("pattern")
		value := n.ChildByFieldName("value")
		if pattern == nil || value == nil || pattern.Type() != "identifier" {
			return
		}
		// val client = basicRequest... — bind through the leading receiver of a
		// field/call chain so a builder value is recognised on the var too.
		out[pattern.Content(src)] = bareTypeName(value.Content(src))
	})
	return out
}

func scalaWalkCalls(root *sitter.Node, src []byte, emit clientCallEmitter) {
	walkNodes(root, func(n *sitter.Node) {
		if n.Type() != "call_expression" {
			return
		}
		fn := n.ChildByFieldName("function")
		if fn == nil || fn.Type() != "field_expression" {
			return
		}
		recv := fn.ChildByFieldName("value")
		field := fn.ChildByFieldName("field")
		if recv == nil || field == nil || recv.Type() != "identifier" {
			return
		}
		args := positionalArgs(n.ChildByFieldName("arguments"), nil)
		emit(recv.Content(src), field.Content(src), args, nodeLine(n))
	})
}

// findChildOfType returns the first named descendant of n with the given type,
// searching breadth-first one level at a time (shallow — direct named children
// first, then their children) to keep the search cheap and local.
func findChildOfType(n *sitter.Node, typ string) *sitter.Node {
	if n == nil {
		return nil
	}
	for i := 0; i < int(n.NamedChildCount()); i++ {
		ch := n.NamedChild(i)
		if ch == nil {
			continue
		}
		if ch.Type() == typ {
			return ch
		}
	}
	for i := 0; i < int(n.NamedChildCount()); i++ {
		if got := findChildOfType(n.NamedChild(i), typ); got != nil {
			return got
		}
	}
	return nil
}
