package languages

import (
	"strings"

	"github.com/zzet/gortex/internal/graph"
	sitter "github.com/zzet/gortex/internal/parser/tsitter"
)

// jsBuiltinReceivers is a file-wide veto set, not a scope/type solver. A name
// gets positive evidence only from one supported binding visible at the call,
// or from a builtin spelling with no binding or write anywhere in the file.
// Unrecognized identifier roles can veto proof; they cannot supply it.
type jsBuiltinReceivers struct {
	language string
	src      []byte
	bindings map[string]jsReceiverBinding
	unsafe   bool
}

type jsReceiverBinding struct {
	scope *sitter.Node
	decl  *sitter.Node
	known graph.BuiltinReceiverHint
}

func collectJSBuiltinReceivers(root *sitter.Node, src []byte, language string) *jsBuiltinReceivers {
	r := &jsBuiltinReceivers{language: language, src: src, unsafe: root.HasError()}
	if r.unsafe {
		return r
	}
	r.bindings = make(map[string]jsReceiverBinding)
	walkNodes(root, func(n *sitter.Node) {
		switch n.Type() {
		case "with_statement":
			r.unsafe = true
		case "identifier", "type_identifier", "shorthand_property_identifier_pattern", "shorthand_property_identifier":
			name := n.Content(src)
			// Escapes can give another spelling to any binding. Direct eval
			// and with can change which binding is visible at a call site.
			if strings.ContainsRune(name, '\\') || name == "eval" {
				r.unsafe = true
			}
			p := n.Parent()
			if jsReceiverRead(n, p) {
				return
			}
			if p.Type() == "import_specifier" && p.ChildByFieldName("alias") != nil && n.Equal(p.ChildByFieldName("name")) {
				return // The imported name is not the local binding.
			}
			if _, exists := r.bindings[name]; exists {
				r.bindings[name] = jsReceiverBinding{}
				return
			}
			r.bindings[name] = jsReceiverBindingAt(n, p, src)
		}
	})
	return r
}

// These positions are reads, not bindings or assignment destinations. In
// particular x.m() does not veto x, but x.m = f and x[k]++ do.
func jsReceiverRead(n, p *sitter.Node) bool {
	for p != nil {
		switch p.Type() {
		case "parenthesized_expression":
			if p.ChildByFieldName("type") != nil {
				return false
			}
		case "member_expression":
			if !n.Equal(p.ChildByFieldName("object")) {
				return false
			}
		case "call_expression":
			return n.Equal(p.ChildByFieldName("function"))
		case "new_expression":
			return n.Equal(p.ChildByFieldName("constructor"))
		default:
			return false
		}
		n, p = p, p.Parent()
	}
	return false
}

func jsReceiverBindingAt(n, p *sitter.Node, src []byte) jsReceiverBinding {
	if n.Type() != "identifier" {
		return jsReceiverBinding{}
	}
	switch p.Type() {
	case "import_clause", "namespace_import", "import_specifier":
		original := "default"
		switch p.Type() {
		case "namespace_import":
			original = "*"
		case "import_specifier":
			if name := p.ChildByFieldName("name"); name != nil {
				original = name.Content(src)
			} else {
				return jsReceiverBinding{}
			}
		}
		for ancestor := p; ancestor != nil; ancestor = ancestor.Parent() {
			if ancestor.Type() == "import_statement" {
				if source := ancestor.ChildByFieldName("source"); source != nil && source.Type() == "string" {
					text := source.Content(src)
					if len(text) >= 2 {
						return jsReceiverBinding{scope: ancestor.Parent(), known: graph.BuiltinReceiverHint{
							Form: "import", Name: original, Module: text[1 : len(text)-1],
						}}
					}
				}
				break
			}
		}
	case "variable_declarator":
		if !n.Equal(p.ChildByFieldName("name")) {
			break
		}
		declaration := p.Parent()
		if declaration.Type() != "lexical_declaration" {
			break // var hoisting is not modeled.
		}
		scope := declaration.Parent()
		if scope.Type() == "export_statement" {
			scope = scope.Parent()
		}
		if scope.Type() == "program" || scope.Type() == "statement_block" {
			return jsReceiverBinding{scope: scope, decl: p}
		}
	case "required_parameter":
		if !n.Equal(p.ChildByFieldName("pattern")) && !n.Equal(p.ChildByFieldName("name")) {
			break
		}
		if params := p.Parent(); params.Type() == "formal_parameters" {
			if body := params.Parent().ChildByFieldName("body"); body != nil {
				return jsReceiverBinding{scope: body, decl: p}
			}
		}
	case "formal_parameters":
		// A unique untyped JS parameter gives no family and no contrary
		// evidence unless it shadows a builtin spelling.
		return jsReceiverBinding{scope: p.Parent().ChildByFieldName("body"), decl: p}
	}
	return jsReceiverBinding{}
}

func (b jsReceiverBinding) visibleAt(site uint32) bool {
	return b.scope != nil && site >= b.scope.StartByte() && site < b.scope.EndByte() &&
		(b.decl == nil || site >= b.decl.EndByte())
}

