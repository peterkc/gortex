package languages

import (
	"slices"
	"strings"

	"github.com/zzet/gortex/internal/graph"
	sitter "github.com/zzet/gortex/internal/parser/tsitter"
)

// jsBuiltinReceivers tracks lexical value bindings and file-wide write vetoes.
// A receiver gets positive evidence only from a supported visible binding or
// an unbound builtin spelling. Reads and type-only parameters never bind names.
type jsBuiltinReceivers struct {
	language string
	src      []byte
	bindings map[string][]jsReceiverBinding
	types    map[string]bool
	writes   map[jsReceiverWrite]bool
	unsafe   bool
}

// Empty form denotes a write to a local name; "global" replaces the family.
// Static and instance writes affect only their own member (empty = all).
type jsReceiverWrite struct{ form, owner, member string }

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
	var writes []*sitter.Node
	r.bindings = make(map[string][]jsReceiverBinding)
	r.types = make(map[string]bool)
	bind := func(n *sitter.Node) {
		name := n.Content(src)
		b := jsReceiverBindingAt(n, n.Parent(), src)
		if b.scope == nil {
			b.scope = jsReceiverBindingScope(n)
		}
		if b.scope == nil {
			return
		}
		bindings := r.bindings[name]
		for i, prior := range bindings {
			if prior.scope.Equal(b.scope) {
				bindings[i] = jsReceiverBinding{scope: b.scope, known: graph.BuiltinReceiverHint{Form: "shadowed"}}
				return
			}
		}
		r.bindings[name] = append(bindings, b)
	}
	walkNodes(root, func(n *sitter.Node) {
		switch n.Type() {
		case "with_statement":
			r.unsafe = true
		case "identifier", "type_identifier", "shorthand_property_identifier_pattern", "shorthand_property_identifier":
			name := n.Content(src)
			if strings.ContainsRune(name, '\\') || name == "eval" {
				r.unsafe = true
			}
		case "property_identifier":
			if strings.ContainsRune(n.Content(src), '\\') {
				r.unsafe = true
			}
		case "variable_declarator", "function_declaration", "function_expression", "generator_function", "generator_function_declaration",
			"function_signature":
			jsReceiverTargets(n.ChildByFieldName("name"), bind)
		case "internal_module", "module":
			name := n.ChildByFieldName("name")
			for name != nil && (name.Type() == "nested_identifier" || name.Type() == "member_expression") {
				name = name.ChildByFieldName("object")
			}
			jsReceiverTargets(name, bind)
		case "class", "class_declaration", "abstract_class_declaration", "enum_declaration":
			if name := n.ChildByFieldName("name"); name != nil {
				r.types[name.Content(src)] = true
				bind(name)
			}
		case "interface_declaration":
			r.types[n.ChildByFieldName("name").Content(src)] = true
		case "arrow_function", "catch_clause":
			jsReceiverTargets(n.ChildByFieldName("parameter"), bind)
		case "formal_parameters", "import_clause", "namespace_import", "import_require_clause":
			if n.Type() == "formal_parameters" && !jsReceiverValueFunction(n.Parent()) {
				break
			}
			for c := range n.NamedChildren() {
				jsReceiverTargets(c, bind)
			}
		case "import_alias":
			jsReceiverTargets(n.NamedChild(0), bind)
		case "import_specifier":
			local := n.ChildByFieldName("alias")
			if local == nil {
				local = n.ChildByFieldName("name")
			}
			jsReceiverTargets(local, bind)
		case "assignment_expression", "augmented_assignment_expression":
			writes = append(writes, n.ChildByFieldName("left"))
		case "for_in_statement":
			left := n.ChildByFieldName("left")
			if n.ChildByFieldName("kind") == nil {
				writes = append(writes, left)
			} else {
				jsReceiverTargets(left, bind)
			}
		case "update_expression":
			writes = append(writes, n.ChildByFieldName("argument"))
		case "unary_expression":
			if n.ChildByFieldName("operator").Content(src) == "delete" {
				writes = append(writes, n.ChildByFieldName("argument"))
			}
		}
	})
	// Only unbound constructor/global names denote the platform here. Collect
	// their writes after declarations so a later shadow is respected too.
	if len(writes) != 0 {
		r.writes = make(map[jsReceiverWrite]bool)
		for _, write := range writes {
			jsReceiverTargets(write, r.recordWrite)
		}
	}
	return r
}

