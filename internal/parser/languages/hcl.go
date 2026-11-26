package languages

import (
	"path"
	"strings"

	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/parser"
	sitter "github.com/zzet/gortex/internal/parser/tsitter"
	"github.com/zzet/gortex/internal/parser/tsitter/hcl"
)

// HCLExtractor extracts HCL/Terraform files into graph nodes and edges.
// Top-level blocks (resource, data, module, variable, output, provider,
// locals, terraform, …) become KindType nodes; the block kind rides on
// Meta["block_type"] and its Terraform reference address on
// Meta["tf_address"]. Each `locals` declaration additionally yields a
// KindConstant node addressed `local.<key>`. Cross-block value
// expressions (var.x, local.y, module.m, data.t.n, aws_instance.web.id)
// produce EdgeReferences edges so a change-impact walk can answer "what
// breaks if this resource/variable changes?". Config values a config
// declares — variable/output names, and the keys an allow-listed
// resource type declares (see tfConfigKeySites) — additionally yield
// KindConfigKey nodes on the shared cfg::env::<NAME> ID plus an
// EdgeUsesEnv edge from the declaring block. Block node IDs are scoped
// to the file's directory — the Terraform module boundary — so a
// reference in one .tf file resolves to a block defined in a sibling .tf
// file of the same module by exact ID match.
type HCLExtractor struct {
	retainTree bool
	lang       *sitter.Language
}

func NewHCLExtractor() *HCLExtractor {
	return &HCLExtractor{lang: hcl.GetLanguage()}
}

func (e *HCLExtractor) Language() string     { return "hcl" }
func (e *HCLExtractor) Extensions() []string { return []string{".tf", ".tfvars", ".hcl"} }

// WithRetainedTree requests the contract capture tree on an extraction-scoped copy.
func (e *HCLExtractor) WithRetainedTree() parser.Extractor {
	copy := *e
	copy.retainTree = true
	return &copy
}

func (e *HCLExtractor) Extract(filePath string, src []byte) (*parser.ExtractionResult, error) {
	tree, err := parser.ParseFile(src, e.lang)
	if err != nil {
		return nil, err
	}
	keepTree := false
	defer func() {
		if !keepTree {
			tree.Close()
		}
	}()

	root := tree.RootNode()
	result := &parser.ExtractionResult{}

	fileNode := &graph.Node{
		ID: filePath, Kind: graph.KindFile, Name: filePath,
		FilePath: filePath, StartLine: 1, EndLine: int(root.EndPoint().Row) + 1,
		Language: "hcl",
	}
	result.Nodes = append(result.Nodes, fileNode)

	dir := hclModuleDir(filePath)
	seen := make(map[string]bool)    // block-name dedup, per file
	refSeen := make(map[string]bool) // (from\x00to) reference dedup
	e.walkTopLevel(root, src, filePath, dir, fileNode.ID, result, seen, refSeen)

	if e.retainTree {
		result.ContractTree = parser.NewParseTree(tree, src, "hcl")
		keepTree = true
	}
	return result, nil
}

// hclModuleDir returns the directory holding the file — the Terraform
// module boundary. Every .tf file in a directory shares one address
// space, so block node IDs are scoped to the directory
// (hcl::<dir>::<address>); a reference in one file then resolves to a
// block defined in a sibling file of the same module by exact ID match.
func hclModuleDir(filePath string) string {
	d := path.Dir(filePath)
	if d == "" {
		return "."
	}
	return d
}

func hclNodeID(dir, address string) string { return "hcl::" + dir + "::" + address }

