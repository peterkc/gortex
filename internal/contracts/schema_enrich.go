package contracts

import (
	"regexp"
	"sort"
	"strconv"
	"strings"

	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/parser"
)

// schemaHints collects the structured fields a single enricher extracts
// from a handler body. Each field is optional — a detector populates
// only what it can pin down from the text. The driver merges hints
// from every applicable enricher before writing to Contract.Meta.
type schemaHints struct {
	// RequestType is the symbol ID or bare type name of the request
	// body. A symbol ID is emitted when the type is defined in the
	// same file as the handler (cheap to resolve via the file-scoped
	// node list); otherwise the bare name is stored so a later
	// module-wide pass can upgrade it.
	RequestType string
	// RequestExpr is the raw source expression used to receive the
	// body when no type name could be pulled out (e.g.
	// `c.BindJSON(map[string]any{})`). Kept so the UI can at least
	// point at the binding call site.
	RequestExpr string

	ResponseType string
	ResponseExpr string
	// ResponseRepeated is true when the captured type was a list
	// shape (`Foo[]`, `Array<Foo>`, `[]Foo`), so the dashboard can
	// render the response as an array of Foo without losing the
	// underlying type.
	ResponseRepeated bool
	// ResponseEnvelope is the structured form of an inline map response
	// like `map[string]any{"files": out, "total": count}`. Each field
	// records the JSON key, the source expression that fed it, and
	// (when resolvable) the inferred type. The dashboard prefers this
	// over ResponseExpr when present so the schema view shows a real
	// shape instead of the raw response-helper call.
	ResponseEnvelope []envelopeField

	QueryParams []string
	StatusCodes []int
}

// envelopeField is one key in an inline JSON envelope literal. Type
// is best-effort — empty when the value couldn't be traced to a
// concrete declaration; Expr is always the trimmed source expression.
// Repeated is true when the value's declared type was a slice
// (`[]Foo` or `make([]Foo, …)`), so the dashboard can render the
// field as an array.
type envelopeField struct {
	Name     string
	Expr     string
	Type     string
	Repeated bool
}

func (h *schemaHints) isEmpty() bool {
	return h.RequestType == "" &&
		h.RequestExpr == "" &&
		h.ResponseType == "" &&
		h.ResponseExpr == "" &&
		len(h.ResponseEnvelope) == 0 &&
		len(h.QueryParams) == 0 &&
		len(h.StatusCodes) == 0
}

func (h *schemaHints) merge(o schemaHints) {
	if h.RequestType == "" {
		h.RequestType = o.RequestType
	}
	if h.RequestExpr == "" {
		h.RequestExpr = o.RequestExpr
	}
	if h.ResponseType == "" {
		h.ResponseType = o.ResponseType
		// ResponseRepeated travels with ResponseType: only adopt it
		// when we adopt the underlying type, so a later enricher
		// can't promote a non-list type to a list one (or vice
		// versa) in isolation.
		h.ResponseRepeated = o.ResponseRepeated
	}
	if h.ResponseExpr == "" {
		h.ResponseExpr = o.ResponseExpr
	}
	if len(h.ResponseEnvelope) == 0 {
		h.ResponseEnvelope = o.ResponseEnvelope
	}
	h.QueryParams = append(h.QueryParams, o.QueryParams...)
	h.StatusCodes = append(h.StatusCodes, o.StatusCodes...)
}

// schemaEnricher is a per-(language, role) body scanner. Each detector
// looks for binding / encoding / query / status patterns specific to
// its framework and returns the hints it found. Framework gating is
// intentionally absent — regex specificity already keeps a gin
// detector from firing on fiber code, and upstream httpPatterns
// sometimes mis-label the framework tag (e.g. a `r.POST(...)`
// declaration in a gin file gets tagged "fiber" because the uppercase
// pattern is shared). Role gating is explicit: provider-side decoders
// would otherwise match response-body decodes on the consumer side.
type schemaEnricher struct {
	name      string
	languages []string // empty = any
	roles     []Role   // empty = any
	detect    func(body string, fileNodes []*graph.Node) schemaHints
}

