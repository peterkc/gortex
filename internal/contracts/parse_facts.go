package contracts

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"maps"
	"strconv"
	"strings"

	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/parser"
	sitter "github.com/zzet/gortex/internal/parser/tsitter"
)

// FileFacts is the checked, detached contract input from one accepted parse.
// Nil shape values explicitly record that a declaration has no native shape.
// LaravelHandlerClasses holds the detached class answer at each registration site.
// Router includes a superset of the native router census: an unresolved Go
// route question also retains file facts. C1b must filter these facts with
// contractRouterSupported on the resolved records before native prefix joining.
type FileFacts struct {
	Records               []Contract
	Questions             []EndpointContractQuestion
	Shapes                map[string]*Shape
	Handlers              map[string]HandlerFacts
	LaravelHandlerClasses map[string]string
	Router                *RouterFacts
	ConfigKeys            []*graph.Node
	ScanSkipped           bool
}

// ProviderHandlerHints selects unresolved HTTP providers for the native
// cross-file handler pass. Only PHP uses a class to narrow candidates.
func ProviderHandlerHints(c Contract) (trail, fallback, class string) {
	if c.Role != RoleProvider || c.Type != ContractHTTP {
		return "", "", ""
	}
	trail, _ = c.Meta["handler_trail"].(string)
	fallback, _ = c.Meta["handler_ident"].(string)
	if trail == "" && fallback == "" {
		return "", "", ""
	}
	// The initial file-scoped pass already populated the schema.
	if src, _ := c.Meta["schema_source"].(string); src == "extracted" || src == "partial" {
		return "", "", ""
	}
	if strings.HasSuffix(c.FilePath, ".php") {
		class, _ = c.Meta["handler_class"].(string)
	}
	return trail, fallback, class
}

// LaravelHandlerClassKey identifies a PHP class question from a prepared
// registration. Use the registration file's facts, not the winning handler's.
// Several resource routes can share the same class and source line.
func LaravelHandlerClassKey(c Contract) (key, class string) {
	_, _, class = ProviderHandlerHints(c)
	if class == "" {
		return "", ""
	}
	return strconv.Itoa(c.Line) + ":" + class, class
}

// ErrContractFactTree indicates that a required native detector would need
// another parse. A build worker must refuse rather than silently reparse.
var ErrContractFactTree = errors.New("required contract fact tree unavailable")

// CheckFileFactTree checks only the native detectors with parsing fallbacks.
// The caller may pass nil for detectors whose native prefilters prove no AST
// work, but never when the native extractor needs to inspect a tree.
func CheckFileFactTree(filePath string, src []byte, tree *parser.ParseTree, exts []Extractor) error {
	lang := detectLanguage(filePath)
	hasTree := tree != nil && tree.Tree() != nil
	if hasTree && !bytes.Equal(tree.Source(), src) {
		return fmt.Errorf("%w: %s: source differs", ErrContractFactTree, filePath)
	}
	if lang == "go" && hasTree && !NativeGoBodyFactsFactory() {
		return fmt.Errorf("%w: native Go body facts factory changed", ErrContractFactTree)
	}
	for _, ex := range exts {
		var required string
		switch ex.(type) {
		case *HTTPExtractor:
			if lang == "go" && len(src) != 0 || clientLibAdapters[lang] != nil && srcMentionsClientLib(src, httpClientLibraries[lang]) {
				required = lang
			}
		case *TopicExtractor:
			if lang == "go" && len(src) != 0 {
				required = lang
			}
		case *TerraformExtractor:
			if srcHasAnyMarker(src, terraformPrefilterMarkers) {
				required = "hcl"
			}
		case *GRPCExtractor:
			if needsTSGRPCTree(filePath, src) && !matchesTSGRPCTree(filePath, tree) {
				return fmt.Errorf("%w: %s: gRPC grammar differs or is unavailable", ErrContractFactTree, filePath)
			}
		}
		if required != "" && (!hasTree || tree.Lang() != required) {
			return fmt.Errorf("%w: %s: %s", ErrContractFactTree, filePath, required)
		}
	}
	return nil
}

// HandlerFacts holds separate regex writes and AST writes so replay preserves
// native overwrite, merge and deletion behavior. Binding lookups remain local.
type HandlerFacts struct {
	Regex          map[string]any
	AST            map[string]any
	Bindings       map[string]Binding
	ASTTypeLookups map[string]string // exact normalized local name to native answer
}