// walkTopLevel descends config_file / body wrappers and dispatches every
// TOP-LEVEL block (one not nested inside another block) to extractBlock.
// Nested blocks (ingress, lifecycle, dynamic, …) are not separate
// definition nodes — their value expressions are attributed to the
// enclosing top-level block as references.
func (e *HCLExtractor) walkTopLevel(node *sitter.Node, src []byte, filePath, dir, fileID string, result *parser.ExtractionResult, seen, refSeen map[string]bool) {
	if node == nil {
		return
	}
	for i, _nc := 0, int(node.ChildCount()); i < _nc; i++ {
		child := node.Child(i)
		if child == nil {
			continue
		}
		switch child.Type() {
		case "block":
			e.extractBlock(child, src, filePath, dir, fileID, result, seen, refSeen)
		case "config_file", "body":
			e.walkTopLevel(child, src, filePath, dir, fileID, result, seen, refSeen)
		}
	}
}

func (e *HCLExtractor) extractBlock(node *sitter.Node, src []byte, filePath, dir, fileID string, result *parser.ExtractionResult, seen, refSeen map[string]bool) {
	// A block is: identifier (block type), string_lit labels, then body.
	// E.g. resource "aws_instance" "web" { ... }
	var blockType string
	var labels []string
	var body *sitter.Node
	for i, _nc := 0, int(node.ChildCount()); i < _nc; i++ {
		child := node.Child(i)
		if child == nil {
			continue
		}
		switch child.Type() {
		case "identifier":
			if blockType == "" {
				blockType = child.Content(src)
			}
		case "string_lit":
			if text := trimQuotes(child.Content(src)); text != "" {
				labels = append(labels, text)
			}
		case "body":
			body = child
		}
	}
	if blockType == "" {
		return
	}

	// Name keeps the block-type prefix (resource.aws_instance.web) for
	// human display; tf_address is the reference form other blocks use.
	name := blockType
	for _, l := range labels {
		name += "." + l
	}
	address := hclBlockAddress(blockType, labels)
	id := hclNodeID(dir, address)
	startLine := int(node.StartPoint().Row) + 1

	if !seen[name] {
		seen[name] = true
		result.Nodes = append(result.Nodes, &graph.Node{
			ID: id, Kind: graph.KindType, Name: name,
			FilePath: filePath, StartLine: startLine, EndLine: int(node.EndPoint().Row) + 1,
			Language: "hcl",
			Meta: map[string]any{
				"block_type":  blockType,
				"type_flavor": blockType,
				"labels":      labels,
				"tf_address":  address,
			},
		})
		result.Edges = append(result.Edges, &graph.Edge{
			From: fileID, To: id, Kind: graph.EdgeDefines,
			FilePath: filePath, Line: startLine,
		})
	}

	e.extractConfigKeys(blockType, labels, body, src, filePath, id, startLine, result)

	// A locals block declares N independently-addressable values
	// (local.<key>); emit one KindConstant per key and resolve each
	// value's references from that key's node.
	if blockType == "locals" && body != nil {
		e.extractLocals(body, src, filePath, dir, fileID, result, refSeen)
		return
	}

	// Cross-block references: var.x, local.y, module.m, data.t.n, and
	// <resource_type>.<name> traversals anywhere in the block body.
	if body != nil {
		e.collectReferences(body, src, filePath, dir, id, result, refSeen)
	}
}

// extractLocals emits a KindConstant node per declaration in a `locals`
// block (addressed local.<key>) and links each to the blocks its value
// expression references.
func (e *HCLExtractor) extractLocals(body *sitter.Node, src []byte, filePath, dir, fileID string, result *parser.ExtractionResult, refSeen map[string]bool) {
	for i, _nc := 0, int(body.ChildCount()); i < _nc; i++ {
		attr := body.Child(i)
		if attr == nil || attr.Type() != "attribute" {
			continue
		}
		key, expr := hclAttrKeyExpr(attr, src)
		if key == "" {
			continue
		}
		address := "local." + key
		id := hclNodeID(dir, address)
		line := int(attr.StartPoint().Row) + 1
		result.Nodes = append(result.Nodes, &graph.Node{
			ID: id, Kind: graph.KindConstant, Name: address,
			FilePath: filePath, StartLine: line, EndLine: int(attr.EndPoint().Row) + 1,
			Language: "hcl",
			Meta:     map[string]any{"block_type": "local", "type_flavor": "local", "tf_address": address},
		})
		result.Edges = append(result.Edges, &graph.Edge{
			From: fileID, To: id, Kind: graph.EdgeDefines,
			FilePath: filePath, Line: line,
		})
		if expr != nil {
			e.collectReferences(expr, src, filePath, dir, id, result, refSeen)
		}
	}
}