// schemaEnrichers is the registry of body scanners. Each language
// contributes its own detectors below; the driver iterates over them
// and merges results. Declaration-site order is not significant:
// hints merge additively so two enrichers can contribute different
// fields without stepping on each other.
var schemaEnrichers []schemaEnricher

// EnrichHTTPContract is the exported entry point for the schema
// enrichment pipeline. The per-file HTTPExtractor calls it during
// extraction; the indexer's cross-file handler-resolution post-pass
// calls it again when the first attempt ran against the wrong
// function body (router vs. actual handler).
func EnrichHTTPContract(c *Contract, lines []string, fileNodes []*graph.Node, lang string) {
	EnrichHTTPContractWithTree(c, lines, fileNodes, lang, nil)
}

// EnrichHTTPContractWithTree is the tree-aware variant: it runs the
// regex-based enricher first (so non-Go languages and patterns the
// AST doesn't recognise still produce meta), then overlays AST-derived
// facts when tree is non-nil and the language has a registered
// BodyFactsFactory. AST output wins on the keys it can confidently
// produce; regex output stays for the rest.
func EnrichHTTPContractWithTree(
	c *Contract,
	lines []string,
	fileNodes []*graph.Node,
	lang string,
	tree *parser.ParseTree,
) {
	enrichHTTPContract(c, lines, fileNodes, lang)
	if tree != nil {
		applyBodyFactsToHTTPContract(c, fileNodes, tree)
	}
}

// enrichHTTPContract extracts schema-shape hints from the handler body
// that owns `c` and folds them into c.Meta. It is a no-op when the
// contract is a consumer (there is no handler to scan), when the
// handler's body span can't be located, or when no enricher matched.
//
// Keys written to c.Meta on success:
//
//	path_params   []string   — always when the path template has placeholders
//	query_params  []string   — when any enricher found query reads
//	status_codes  []int      — when any enricher found status writes
//	request_type  string     — symbol ID or bare type name
//	request_expr  string     — raw expression fallback
//	response_type string     — symbol ID or bare type name
//	response_expr string     — raw expression fallback
//	schema_source string     — one of "extracted" | "partial" | "none"
func enrichHTTPContract(c *Contract, lines []string, fileNodes []*graph.Node, lang string) {
	if c.Meta == nil {
		c.Meta = map[string]any{}
	}

	// Path params can be read off the normalised path regardless of
	// whether we ever find the handler body.
	if path, _ := c.Meta["path"].(string); path != "" {
		if pps := pathParamsFromTemplate(path); len(pps) > 0 {
			c.Meta["path_params"] = pps
		}
	}

	if c.Role != RoleProvider {
		// Consumer-side schema extraction is handled by the consumer
		// enrichers below, which look at the call site not a handler
		// body. Keep the call short: find the call-site line and a
		// small window around it.
		enrichConsumerContract(c, lines, fileNodes, lang)
		return
	}

	start, end := handlerBodyRange(c, fileNodes, lines)
	enrichProviderBody(c, lines, fileNodes, lang, start, end)
}

func enrichProviderBody(c *Contract, lines []string, fileNodes []*graph.Node, lang string, start, end int) {
	if start <= 0 || end < start {
		c.Meta["schema_source"] = "none"
		return
	}
	body := strings.Join(lines[start-1:end], "\n")

	var merged schemaHints
	matched := false
	for _, e := range schemaEnrichers {
		if !containsStr(e.languages, lang) {
			continue
		}
		if !containsRole(e.roles, c.Role) {
			continue
		}
		h := e.detect(body, fileNodes)
		if h.isEmpty() {
			continue
		}
		matched = true
		merged.merge(h)
	}
	applyHints(c, merged, matched)
}

