package languages

import (
	"strings"

	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/parser"
	sitter "github.com/zzet/gortex/internal/parser/tsitter"
	"github.com/zzet/gortex/internal/parser/tsitter/html"
)

// HTMLExtractor extracts HTML files into graph nodes and edges. Inline
// <script> bodies are delegated to the JavaScript extractor so their
// functions / classes / calls land in the graph, and id-anchored elements
// become navigable DocSection (KindDoc) nodes.
type HTMLExtractor struct {
	lang *sitter.Language
	js   *JavaScriptExtractor
}

func NewHTMLExtractor() *HTMLExtractor {
	return &HTMLExtractor{lang: html.GetLanguage(), js: NewJavaScriptExtractor()}
}

func (e *HTMLExtractor) Language() string     { return "html" }
func (e *HTMLExtractor) Extensions() []string { return []string{".html", ".htm"} }

func (e *HTMLExtractor) Extract(filePath string, src []byte) (*parser.ExtractionResult, error) {
	return e.ExtractWithOptions(filePath, src, parser.ExtractionOptions{})
}

func (e *HTMLExtractor) ExtractWithOptions(filePath string, src []byte, opts parser.ExtractionOptions) (*parser.ExtractionResult, error) {
	tree, err := parser.ParseFile(src, e.lang)
	if err != nil {
		return nil, err
	}
	defer tree.Close()

	root := tree.RootNode()
	result := &parser.ExtractionResult{}

	fileNode := &graph.Node{
		ID: filePath, Kind: graph.KindFile, Name: filePath,
		FilePath: filePath, StartLine: 1, EndLine: int(root.EndPoint().Row) + 1,
		Language: "html",
	}
	result.Nodes = append(result.Nodes, fileNode)

	// Walk the AST manually since HTML tree-sitter queries can be quirky.
	e.walkNode(root, src, filePath, fileNode.ID, result, opts)

	return result, nil
}

func (e *HTMLExtractor) walkNode(node *sitter.Node, src []byte, filePath, fileID string, result *parser.ExtractionResult, opts parser.ExtractionOptions) {
	nodeType := node.Type()

	switch nodeType {
	case "script_element":
		e.extractScriptImport(node, src, filePath, fileID, result, opts)
	case "element":
		e.extractElement(node, src, filePath, fileID, result)
	}

	// Recurse into children.
	for i, _nc := 0, int(node.ChildCount()); i < _nc; i++ {
		child := node.Child(i)
		if child != nil {
			e.walkNode(child, src, filePath, fileID, result, opts)
		}
	}
}

// extractScriptImport handles a script_element: a `src` attribute is an
// external import; an inline body (no src) is delegated to the
// JavaScript extractor so its functions / classes / calls join the graph.
func (e *HTMLExtractor) extractScriptImport(node *sitter.Node, src []byte, filePath, fileID string, result *parser.ExtractionResult, opts parser.ExtractionOptions) {
	startTag := findChildByType(node, "start_tag")
	selfClosing := false
	if startTag == nil {
		// Self-closing script tag.
		startTag = findChildByType(node, "self_closing_tag")
		selfClosing = true
	}
	if startTag == nil {
		return
	}

	if srcAttr := findAttribute(startTag, "src", src); srcAttr != "" {
		result.Edges = append(result.Edges, &graph.Edge{
			From:     fileID,
			To:       "unresolved::import::" + srcAttr,
			Kind:     graph.EdgeImports,
			FilePath: filePath,
			Line:     int(node.StartPoint().Row) + 1,
		})
		return
	}
	if selfClosing {
		return
	}

	// Inline <script>: parse the body as JavaScript when the type marks
	// it as a script (the HTML default, or an explicit JS / module type).
	if !htmlScriptIsJS(findAttribute(startTag, "type", src)) {
		return
	}
	body := findChildByType(node, "raw_text")
	if body == nil {
		return
	}
	e.delegateInlineScript(body, src, filePath, fileID, result, opts)
}

// htmlScriptIsJS reports whether a <script type=...> value denotes
// JavaScript (so its body is worth parsing). An empty type is JavaScript
// by the HTML default; data blocks (application/json, text/template, …)
// are not.
func htmlScriptIsJS(scriptType string) bool {
	switch strings.ToLower(strings.TrimSpace(scriptType)) {
	case "", "module", "text/javascript", "application/javascript",
		"text/ecmascript", "application/ecmascript", "text/babel", "text/jsx":
		return true
	}
	return false
}