// tfConfigKeySite names, for one allowlisted Terraform resource type,
// where that type's declared config values live: an attribute holding an
// object literal, optionally inside a single nested block.
type tfConfigKeySite struct {
	block  string // enclosing nested block, "" when attr sits in the resource body
	attr   string // attribute whose object literal declares the keys
	source string // Meta["source"] tag for the emitted config-key nodes
}

// tfConfigKeySites is the v1 allowlist of resource types whose declared
// config values become KindConfigKey nodes. It is deliberately an
// allowlist of exact attribute paths rather than a general nested-block
// traversal — every entry here is a shape where the declared key is
// known to name a config value, which is not true of Terraform block
// attributes in general.
var tfConfigKeySites = map[string]tfConfigKeySite{
	"aws_lambda_function":   {block: "environment", attr: "variables", source: "env"},
	"kubernetes_config_map": {attr: "data", source: "k8s_cm"},
	"kubernetes_secret":     {attr: "data", source: "k8s_secret"},
}

// extractConfigKeys emits the KindConfigKey nodes a block declares:
// the name of a variable/output block, and each key of the object
// literal an allowlisted resource type declares its config values in.
// Every key lands on the shared cfg::env::<NAME> ID so a code-side
// os.Getenv read resolves to the same node.
func (e *HCLExtractor) extractConfigKeys(blockType string, labels []string, body *sitter.Node, src []byte, filePath, blockID string, startLine int, result *parser.ExtractionResult) {
	if len(labels) == 0 {
		return
	}
	if blockType == "variable" || blockType == "output" {
		emitHCLConfigKey(result, blockID, labels[0], "tf_"+blockType, filePath, startLine)
		return
	}
	if blockType != "resource" {
		return
	}
	site, ok := tfConfigKeySites[labels[0]]
	if !ok || body == nil {
		return
	}
	scope := body
	if site.block != "" {
		if scope = hclNestedBlockBody(body, site.block, src); scope == nil {
			return
		}
	}
	obj := hclAttrObject(scope, site.attr, src)
	if obj == nil {
		return
	}
	for i, _nc := 0, int(obj.ChildCount()); i < _nc; i++ {
		elem := obj.Child(i)
		if elem == nil || elem.Type() != "object_elem" {
			continue
		}
		key := hclObjectElemKey(elem, src)
		if key == "" {
			continue
		}
		emitHCLConfigKey(result, blockID, key, site.source, filePath, int(elem.StartPoint().Row)+1)
	}
}

// emitHCLConfigKey materialises one KindConfigKey node plus the
// EdgeUsesEnv edge from the declaring block, mirroring the Kubernetes
// and Dockerfile extractors. Not deduped, matching dockerfile.go's own
// config-key extractor: AddNode dedupes by ID and edges dedupe by
// {From,To,Kind,FilePath,Line} once committed to the graph, so a second
// identical emission here is a harmless no-op downstream.
func emitHCLConfigKey(result *parser.ExtractionResult, fromID, name, source, filePath string, line int) {
	if name == "" {
		return
	}
	keyID := configKeyEnvID(name)
	result.Nodes = append(result.Nodes, &graph.Node{
		ID: keyID, Kind: graph.KindConfigKey, Name: name,
		FilePath: filePath, StartLine: line, EndLine: line,
		Language: "hcl",
		Meta: map[string]any{
			"source": source,
			"origin": "terraform",
		},
	})
	result.Edges = append(result.Edges, &graph.Edge{
		From: fromID, To: keyID, Kind: graph.EdgeUsesEnv,
		FilePath: filePath, Line: line,
		Meta: map[string]any{"scope": "runtime"},
	})
}