// enrichConsumerContract handles the consumer case — we scan a small
// window around the call site for payload arg types, decode targets,
// and any JSON-encode wrapper expressions.
func enrichConsumerContract(c *Contract, lines []string, fileNodes []*graph.Node, lang string) {
	// Prefer the enclosing function's body range when SymbolID
	// resolves to a known function in this file. The wide
	// callSiteWindow heuristic (±6/+14 lines) was designed for
	// top-level/script call sites where there's no enclosing
	// function — but it spans MULTIPLE neighbouring functions in
	// dense object literals (api.health/api.tools/api.stats stacked
	// 2-3 lines apart in api.ts), so the regex's first
	// `Promise<X>` match latches onto the WRONG endpoint's type.
	// The function-body window is tight, structurally correct, and
	// always preferable when available.
	start, end := consumerBodyRange(c, fileNodes, lines)
	if start <= 0 {
		c.Meta["schema_source"] = "none"
		return
	}
	body := strings.Join(lines[start-1:end], "\n")

	var merged schemaHints
	matched := false
	for _, e := range schemaEnrichers {
		if !containsStr(e.languages, lang) {
			continue
		}
		if !containsRole(e.roles, c.Role) {
			continue
		}
		h := e.detect(body, fileNodes)
		if h.isEmpty() {
			continue
		}
		matched = true
		merged.merge(h)
	}
	applyHints(c, merged, matched)
}

func containsRole(xs []Role, r Role) bool {
	if len(xs) == 0 {
		return true
	}
	for _, x := range xs {
		if x == r {
			return true
		}
	}
	return false
}

func applyHints(c *Contract, h schemaHints, matched bool) {
	if !matched {
		c.Meta["schema_source"] = "none"
		return
	}
	if h.RequestType != "" {
		c.Meta["request_type"] = h.RequestType
	}
	if h.RequestExpr != "" {
		c.Meta["request_expr"] = h.RequestExpr
	}
	if h.ResponseType != "" {
		c.Meta["response_type"] = h.ResponseType
		if h.ResponseRepeated {
			c.Meta["response_repeated"] = true
		}
	}
	if h.ResponseExpr != "" {
		c.Meta["response_expr"] = h.ResponseExpr
	}
	if len(h.ResponseEnvelope) > 0 {
		arr := make([]map[string]any, 0, len(h.ResponseEnvelope))
		for _, f := range h.ResponseEnvelope {
			row := map[string]any{"name": f.Name}
			if f.Type != "" {
				row["type"] = f.Type
			}
			if f.Expr != "" {
				row["expr"] = f.Expr
			}
			if f.Repeated {
				row["repeated"] = true
			}
			arr = append(arr, row)
		}
		c.Meta["response_envelope"] = arr
	}
	if qs := uniqStrings(h.QueryParams); len(qs) > 0 {
		c.Meta["query_params"] = qs
	}
	if ss := uniqInts(h.StatusCodes); len(ss) > 0 {
		c.Meta["status_codes"] = ss
	}
	// "extracted" means we have real type references on whichever side
	// of the wire is relevant for this role. Everything else is
	// partial — we saw something but couldn't pin a type.
	haveTypes := false
	if c.Role == RoleProvider {
		haveTypes = h.RequestType != "" || h.ResponseType != ""
	} else {
		haveTypes = h.RequestType != "" || h.ResponseType != ""
	}
	if haveTypes {
		c.Meta["schema_source"] = "extracted"
	} else {
		c.Meta["schema_source"] = "partial"
	}
}

// handlerBodyRange returns the 1-based [start, end] line range covering
// the handler's body in the file. Returns (0, 0) when no sensible span
// can be located. For parsers that record end_line accurately (Go,
// Python) we trust it; otherwise we brace-balance up to a 200-line
// safety window starting from the symbol's start_line.
func handlerBodyRange(c *Contract, fileNodes []*graph.Node, lines []string) (int, int) {
	for _, n := range fileNodes {
		if n.ID == c.SymbolID {
			return handlerNodeBodyRange(n, c.Line, lines)
		}
	}
	// Fall back to a window around the declaration line.
	return braceWindow(lines, c.Line)
}