func (r *jsBuiltinReceivers) stamp(edge *graph.Edge, call *sitter.Node) {
	if r == nil || call == nil {
		return
	}
	fn := call.ChildByFieldName("function")
	if fn == nil || fn.Type() != "member_expression" {
		return
	}
	hint := graph.BuiltinReceiverHint{Form: "unknown"}
	if !r.unsafe {
		hint = r.receiver(fn.ChildByFieldName("object"), call.StartByte())
	}
	hint.Language = r.language
	edge.SetBuiltinReceiverHint(hint)
}

func (r *jsBuiltinReceivers) receiver(node *sitter.Node, site uint32) graph.BuiltinReceiverHint {
	n := jsUnwrapReceiver(node)
	if n == nil {
		return graph.BuiltinReceiverHint{Form: "unknown"}
	}
	switch n.Type() {
	case "identifier":
		name := n.Content(r.src)
		if b, exists := r.bindings[name]; exists {
			if !b.visibleAt(site) {
				return graph.BuiltinReceiverHint{Form: "shadowed"}
			}
			if b.decl == nil {
				return b.known
			}
			hint := r.bindingValue(b.decl)
			if hint.Form == "unknown" && jsStaticReceiverFamily(name) != "" {
				return graph.BuiltinReceiverHint{Form: "shadowed"}
			}
			return hint
		}
		if family := jsStaticReceiverFamily(name); family != "" {
			return graph.BuiltinReceiverHint{Form: "static", Family: family}
		}
	case "member_expression", "subscript_expression":
		// An imported namespace/member remains contrary evidence, but its
		// property name alone never creates a static owner or instance.
		hint := r.receiver(n.ChildByFieldName("object"), site)
		switch hint.Form {
		case "import", "shadowed", "user":
			return hint
		}
	default:
		return r.receiverValue(n)
	}
	return graph.BuiltinReceiverHint{Form: "unknown"}
}

func (r *jsBuiltinReceivers) bindingValue(decl *sitter.Node) graph.BuiltinReceiverHint {
	value := decl.ChildByFieldName("value")
	hint := r.receiverValue(value)
	if annotation := decl.ChildByFieldName("type"); annotation != nil {
		t := annotation.NamedChild(0)
		if t == nil {
			return graph.BuiltinReceiverHint{Form: "shadowed"}
		}
		var family string
		switch t.Type() {
		case "array_type":
			family = "array"
		case "predefined_type":
			if name := t.Content(r.src); name == "string" || name == "number" {
				family = name
			}
		case "type_identifier", "generic_type", "nested_type_identifier", "object_type":
			return graph.BuiltinReceiverHint{Form: "user", Name: t.Content(r.src)}
		}
		if family == "" || (value != nil && hint.Family != family) {
			return graph.BuiltinReceiverHint{Form: "shadowed"}
		}
		return graph.BuiltinReceiverHint{Form: "instance", Family: family}
	}
	if value != nil && hint.Form == "unknown" {
		return graph.BuiltinReceiverHint{Form: "shadowed"} // Unsupported initializer or alias.
	}
	return hint
}

func (r *jsBuiltinReceivers) receiverValue(node *sitter.Node) graph.BuiltinReceiverHint {
	n := jsUnwrapReceiver(node)
	if n == nil {
		return graph.BuiltinReceiverHint{Form: "unknown"}
	}
	var family string
	switch n.Type() {
	case "array":
		family = "array"
	case "string", "template_string":
		family = "string"
	case "number":
		if strings.HasSuffix(n.Content(r.src), "n") {
			return graph.BuiltinReceiverHint{Form: "user"} // bigint is not number.
		}
		family = "number"
	case "object":
		if n.NamedChildCount() != 0 {
			return graph.BuiltinReceiverHint{Form: "user"} // Own members and spreads are not checked.
		}
		family = "object"
	case "new_expression":
		ctor := n.ChildByFieldName("constructor")
		if ctor == nil {
			break
		}
		switch ctor.Type() {
		case "identifier":
			name := ctor.Content(r.src)
			if b, exists := r.bindings[name]; exists {
				if b.known.Form == "import" && b.visibleAt(n.StartByte()) {
					return b.known
				}
				return graph.BuiltinReceiverHint{Form: "shadowed"}
			}
			switch name {
			case "Array", "String", "Number", "Map", "Set", "Promise":
				family = jsStaticReceiverFamily(name)
			default:
				return graph.BuiltinReceiverHint{Form: "user", Name: name}
			}
		case "member_expression", "subscript_expression":
			return r.receiver(ctor, n.StartByte())
		}
	}
	if family == "" {
		return graph.BuiltinReceiverHint{Form: "unknown"}
	}
	return graph.BuiltinReceiverHint{Form: "instance", Family: family}
}

func jsUnwrapReceiver(n *sitter.Node) *sitter.Node {
	for n != nil && n.Type() == "parenthesized_expression" {
		if n.NamedChildCount() != 1 || n.ChildByFieldName("type") != nil {
			return nil
		}
		n = n.NamedChild(0)
	}
	return n
}

func jsStaticReceiverFamily(name string) string {
	switch name {
	case "Array", "String", "Number", "Map", "Set", "Promise", "Object", "JSON", "console":
		return strings.ToLower(name)
	default:
		return ""
	}
}
