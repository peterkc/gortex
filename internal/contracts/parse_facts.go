package contracts

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"maps"
	"strings"

	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/parser"
	sitter "github.com/zzet/gortex/internal/parser/tsitter"
)

// FileFacts is the checked, detached contract input from one accepted parse.
// Nil shape values explicitly record that a declaration has no native shape.
// NeedsRawHandlerTree names the transformed Go exception without retaining raw bytes.
type FileFacts struct {
	Records []Contract
	Questions []EndpointContractQuestion
	Shapes map[string]*Shape
	Handlers map[string]HandlerFacts
	Router *RouterFacts
	ConfigKeys []*graph.Node
	NeedsRawHandlerTree bool
	ScanSkipped bool
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
			if needsTSGRPCTree(src) && !matchesTSGRPCTree(filePath, tree) {
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
	Regex map[string]any
	AST map[string]any
	Bindings map[string]Binding
}

// CaptureFileHandlerFacts derives detached facts with the same first-line,
// then first-name selection as native MakeBodyFacts. Index the accepted tree
// once so eager capture does not scan the whole tree for each function.
func CaptureFileHandlerFacts(ctx context.Context, nodes []*graph.Node, lines []string, lang string, tree *parser.ParseTree) (map[string]HandlerFacts, error) {
	byRow := make(map[int]*sitter.Node)
	byName := make(map[string]*sitter.Node)
	if tree != nil && tree.Tree() != nil && tree.Lang() == "go" {
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
	for _, n := range nodes {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if n.Kind != graph.KindFunction && n.Kind != graph.KindMethod {
			continue
		}
		var bf *goBodyFacts
		if tree != nil && tree.Tree() != nil && tree.Lang() == "go" {
			body := byRow[n.StartLine-1]
			if body == nil && n.Name != "" {
				body = byName[n.Name]
			}
			bf = &goBodyFacts{tree: tree, src: tree.Source(), handler: n, body: body, bindings: map[string]Binding{}}
			bf.walk(body)
		}
		c := Contract{Role: RoleProvider, SymbolID: n.ID, FilePath: n.FilePath, Line: n.StartLine, Meta: map[string]any{}}
		enrichHTTPContract(&c, lines, nodes, lang)
		f := HandlerFacts{Regex: c.Meta, AST: map[string]any{}}
		if bf != nil {
			c.Meta = f.AST
			applyProviderFacts(&c, nodes, bf)
			f.Bindings = maps.Clone(bf.bindings)
		}
		out[n.ID] = f
	}
	return out, nil
}

// EnrichProvider applies precisely the native provider regex/AST updates to
// the selected handler's incoming record; the caller retains registration site.
func (f HandlerFacts) EnrichProvider(c *Contract) {
	if c.Meta == nil { c.Meta = map[string]any{} }
	if path, _ := c.Meta["path"].(string); path != "" {
		if params := pathParamsFromTemplate(path); len(params) > 0 {
			c.Meta["path_params"] = params
		}
	}
	maps.Copy(c.Meta, cloneFactMeta(f.Regex))
	for k, v := range cloneFactMeta(f.AST) {
		switch k {
		case "status_codes": mergeIntStrings(c.Meta, k, v.([]int))
		case "query_params": mergeStringList(c.Meta, k, v.([]string))
		case "response_expr":
			if rt, _ := c.Meta["response_type"].(string); rt == "" {
				c.Meta[k] = v
			}
		default: c.Meta[k] = v
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
	profile, ok := springConfigFile(filePath)
	if !ok {
		return nil
	}
	return ExtractSpringConfigKeys(filePath, raw, profile)
}