// hclAttrKeyExpr splits an "attribute" node (`key = expr`) into its
// identifier key and expression value. Shared by extractLocals (which
// wants every attribute) and hclAttrObject (which wants one by name).
func hclAttrKeyExpr(attr *sitter.Node, src []byte) (string, *sitter.Node) {
	var key string
	if id := findChildByType(attr, "identifier"); id != nil {
		key = id.Content(src)
	}
	return key, findChildByType(attr, "expression")
}

// hclNestedBlockBody returns the body of the first nested block named
// `name` inside body, or nil when absent. Scoped to one named block on
// purpose — this is not a general nested-block traversal.
func hclNestedBlockBody(body *sitter.Node, name string, src []byte) *sitter.Node {
	for i, _nc := 0, int(body.ChildCount()); i < _nc; i++ {
		child := body.Child(i)
		if child == nil || child.Type() != "block" {
			continue
		}
		id := findChildByType(child, "identifier")
		if id == nil || id.Content(src) != name {
			continue
		}
		return findChildByType(child, "body")
	}
	return nil
}

// hclAttrObject returns the object node of `<name> = { … }` inside body,
// or nil when the attribute is absent or its value isn't a literal
// object. A map built by merge(…), a for-expression, or a reference to a
// local has no statically enumerable keys, so those are skipped rather
// than guessed at.
func hclAttrObject(body *sitter.Node, name string, src []byte) *sitter.Node {
	for i, _nc := 0, int(body.ChildCount()); i < _nc; i++ {
		attr := body.Child(i)
		if attr == nil || attr.Type() != "attribute" {
			continue
		}
		key, expr := hclAttrKeyExpr(attr, src)
		if key != name || expr == nil {
			continue
		}
		return hclObjectOf(expr)
	}
	return nil
}

// hclObjectOf unwraps expression → collection_value → object. Only
// direct children are inspected, so an object nested inside a function
// call or for-expression is not mistaken for the attribute's own value.
func hclObjectOf(expr *sitter.Node) *sitter.Node {
	coll := findChildByType(expr, "collection_value")
	if coll == nil {
		return nil
	}
	return findChildByType(coll, "object")
}

// hclObjectElemKey returns the literal key of an object_elem — the bare
// form (KEY = "v") or the quoted form ("KEY" = "v"). A computed key
// ((var.x) = "v") has no static name and yields "".
func hclObjectElemKey(elem *sitter.Node, src []byte) string {
	expr := findChildByType(elem, "expression")
	if expr == nil {
		return ""
	}
	// The first expression child is the key; the second is the value.
	for j, _jc := 0, int(expr.ChildCount()); j < _jc; j++ {
		c := expr.Child(j)
		if c == nil {
			continue
		}
		switch c.Type() {
		case "variable_expr":
			return hclIdentText(c, src)
		case "literal_value":
			return trimQuotes(strings.TrimSpace(c.Content(src)))
		}
	}
	return ""
}

