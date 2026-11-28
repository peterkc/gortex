package languages

import (
	"slices"
	"strings"

	"github.com/zzet/gortex/internal/graph"
	sitter "github.com/zzet/gortex/internal/parser/tsitter"
)

// jsBuiltinReceivers tracks lexical bindings and their write vetoes.
// A receiver gets positive evidence only from a supported visible binding or
// an unbound builtin spelling. Reads and type-only parameters never bind names.
type jsBuiltinReceivers struct {
	language string
	src      []byte
	bindings map[string]map[uintptr]jsReceiverBinding
	types    map[string]map[uintptr]bool
	writes   map[jsReceiverWrite]bool
	unsafe   bool
}

// Empty form denotes a write to a local name; "global" replaces the family.
// Static and instance writes affect only their own member (empty = all).
type jsReceiverWrite struct {
	form, owner, member string
	scope               uintptr // zero for an unbound name or a platform family
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
	var writes, mutations []*sitter.Node
	r.bindings = make(map[string]map[uintptr]jsReceiverBinding)
	r.types = make(map[string]map[uintptr]bool)
	r.writes = make(map[jsReceiverWrite]bool)
	module := false
	for child := range root.NamedChildren() {
		if child.Type() == "import_statement" || child.Type() == "export_statement" {
			module = true
			break
		}
	}
	sloppyScript := language == "javascript" && !module
	bindType := func(n *sitter.Node, isInterface bool) {
		name := n.Content(src)
		scope := jsReceiverBindingScope(n, src, sloppyScript)
		if r.types[name] == nil {
			r.types[name] = make(map[uintptr]bool)
		}
		key := scope.Id()
		r.types[name][key] = r.types[name][key] || !isInterface || module || scope.Type() != "program"
	}
	bind := func(n *sitter.Node) {
		name := n.Content(src)
		b := jsReceiverBindingAt(n, n.Parent(), src)
		if b.scope == nil {
			b.scope = jsReceiverBindingScope(n, src, sloppyScript)
		}
		// Every binder is below the program root, so the scope walk always
		// finds an enclosing scope. Scope IDs are stable across AST wrappers.
		if r.bindings[name] == nil {
			r.bindings[name] = make(map[uintptr]jsReceiverBinding)
		}
		key := b.scope.Id()
		if _, duplicate := r.bindings[name][key]; duplicate {
			b = jsReceiverBinding{scope: b.scope, known: graph.BuiltinReceiverHint{Form: "shadowed"}}
		}
		r.bindings[name][key] = b
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
				bindType(name, false)
				bind(name)
			}
		case "interface_declaration":
			if name := n.ChildByFieldName("name"); name != nil && !jsReceiverGlobalAugmentation(n) {
				// Script-level interfaces may merge with global platform types.
				bindType(name, true)
			}
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
		case "call_expression":
			if object := jsObjectMutationTarget(n, src); object != nil {
				mutations = append(mutations, object)
			}
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
	for _, write := range writes {
		jsReceiverTargets(write, r.recordWrite)
	}
	for _, target := range mutations {
		r.recordMutation(target)
	}
	return r
}

func jsObjectMutationTarget(call *sitter.Node, src []byte) *sitter.Node {
	name, fields := jsReceiverWritePath(call.ChildByFieldName("function"), src)
	if len(fields) != 1 {
		return nil
	}
	var mutation bool
	switch name {
	case "Object":
		mutation = slices.Contains([]string{"assign", "defineProperty", "defineProperties", "setPrototypeOf"}, fields[0])
	case "Reflect":
		mutation = slices.Contains([]string{"defineProperty", "set", "setPrototypeOf"}, fields[0])
	}
	if mutation {
		if args := call.ChildByFieldName("arguments"); args != nil {
			for arg := range args.NamedChildren() {
				if arg.Type() != "comment" {
					return arg
				}
			}
		}
	}
	return nil
}

func jsGlobalAlias(name string) bool {
	return name == "window" || name == "self" || name == "global"
}