// Follow only target fields. In { [key]: x = fallback } only x is a target.
func jsReceiverTargets(n *sitter.Node, visit func(*sitter.Node)) {
	if n == nil {
		return
	}
	switch n.Type() {
	case "identifier", "type_identifier", "shorthand_property_identifier_pattern", "member_expression", "subscript_expression":
		visit(n)
	case "required_parameter", "optional_parameter":
		pattern := n.ChildByFieldName("pattern")
		if pattern == nil {
			pattern = n.ChildByFieldName("name")
		}
		jsReceiverTargets(pattern, visit)
	case "array_pattern", "object_pattern":
		for c := range n.NamedChildren() {
			jsReceiverTargets(c, visit)
		}
	case "rest_pattern", "parenthesized_expression", "non_null_expression", "as_expression", "satisfies_expression":
		for c := range n.NamedChildren() {
			if c.Type() != "comment" {
				jsReceiverTargets(c, visit)
				break
			}
		}
	case "pair_pattern":
		jsReceiverTargets(n.ChildByFieldName("value"), visit)
	case "assignment_pattern", "object_assignment_pattern":
		jsReceiverTargets(n.ChildByFieldName("left"), visit)
	}
}

// For writes, follow only the object spine. A computed index is a read, not
// a write to that index's binding. "*" means the property is not known.
func jsReceiverWritePath(n *sitter.Node, src []byte) (string, []string) {
	var fields []string
	for n = jsUnwrapReceiver(n); n != nil; n = jsUnwrapReceiver(n.ChildByFieldName("object")) {
		switch n.Type() {
		case "identifier", "shorthand_property_identifier_pattern":
			slices.Reverse(fields)
			return n.Content(src), fields
		case "member_expression":
			fields = append(fields, n.ChildByFieldName("property").Content(src))
		case "subscript_expression":
			index := n.ChildByFieldName("index")
			key := "*"
			if index.Type() == "string" {
				text := index.Content(src)
				if !strings.ContainsRune(text, '\\') {
					key = text[1 : len(text)-1]
					if key == "" || key == "*" {
						return "", nil // Neither names a callable identifier.
					}
				}
			}
			fields = append(fields, key)
		default:
			return "", nil
		}
	}
	return "", nil
}

func (r *jsBuiltinReceivers) recordWrite(n *sitter.Node) {
	name, fields := jsReceiverWritePath(n, r.src)
	if name == "" {
		return
	}
	binding, bound := r.bindingAt(name, n.StartByte())
	platformName := name == "globalThis" || jsStaticReceiverFamily(name) != ""
	if !bound && platformName && len(r.bindings[name]) != 0 {
		r.unsafe = true // A platform write outside the local binding's scope.
	}
	if bound || !platformName {
		switch len(fields) {
		case 0:
			r.writes[jsReceiverWrite{owner: name}] = true
		case 1:
			r.writes[jsReceiverWrite{owner: name, member: fields[0]}] = true
		}
		if bound && !binding.visibleAt(n.StartByte()) && platformName {
			r.unsafe = true // Cannot tell whether this write reaches the platform.
		}
		return
	}
	if name == "globalThis" {
		if len(fields) == 0 || fields[0] == "*" {
			r.unsafe = true
			return
		}
		name, fields = fields[0], fields[1:]
	}
	family := jsStaticReceiverFamily(name)
	if family == "" {
		return
	}
	form := "static"
	switch {
	case len(fields) == 0:
		form = "global"
	case fields[0] == "prototype":
		form, fields = "instance", fields[1:]
	case fields[0] == "*":
		r.writes[jsReceiverWrite{"global", family, "*"}] = true
		return
	}
	switch len(fields) {
	case 0:
		r.writes[jsReceiverWrite{form, family, ""}] = true
	case 1:
		r.writes[jsReceiverWrite{form, family, fields[0]}] = true
	}
}