func handlerNodeBodyRange(node *graph.Node, line int, lines []string) (int, int) {
	start := node.StartLine
	end := node.EndLine
	if start <= 0 {
		start = line
	}
	if end > start {
		return start, clampLine(end, len(lines))
	}
	return braceWindow(lines, start)
}

// braceWindow is the fallback for parsers that only record start_line:
// walk from `from` forward up to 200 lines, counting braces, and stop
// when the first opening `{` on or after `from` closes. Useful for
// Dart and (historically) TypeScript where the extractor pins only the
// declaration line.
func braceWindow(lines []string, from int) (int, int) {
	if from <= 0 || from > len(lines) {
		return 0, 0
	}
	depth := 0
	opened := false
	end := from
	limit := from + 200
	if limit > len(lines) {
		limit = len(lines)
	}
	for i := from - 1; i < limit; i++ {
		line := lines[i]
		for _, ch := range line {
			if ch == '{' {
				depth++
				opened = true
			} else if ch == '}' && opened {
				depth--
			}
		}
		end = i + 1
		if opened && depth <= 0 {
			return from, end
		}
	}
	return from, end
}

// consumerBodyRange returns the best body window for a consumer
// contract. Order of preference:
//  1. The enclosing function node when SymbolID resolves — gives a
//     tight, correct window even in dense object-literal API
//     declarations where neighbouring methods sit 2-3 lines apart.
//  2. The wide ±6/+14 callSiteWindow heuristic, used when SymbolID
//     doesn't resolve to a fileNode (top-level scripts, inline
//     module code).
func consumerBodyRange(c *Contract, fileNodes []*graph.Node, lines []string) (int, int) {
	if c.SymbolID != "" {
		for _, n := range fileNodes {
			if n.ID != c.SymbolID {
				continue
			}
			// Only trust the function body window when the call line
			// falls inside the function's range. Synthetic / stale
			// SymbolIDs (legacy tests, contracts whose handler was
			// re-resolved post-extract) point at the wrong function;
			// in that case fall back to the wide call-site window.
			if n.StartLine > 0 && n.EndLine >= n.StartLine &&
				c.Line >= n.StartLine && c.Line <= n.EndLine {
				return n.StartLine, clampLine(n.EndLine, len(lines))
			}
			break
		}
	}
	return callSiteWindow(c, lines)
}

// callSiteWindow picks a small window of source around a consumer
// call site — enough to catch `jsonEncode(payload)` or `Decode(&resp)`
// patterns that sit in adjacent lines.
func callSiteWindow(c *Contract, lines []string) (int, int) {
	if c.Line <= 0 || c.Line > len(lines) {
		return 0, 0
	}
	start := c.Line - 6
	if start < 1 {
		start = 1
	}
	end := c.Line + 14
	if end > len(lines) {
		end = len(lines)
	}
	return start, end
}

func clampLine(n, max int) int {
	if n < 1 {
		return 1
	}
	if n > max {
		return max
	}
	return n
}

// pathParamRe picks canonical {name} placeholders out of a normalised
// path. Other placeholder syntaxes are already collapsed to this form
// by NormalizeHTTPPath, so this is the only pattern we need here.
var pathParamRe = regexp.MustCompile(`\{([^}]+)\}`)

func pathParamsFromTemplate(path string) []string {
	ms := pathParamRe.FindAllStringSubmatch(path, -1)
	if len(ms) == 0 {
		return nil
	}
	out := make([]string, 0, len(ms))
	for _, m := range ms {
		out = append(out, m[1])
	}
	return uniqStrings(out)
}

// -----------------------------------------------------------------------------
// Shared helpers for enricher implementations
// -----------------------------------------------------------------------------