// collectReferences walks an expression subtree and emits an
// EdgeReferences from fromID to every block/variable/local/module/data
// address it traverses. A traversal is a variable_expr (the head
// identifier) immediately followed by get_attr children within the same
// parent — var.region, aws_instance.web.id, data.aws_ami.ubuntu.id,
// module.vpc.subnet_ids. The recursion reaches traversals nested inside
// templates ("web-${var.region}"), objects, function-call args, and
// for-expressions.
func (e *HCLExtractor) collectReferences(node *sitter.Node, src []byte, filePath, dir, fromID string, result *parser.ExtractionResult, refSeen map[string]bool) {
	if node == nil {
		return
	}
	cc := int(node.ChildCount())
	for i := 0; i < cc; i++ {
		child := node.Child(i)
		if child == nil || child.Type() != "variable_expr" {
			continue
		}
		head := hclIdentText(child, src)
		if head == "" {
			continue
		}
		var attrs []string
		for j := i + 1; j < cc; j++ {
			sib := node.Child(j)
			if sib == nil || sib.Type() != "get_attr" {
				break // stop the chain at the first index ([0]) or operator
			}
			if a := hclGetAttrName(sib, src); a != "" {
				attrs = append(attrs, a)
			}
		}
		addr := hclRefAddress(head, attrs)
		if addr == "" {
			continue
		}
		to := hclNodeID(dir, addr)
		if to == fromID {
			continue
		}
		key := fromID + "\x00" + to
		if refSeen[key] {
			continue
		}
		refSeen[key] = true
		result.Edges = append(result.Edges, &graph.Edge{
			From: fromID, To: to, Kind: graph.EdgeReferences,
			FilePath: filePath, Line: int(child.StartPoint().Row) + 1,
			Origin: graph.OriginASTResolved,
		})
	}
	for i := 0; i < cc; i++ {
		e.collectReferences(node.Child(i), src, filePath, dir, fromID, result, refSeen)
	}
}

// hclBlockAddress returns the Terraform reference address for a block —
// the form other blocks use to refer to it: resource → <type>.<name>
// (no leading "resource."), data → data.<type>.<name>, variable →
// var.<name>, module/output/provider → <type>.<name>; everything else
// (locals, terraform, moved, import, check, …) is addressed by its type
// plus any labels.
func hclBlockAddress(blockType string, labels []string) string {
	switch blockType {
	case "resource":
		if len(labels) >= 2 {
			return labels[0] + "." + labels[1]
		}
	case "data":
		if len(labels) >= 2 {
			return "data." + labels[0] + "." + labels[1]
		}
	case "variable":
		if len(labels) >= 1 {
			return "var." + labels[0]
		}
	case "module", "output", "provider":
		if len(labels) >= 1 {
			return blockType + "." + labels[0]
		}
	}
	addr := blockType
	for _, l := range labels {
		addr += "." + l
	}
	return addr
}

// hclRefAddress maps a parsed traversal (head identifier + get_attr chain)
// to the Terraform address of the block it refers to, or "" when the head
// is a built-in scope (each/count/self/path/terraform) or the traversal
// is too short to name a block.
func hclRefAddress(head string, attrs []string) string {
	switch head {
	case "each", "count", "self", "path", "terraform":
		return ""
	case "var":
		if len(attrs) >= 1 {
			return "var." + attrs[0]
		}
	case "local":
		if len(attrs) >= 1 {
			return "local." + attrs[0]
		}
	case "module":
		if len(attrs) >= 1 {
			return "module." + attrs[0]
		}
	case "data":
		if len(attrs) >= 2 {
			return "data." + attrs[0] + "." + attrs[1]
		}
	default:
		if len(attrs) >= 1 {
			// Resource reference: <type>.<name>[.attr…] → <type>.<name>.
			return head + "." + attrs[0]
		}
	}
	return ""
}

// hclIdentText returns the identifier text of a variable_expr node.
func hclIdentText(varExpr *sitter.Node, src []byte) string {
	for i, _nc := 0, int(varExpr.ChildCount()); i < _nc; i++ {
		c := varExpr.Child(i)
		if c != nil && c.Type() == "identifier" {
			return c.Content(src)
		}
	}
	return strings.TrimSpace(varExpr.Content(src))
}

// hclGetAttrName returns the attribute name of a get_attr node (".id" → "id").
func hclGetAttrName(getAttr *sitter.Node, src []byte) string {
	for i, _nc := 0, int(getAttr.ChildCount()); i < _nc; i++ {
		c := getAttr.Child(i)
		if c != nil && c.Type() == "identifier" {
			return c.Content(src)
		}
	}
	return strings.TrimPrefix(strings.TrimSpace(getAttr.Content(src)), ".")
}

func trimQuotes(s string) string {
	if len(s) >= 2 && (s[0] == '"' || s[0] == '\'') {
		return s[1 : len(s)-1]
	}
	return s
}