// CaptureFileHandlerFacts derives detached facts with the same first-line,
// then first-name selection as native MakeBodyFacts. Index the accepted tree
// once so eager capture does not scan the whole tree for each function.
func CaptureFileHandlerFacts(ctx context.Context, nodes []*graph.Node, lines []string, lang string, tree *parser.ParseTree) (map[string]HandlerFacts, error) {
	if lang == "go" && tree != nil && !NativeGoBodyFactsFactory() {
		return nil, fmt.Errorf("%w: native Go body facts factory changed", ErrContractFactTree)
	}
	byRow := make(map[int]*sitter.Node)
	byName := make(map[string]*sitter.Node)
	goTree := tree != nil && tree.Tree() != nil && tree.Lang() == "go"
	if goTree {
		var visit func(*sitter.Node) error
		visit = func(n *sitter.Node) error {
			if n == nil {
				return nil
			}
			if err := ctx.Err(); err != nil {
				return err
			}
			switch n.Type() {
			case "function_declaration", "method_declaration", "func_literal":
				if body := n.ChildByFieldName("body"); body != nil {
					row := int(n.StartPoint().Row)
					if byRow[row] == nil {
						byRow[row] = body
					}
					if name := n.ChildByFieldName("name"); name != nil && n.Type() != "func_literal" {
						key := name.Content(tree.Source())
						if byName[key] == nil {
							byName[key] = body
						}
					}
				}
			}
			for i := 0; i < int(n.NamedChildCount()); i++ {
				if err := visit(n.NamedChild(i)); err != nil {
					return err
				}
			}
			return nil
		}
		if err := visit(tree.Tree().RootNode()); err != nil {
			return nil, err
		}
	}
	out := make(map[string]HandlerFacts)
	firstByID := make(map[string]*graph.Node)
	for _, n := range nodes {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		// Native takes the first ID match, even if it isn't callable.
		first := firstByID[n.ID]
		if first == nil {
			firstByID[n.ID] = n
			first = n
		}
		if n.Kind != graph.KindFunction && n.Kind != graph.KindMethod {
			continue
		}
		var bf *goBodyFacts
		if goTree {
			body := byRow[n.StartLine-1]
			if body == nil && n.Name != "" {
				body = byName[n.Name]
			}
			bf = &goBodyFacts{tree: tree, src: tree.Source(), handler: n, body: body, bindings: map[string]Binding{}}
			bf.walk(body)
		}
		c := Contract{Role: RoleProvider, SymbolID: n.ID, FilePath: n.FilePath, Line: n.StartLine, Meta: map[string]any{}}
		start, end := handlerNodeBodyRange(first, c.Line, lines)
		enrichProviderBody(&c, lines, nodes, lang, start, end)
		f := HandlerFacts{Regex: c.Meta}
		if lang != "go" || bf != nil {
			f.AST = make(map[string]any)
		}
		if bf != nil {
			c.Meta = f.AST
			f.ASTTypeLookups = make(map[string]string)
			applyProviderFactsWithLookup(&c, bf, func(name string) string {
				answer := resolveTypeInFile(name, nodes)
				name, _ = typeLookupName(name)
				if name != "" {
					f.ASTTypeLookups[name] = answer
				}
				return answer
			})
			f.Bindings = maps.Clone(bf.bindings)
		}
		out[n.ID] = f
	}
	return out, nil
}

// EnrichProvider applies precisely the native provider regex/AST updates to
// the selected handler's incoming record; the caller retains registration site.
func (f HandlerFacts) EnrichProvider(c *Contract) {
	if c.Meta == nil {
		c.Meta = map[string]any{}
	}
	if path, _ := c.Meta["path"].(string); path != "" {
		if params := pathParamsFromTemplate(path); len(params) > 0 {
			c.Meta["path_params"] = params
		}
	}
	maps.Copy(c.Meta, cloneFactMeta(f.Regex))
	for k, v := range cloneFactMeta(f.AST) {
		switch k {
		case "status_codes":
			mergeIntStrings(c.Meta, k, v.([]int))
		case "query_params":
			mergeStringList(c.Meta, k, v.([]string))
		case "response_expr":
			if rt, _ := c.Meta["response_type"].(string); rt == "" {
				c.Meta[k] = v
			}
		default:
			c.Meta[k] = v
		}
	}
	if _, ok := f.AST["request_type"]; ok {
		delete(c.Meta, "request_expr")
	}
	if _, ok := f.AST["response_type"]; ok {
		delete(c.Meta, "response_expr")
	}
	if _, ok := f.AST["response_envelope"]; ok {
		delete(c.Meta, "response_expr")
	}
}

func (f HandlerFacts) VarBinding(name string) Binding {
	return f.Bindings[strings.TrimLeft(strings.TrimSpace(name), "&*")]
}

// SpringFileFacts uses the native value-redacted config-key extractor. It
// does not retain config values or read any other file.
func SpringFileFacts(filePath string, raw []byte) []*graph.Node {
	profile, ok := SpringConfigProfile(filePath)
	if !ok {
		return nil
	}
	return ExtractSpringConfigKeys(filePath, raw, profile)
}