// An interface in declare global augments the platform type namespace; it
// does not prove a local user type.
func jsReceiverGlobalAugmentation(n *sitter.Node) bool {
	for p := n.Parent(); p != nil; p = p.Parent() {
		if p.Type() != "ambient_declaration" {
			continue
		}
		for i := range p.ChildCount() {
			if p.Child(int(i)).Type() == "global" {
				return true
			}
		}
	}
	return false
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

// A direct alias of a known platform constructor or its prototype can
// mutate that object. Do not follow computed initializers or alias chains.
func (r *jsBuiltinReceivers) platformAlias(binding jsReceiverBinding, site *sitter.Node) (string, []string) {
	if binding.decl == nil || !binding.visibleAt(site) {
		return "", nil
	}
	value := binding.decl.ChildByFieldName("value")
	name, fields := jsReceiverWritePath(value, r.src)
	if name == "" {
		return "", nil
	}
	if _, bound := r.bindingAt(name, value); bound {
		return "", nil
	}
	name, fields, _ = jsReceiverGlobalPath(name, fields)
	if jsStaticReceiverFamily(name) != "" && (len(fields) == 0 || len(fields) == 1 && fields[0] == "prototype") {
		return name, fields
	}
	return "", nil
}

// Walk nested global-object aliases. Window/self/global were always treated
// conservatively rather than as a proved single platform family.
func jsReceiverGlobalPath(name string, fields []string) (string, []string, bool) {
	alias := false
	for name == "globalThis" || jsGlobalAlias(name) {
		alias = alias || jsGlobalAlias(name)
		if len(fields) == 0 || fields[0] == "*" {
			return "", nil, true
		}
		name, fields = fields[0], fields[1:]
	}
	return name, fields, alias && jsStaticReceiverFamily(name) != ""
}

func (r *jsBuiltinReceivers) recordWrite(n *sitter.Node) {
	name, fields := jsReceiverWritePath(n, r.src)
	if name == "" {
		return
	}
	binding, bound := r.bindingAt(name, n)
	platformName := name == "globalThis" || jsGlobalAlias(name) || jsStaticReceiverFamily(name) != ""
	if !bound && (name == "globalThis" || jsStaticReceiverFamily(name) != "") && len(r.bindings[name]) != 0 {
		r.unsafe = true // A platform write outside the local binding's scope.
	}
	if bound || !platformName {
		switch len(fields) {
		case 0:
			r.writes[jsReceiverWrite{owner: name, scope: binding.scope.Id()}] = true
		case 1:
			r.writes[jsReceiverWrite{owner: name, member: fields[0], scope: binding.scope.Id()}] = true
		}
		if bound && !binding.visibleAt(n) && (name == "globalThis" || jsStaticReceiverFamily(name) != "") {
			r.unsafe = true // Cannot tell whether this write reaches the platform.
		}
		if len(fields) != 0 {
			if alias, prefix := r.platformAlias(binding, n); alias != "" {
				r.recordPlatformWrite(alias, append(prefix, fields...))
			}
		}
		return
	}
	name, fields, unsafe := jsReceiverGlobalPath(name, fields)
	if unsafe {
		r.unsafe = true
		return
	}
	r.recordPlatformWrite(name, fields)
}

func (r *jsBuiltinReceivers) recordPlatformWrite(name string, fields []string) {
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
		r.writes[jsReceiverWrite{form: "global", owner: family, member: "*"}] = true
		return
	}
	switch len(fields) {
	case 0:
		r.writes[jsReceiverWrite{form: form, owner: family}] = true
	case 1:
		r.writes[jsReceiverWrite{form: form, owner: family, member: fields[0]}] = true
	}
}