func jsReceiverBindingAt(n, p *sitter.Node, src []byte) jsReceiverBinding {
	if n.Type() == "shorthand_property_identifier_pattern" {
		return jsReceiverBinding{known: graph.BuiltinReceiverHint{Form: "unknown"}}
	}
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
			return jsReceiverBinding{known: graph.BuiltinReceiverHint{Form: "unknown"}} // No var hoisting proof.
		}
		scope := declaration.Parent()
		if scope.Type() == "export_statement" {
			scope = scope.Parent()
		}
		if scope.Type() == "program" || scope.Type() == "statement_block" {
			return jsReceiverBinding{scope: scope, decl: p}
		}
		return jsReceiverBinding{known: graph.BuiltinReceiverHint{Form: "unknown"}}
	case "required_parameter":
		if !n.Equal(p.ChildByFieldName("pattern")) && !n.Equal(p.ChildByFieldName("name")) {
			break
		}
		if params := p.Parent(); params.Type() == "formal_parameters" && jsReceiverValueFunction(params.Parent()) {
			return jsReceiverBinding{scope: params.Parent(), decl: p}
		}
	case "arrow_function":
		return jsReceiverBinding{scope: p, decl: n}
	case "optional_parameter", "assignment_pattern", "object_assignment_pattern", "pair_pattern", "rest_pattern", "array_pattern", "object_pattern",
		"catch_clause", "for_in_statement":
		return jsReceiverBinding{known: graph.BuiltinReceiverHint{Form: "unknown"}}
	case "formal_parameters":
		// A unique untyped JS parameter gives no family and no contrary
		// evidence unless it shadows a builtin spelling.
		return jsReceiverBinding{scope: p.Parent(), decl: p}
	}
	return jsReceiverBinding{}
}

// Value functions have a body. TS type signatures have formal_parameters too,
// but do not create a value scope that can shadow an executable receiver.
func jsReceiverValueFunction(n *sitter.Node) bool {
	if n == nil {
		return false
	}
	switch n.Type() {
	case "function_declaration", "function_expression", "generator_function", "generator_function_declaration", "arrow_function", "method_definition":
		return n.ChildByFieldName("body") != nil
	default:
		return false
	}
}

// Unsupported binding shapes still shadow names in their lexical scope. var
// is function-scoped; a named expression owns its name inside the expression.
func jsReceiverBindingScope(n *sitter.Node) *sitter.Node {
	functionScoped := false
	for p := n.Parent(); p != nil; p = p.Parent() {
		switch p.Type() {
		case "variable_declaration":
			functionScoped = true
		case "statement_block", "switch_body", "for_statement", "for_in_statement":
			if !functionScoped {
				return p
			}
		case "class", "function_expression", "generator_function", "arrow_function", "method_definition", "catch_clause", "program":
			return p
		case "class_declaration", "abstract_class_declaration", "function_declaration", "generator_function_declaration":
			if name := p.ChildByFieldName("name"); name == nil || !name.Equal(n) {
				return p
			}
		}
	}
	return nil
}

func (r *jsBuiltinReceivers) bindingAt(name string, site uint32) (jsReceiverBinding, bool) {
	var best jsReceiverBinding
	for _, b := range r.bindings[name] {
		if site < b.scope.StartByte() || site >= b.scope.EndByte() {
			continue
		}
		if best.scope == nil || b.scope.EndByte()-b.scope.StartByte() < best.scope.EndByte()-best.scope.StartByte() {
			best = b
		}
	}
	return best, best.scope != nil
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
	hint := r.memberHint(fn, call.StartByte())
	hint.Language = r.language
	edge.SetBuiltinReceiverHint(hint)
}