// resolveTypeInFile upgrades a bare type name to its in-file symbol ID
// when a matching type node is present. Returns the original name
// unchanged when nothing matches — a later module-wide pass can
// upgrade it using the import graph.
func resolveTypeInFile(name string, fileNodes []*graph.Node) string {
	name = strings.TrimSpace(strings.TrimPrefix(name, "*"))
	if name == "" {
		return ""
	}
	// Drop package qualifier for in-file lookup: "common.Foo" -> "Foo".
	if idx := strings.LastIndex(name, "."); idx >= 0 {
		name = name[idx+1:]
	}
	// Also accept KindInterface — TypeScript / Java / Kotlin model
	// type definitions as `interface Foo {}`, which gets emitted as
	// KindInterface, not KindType. Limiting to KindType silently
	// drops every TS interface response_type lookup.
	for _, n := range fileNodes {
		if n.Name != name {
			continue
		}
		if n.Kind == graph.KindType || n.Kind == graph.KindInterface {
			return n.ID
		}
	}
	return name // bare name; upgraded later
}

func uniqStrings(in []string) []string {
	if len(in) == 0 {
		return nil
	}
	seen := make(map[string]struct{}, len(in))
	out := make([]string, 0, len(in))
	for _, s := range in {
		if s == "" {
			continue
		}
		if _, ok := seen[s]; ok {
			continue
		}
		seen[s] = struct{}{}
		out = append(out, s)
	}
	sort.Strings(out)
	return out
}

func uniqInts(in []int) []int {
	if len(in) == 0 {
		return nil
	}
	seen := make(map[int]struct{}, len(in))
	out := make([]int, 0, len(in))
	for _, v := range in {
		if _, ok := seen[v]; ok {
			continue
		}
		seen[v] = struct{}{}
		out = append(out, v)
	}
	sort.Ints(out)
	return out
}

func containsStr(xs []string, needle string) bool {
	if len(xs) == 0 {
		return true // empty = match any
	}
	for _, x := range xs {
		if x == needle {
			return true
		}
	}
	return false
}

// -----------------------------------------------------------------------------
// Status-code name → int table for Go's net/http constants. Any
// framework that uses the same constants (most Go ones do) gets this
// for free.
// -----------------------------------------------------------------------------

var httpStatusNames = map[string]int{
	"StatusContinue":            100,
	"StatusSwitchingProtocols":  101,
	"StatusOK":                  200,
	"StatusCreated":             201,
	"StatusAccepted":            202,
	"StatusNoContent":           204,
	"StatusMovedPermanently":    301,
	"StatusFound":               302,
	"StatusSeeOther":            303,
	"StatusNotModified":         304,
	"StatusTemporaryRedirect":   307,
	"StatusPermanentRedirect":   308,
	"StatusBadRequest":          400,
	"StatusUnauthorized":        401,
	"StatusForbidden":           403,
	"StatusNotFound":            404,
	"StatusMethodNotAllowed":    405,
	"StatusConflict":            409,
	"StatusGone":                410,
	"StatusPreconditionFailed":  412,
	"StatusUnprocessableEntity": 422,
	"StatusTooManyRequests":     429,
	"StatusInternalServerError": 500,
	"StatusNotImplemented":      501,
	"StatusBadGateway":          502,
	"StatusServiceUnavailable":  503,
	"StatusGatewayTimeout":      504,
}

// parseStatusExpr turns a status expression like "http.StatusOK" or
// "200" into an int, ignoring anything it doesn't recognise.
func parseStatusExpr(expr string) (int, bool) {
	expr = strings.TrimSpace(expr)
	if n, err := strconv.Atoi(expr); err == nil {
		return n, true
	}
	if strings.HasPrefix(expr, "http.") {
		name := strings.TrimPrefix(expr, "http.")
		if v, ok := httpStatusNames[name]; ok {
			return v, true
		}
	}
	// Bare constant: "StatusOK"
	if v, ok := httpStatusNames[expr]; ok {
		return v, true
	}
	return 0, false
}