// delegateInlineScript parses an inline <script> body with the JavaScript
// extractor and folds the resulting symbols into the HTML file's graph.
// Node IDs are already scoped to a per-script virtual path
// (<file>#script:<line>), file-defines edges are re-pointed at the HTML
// file node, and every line number is shifted by the body's offset within
// the page so navigation lands on the real source line.
func (e *HTMLExtractor) delegateInlineScript(body *sitter.Node, src []byte, filePath, fileID string, result *parser.ExtractionResult, opts parser.ExtractionOptions) {
	lineOffset := int(body.StartPoint().Row)
	delegateInlineScriptSlice(e.js, []byte(body.Content(src)), lineOffset, filePath, fileID, "", result, opts)
}

// extractElement checks elements for link tags (stylesheet imports) and id attributes.
func (e *HTMLExtractor) extractElement(node *sitter.Node, src []byte, filePath, fileID string, result *parser.ExtractionResult) {
	startTag := findChildByType(node, "start_tag")
	if startTag == nil {
		startTag = findChildByType(node, "self_closing_tag")
	}
	if startTag == nil {
		return
	}

	tagName := findChildByType(startTag, "tag_name")
	if tagName == nil {
		return
	}
	tag := tagName.Content(src)

	// Link/stylesheet imports.
	if tag == "link" {
		href := findAttribute(startTag, "href", src)
		if href != "" {
			result.Edges = append(result.Edges, &graph.Edge{
				From:     fileID,
				To:       "unresolved::import::" + href,
				Kind:     graph.EdgeImports,
				FilePath: filePath,
				Line:     int(node.StartPoint().Row) + 1,
			})
		}
	}

	// Elements with id attributes become navigable DocSection anchors —
	// a deep-link target whose visible text is indexed for prose search.
	idVal := findAttribute(startTag, "id", src)
	if idVal != "" {
		id := filePath + "::doc:#" + idVal
		meta := map[string]any{
			"tag":         tag,
			"html_anchor": true,
		}
		if text := htmlElementText(node, src); text != "" {
			meta["section_text"] = text
		}
		result.Nodes = append(result.Nodes, &graph.Node{
			ID: id, Kind: graph.KindDoc, Name: "#" + idVal,
			FilePath: filePath, StartLine: int(node.StartPoint().Row) + 1, EndLine: int(node.EndPoint().Row) + 1,
			Language: "html", Meta: meta,
		})
		result.Edges = append(result.Edges, &graph.Edge{
			From: fileID, To: id, Kind: graph.EdgeDefines,
			FilePath: filePath, Line: int(node.StartPoint().Row) + 1,
		})
	}
}

// htmlElementText returns the collapsed visible text of an element
// subtree (its descendant "text" nodes), capped, for use as a
// DocSection's searchable body.
func htmlElementText(node *sitter.Node, src []byte) string {
	var b strings.Builder
	var walk func(n *sitter.Node)
	walk = func(n *sitter.Node) {
		if n == nil {
			return
		}
		if n.Type() == "text" {
			if b.Len() > 0 {
				b.WriteByte(' ')
			}
			b.WriteString(n.Content(src))
		}
		for i, _nc := 0, int(n.ChildCount()); i < _nc; i++ {
			walk(n.Child(i))
		}
	}
	walk(node)
	text := strings.Join(strings.Fields(b.String()), " ")
	const maxLen = 240
	if len(text) > maxLen {
		text = text[:maxLen]
	}
	return text
}

// findChildByType finds the first child node with the given type.
func findChildByType(node *sitter.Node, typeName string) *sitter.Node {
	for i, _nc := 0, int(node.ChildCount()); i < _nc; i++ {
		child := node.Child(i)
		if child != nil && child.Type() == typeName {
			return child
		}
	}
	return nil
}

// findAttribute looks for an attribute with the given name in a start_tag node
// and returns its unquoted value.
func findAttribute(startTag *sitter.Node, attrName string, src []byte) string {
	for i, _nc := 0, int(startTag.ChildCount()); i < _nc; i++ {
		child := startTag.Child(i)
		if child == nil || child.Type() != "attribute" {
			continue
		}
		nameNode := findChildByType(child, "attribute_name")
		if nameNode == nil || nameNode.Content(src) != attrName {
			continue
		}
		valNode := findChildByType(child, "quoted_attribute_value")
		if valNode == nil {
			continue
		}
		val := valNode.Content(src)
		val = strings.Trim(val, `"'`)
		return val
	}
	return ""
}