func (r *jsBuiltinReceivers) memberHint(fn *sitter.Node, site uint32) graph.BuiltinReceiverHint {
	if r.unsafe {
		return graph.BuiltinReceiverHint{Form: "unknown"}
	}
	obj := jsUnwrapReceiver(fn.ChildByFieldName("object"))
	hint := r.receiver(obj, site)
	if len(r.writes) == 0 {
		return hint
	}
	member := fn.ChildByFieldName("property").Content(r.src)
	if obj != nil && obj.Type() == "identifier" {
		name := obj.Content(r.src)
		if r.writes[jsReceiverWrite{owner: name, member: member}] {
			return graph.BuiltinReceiverHint{Form: "shadowed"}
		}
		if r.writes[jsReceiverWrite{owner: name, member: "*"}] {
			return graph.BuiltinReceiverHint{Form: "unknown"}
		}
	}
	if family := hint.Family; family != "" {
		if r.writes[jsReceiverWrite{"global", family, ""}] || r.writes[jsReceiverWrite{hint.Form, family, ""}] ||
			r.writes[jsReceiverWrite{hint.Form, family, member}] {
			return graph.BuiltinReceiverHint{Form: "shadowed"}
		}
		if r.writes[jsReceiverWrite{"global", family, "*"}] || r.writes[jsReceiverWrite{hint.Form, family, "*"}] {
			return graph.BuiltinReceiverHint{Form: "unknown"}
		}
	}
	return hint
}

func (r *jsBuiltinReceivers) receiver(node *sitter.Node, site uint32) graph.BuiltinReceiverHint {
	n := jsUnwrapReceiver(node)
	if n == nil {
		return graph.BuiltinReceiverHint{Form: "unknown"}
	}
	switch n.Type() {
	case "identifier":
		name := n.Content(r.src)
		if r.writes[jsReceiverWrite{owner: name}] {
			return graph.BuiltinReceiverHint{Form: "shadowed"}
		}
		if b, exists := r.bindingAt(name, site); exists {
			if !b.visibleAt(site) {
				return graph.BuiltinReceiverHint{Form: "shadowed"}
			}
			if b.decl == nil {
				if b.known.Form != "" && b.known.Form != "unknown" {
					return b.known
				}
				if jsStaticReceiverFamily(name) != "" {
					return graph.BuiltinReceiverHint{Form: "shadowed"}
				}
				return graph.BuiltinReceiverHint{Form: "unknown"}
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
			return graph.BuiltinReceiverHint{Form: "unknown"}
		}
		var family string
		switch t.Type() {
		case "array_type":
			family = "array"
		case "predefined_type":
			if name := t.Content(r.src); name == "string" || name == "number" {
				family = name
			}
		case "type_identifier", "generic_type":
			if t.Type() == "generic_type" {
				t = t.ChildByFieldName("name")
			}
			name := t.Content(r.src)
			if r.types[name] {
				return graph.BuiltinReceiverHint{Form: "user", Name: name}
			}
			if b, ok := r.bindingAt(name, decl.StartByte()); ok && b.known.Form == "import" {
				return b.known
			}
		}
		if family == "" || (value != nil && hint.Form == "unknown") {
			return graph.BuiltinReceiverHint{Form: "unknown"}
		}
		if value != nil && hint.Family != family {
			return graph.BuiltinReceiverHint{Form: "shadowed"}
		}
		return graph.BuiltinReceiverHint{Form: "instance", Family: family}
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
			if r.writes[jsReceiverWrite{owner: name}] {
				return graph.BuiltinReceiverHint{Form: "shadowed"}
			}
			if b, exists := r.bindingAt(name, n.StartByte()); exists {
				if b.known.Form == "import" && b.visibleAt(n.StartByte()) {
					return b.known
				}
				return graph.BuiltinReceiverHint{Form: "shadowed"}
			}
			switch name {
			case "Array", "String", "Number", "Map", "Set", "Promise":
				family = jsStaticReceiverFamily(name)
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