func (r *jsBuiltinReceivers) recordMutation(target *sitter.Node) {
	name, fields := jsReceiverWritePath(target, r.src)
	if name == "" {
		return
	}
	if binding, bound := r.bindingAt(name, target); bound {
		if len(fields) == 0 {
			r.writes[jsReceiverWrite{owner: name, member: "*", scope: binding.scope.Id()}] = true
		}
		alias, prefix := r.platformAlias(binding, target)
		if alias == "" {
			return
		}
		name, fields = alias, append(prefix, fields...)
	}
	if name == "globalThis" || jsGlobalAlias(name) || jsStaticReceiverFamily(name) != "" && (len(fields) == 0 || fields[0] == "prototype" || fields[0] == "*") {
		r.unsafe = true
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
func jsReceiverBindingScope(n *sitter.Node, src []byte, sloppyScript bool) *sitter.Node {
	functionScoped := false
	for p := n.Parent(); p != nil; p = p.Parent() {
		switch p.Type() {
		case "variable_declaration":
			functionScoped = true
		case "for_in_statement":
			if kind := p.ChildByFieldName("kind"); kind != nil && kind.Type() == "var" {
				functionScoped = true
			}
			if !functionScoped {
				return p
			}
		case "statement_block", "switch_body", "for_statement", "catch_clause":
			if !functionScoped {
				return p
			}
		case "class", "function_expression", "generator_function", "arrow_function", "method_definition", "program":
			return p
		case "function_declaration", "generator_function_declaration":
			if name := p.ChildByFieldName("name"); name == nil || !name.Equal(n) {
				return p
			}
			// A sloppy JS block function may also create a var binding in
			// its enclosing function. Do not prove a platform name there.
			if sloppyScript && jsStaticReceiverFamily(n.Content(src)) != "" {
				functionScoped = true
			}
		case "class_declaration", "abstract_class_declaration":
			if name := p.ChildByFieldName("name"); name == nil || !name.Equal(n) {
				return p
			}
		}
	}
	return nil
}

// Every scope is an ancestor of its binding's use. Look up its stable node
// ID instead of comparing the ranges of all same-named sibling bindings.
func jsReceiverScopeLookup[T any](entries map[uintptr]T, site *sitter.Node) (T, bool) {
	if len(entries) != 0 {
		for scope := site; scope != nil; scope = scope.Parent() {
			if value, ok := entries[scope.Id()]; ok {
				return value, true
			}
		}
	}
	var zero T
	return zero, false
}

func (r *jsBuiltinReceivers) bindingAt(name string, site *sitter.Node) (jsReceiverBinding, bool) {
	return jsReceiverScopeLookup(r.bindings[name], site)
}

func (r *jsBuiltinReceivers) nameWritten(name string, site *sitter.Node) bool {
	b, _ := r.bindingAt(name, site)
	return r.writes[jsReceiverWrite{owner: name, scope: b.scope.Id()}]
}

func (b jsReceiverBinding) visibleAt(site *sitter.Node) bool {
	// The binding was found on the site's ancestor chain.
	return b.decl == nil || site.StartByte() >= b.decl.EndByte()
}

func (r *jsBuiltinReceivers) stamp(edge *graph.Edge, call *sitter.Node) {
	if r == nil || call == nil {
		return
	}
	fn := call.ChildByFieldName("function")
	if fn == nil || fn.Type() != "member_expression" {
		return
	}
	hint := r.memberHint(fn, call)
	hint.Language = r.language
	edge.SetBuiltinReceiverHint(hint)
}

func (r *jsBuiltinReceivers) memberHint(fn, site *sitter.Node) graph.BuiltinReceiverHint {
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
		binding, _ := r.bindingAt(name, site)
		write := jsReceiverWrite{owner: name, member: member, scope: binding.scope.Id()}
		if r.writes[write] {
			return graph.BuiltinReceiverHint{Form: "shadowed"}
		}
		write.member = "*"
		if r.writes[write] {
			return graph.BuiltinReceiverHint{Form: "unknown"}
		}
	}
	if family := hint.Family; family != "" {
		if r.writes[jsReceiverWrite{form: "global", owner: family, member: ""}] || r.writes[jsReceiverWrite{form: hint.Form, owner: family, member: ""}] ||
			r.writes[jsReceiverWrite{form: hint.Form, owner: family, member: member}] {
			return graph.BuiltinReceiverHint{Form: "shadowed"}
		}
		if r.writes[jsReceiverWrite{form: "global", owner: family, member: "*"}] || r.writes[jsReceiverWrite{form: hint.Form, owner: family, member: "*"}] {
			return graph.BuiltinReceiverHint{Form: "unknown"}
		}
	}
	return hint
}

func (r *jsBuiltinReceivers) receiver(node, site *sitter.Node) graph.BuiltinReceiverHint {
	n := jsUnwrapReceiver(node)
	if n == nil {
		return graph.BuiltinReceiverHint{Form: "unknown"}
	}
	switch n.Type() {
	case "identifier":
		name := n.Content(r.src)
		if r.nameWritten(name, site) {
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
		case "import", "shadowed":
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
			if local, found := jsReceiverScopeLookup(r.types[name], decl); found {
				if local {
					return graph.BuiltinReceiverHint{Form: "user", Name: name}
				}
				return graph.BuiltinReceiverHint{Form: "unknown"}
			}
			if b, ok := r.bindingAt(name, decl); ok && b.known.Form == "import" {
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
			return graph.BuiltinReceiverHint{Form: "unknown"} // bigint is not number.
		}
		family = "number"
	case "object":
		if n.NamedChildCount() != 0 {
			return graph.BuiltinReceiverHint{Form: "unknown"} // Own members and spreads are not checked.
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
			if r.nameWritten(name, n) {
				return graph.BuiltinReceiverHint{Form: "shadowed"}
			}
			if b, exists := r.bindingAt(name, n); exists {
				if b.known.Form == "import" && b.visibleAt(n) {
					return b.known
				}
				return graph.BuiltinReceiverHint{Form: "shadowed"}
			}
			switch name {
			case "Array", "String", "Number", "Map", "Set", "Promise":
				family = jsStaticReceiverFamily(name)
			}
		case "member_expression", "subscript_expression":
			return r.receiver(ctor, n)
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
