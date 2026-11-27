package languages

import (
	"maps"
	"slices"
	"strings"

	"github.com/zzet/gortex/internal/graph"
	sitter "github.com/zzet/gortex/internal/parser/tsitter"
)

// Inspect only source-file declarations. The native graph uses the same node
// ID for a package var/const and a local var/const, so it cannot prove scope.
func goBuiltinPackageScope(root *sitter.Node, src []byte) string {
	var pkg string
	names := make(map[string]struct{})
	var declaration func(*sitter.Node)
	declaration = func(n *sitter.Node) {
		switch n.Type() {
		case "package_clause":
			if name := n.NamedChild(0); name != nil && name.Type() == "package_identifier" {
				pkg = name.Content(src)
			}
		case "function_declaration", "var_spec", "const_spec", "type_spec", "type_alias":
			for i := 0; i < int(n.ChildCount()); i++ {
				if n.FieldNameForChild(i) == "name" {
					if name := n.Child(i).Content(src); graph.IsGoBuiltinFunction(name) {
						names[name] = struct{}{}
					}
				}
			}
		case "var_declaration", "const_declaration", "type_declaration", "var_spec_list", "const_spec_list", "type_spec_list":
			for child := range n.NamedChildren() {
				declaration(child)
			}
		}
	}
	for n := range root.NamedChildren() {
		declaration(n)
	}
	if pkg == "" {
		return ""
	}
	return strings.Join(append([]string{pkg}, slices.Sorted(maps.Keys(names))...), " ")
}

// Check the Call's AST ancestry, not its line-based graph owner. This is
// evidence about the declaration body only; local block lifetimes and nested
// function literals do not prove absence. Package scope is checked separately.
func goBuiltinCallForm(call *sitter.Node, name string, src []byte, scopes map[uintptr]map[string]string) string {
	for child, parent := call, call.Parent(); parent != nil; child, parent = parent, parent.Parent() {
		switch parent.Type() {
		case "func_literal":
			return "unknown"
		case "function_declaration", "method_declaration":
			body := parent.ChildByFieldName("body")
			if body == nil || body.Id() != child.Id() || parent.HasError() {
				return "unknown"
			}
			bindings, ok := scopes[parent.Id()]
			if !ok {
				bindings = goBuiltinScopeBindings(parent, body, src)
				scopes[parent.Id()] = bindings
			}
			if form := bindings[name]; form != "" {
				return form
			}
			return "unbound"
		}
	}
	return "unknown"
}

// Collect possible binders once per queried function. A matching local name
// blocks proof of absence without computing its block or lifetime.
func goBuiltinScopeBindings(fn, body *sitter.Node, src []byte) map[string]string {
	bindings := make(map[string]string)
	mark := func(n *sitter.Node, form string) {
		if n.Type() == "identifier" || n.Type() == "type_identifier" {
			if name := n.Content(src); graph.IsGoBuiltinFunction(name) {
				bindings[name] = form
			}
		}
	}
	var walk func(*sitter.Node, bool)
	walk = func(n *sitter.Node, receiver bool) {
		if receiver {
			// Type arguments in the method receiver can declare type parameters.
			mark(n, "unknown")
		}
		switch n.Type() {
		case "func_literal":
			// A literal's declarations do not bind names in its enclosing scope.
			return
		case "var_spec", "const_spec", "type_spec", "type_alias":
			for i := 0; i < int(n.ChildCount()); i++ {
				if n.FieldNameForChild(i) == "name" {
					mark(n.Child(i), "unknown")
				}
			}
		case "short_var_declaration", "range_clause", "receive_statement", "type_switch_statement":
			field := "left"
			if n.Type() == "type_switch_statement" {
				field = "alias"
			}
			if names := n.ChildByFieldName(field); names != nil {
				for name := range names.NamedChildren() {
					mark(name, "unknown")
				}
			}
		}
		for i, count := 0, int(n.NamedChildCount()); i < count; i++ {
			walk(n.NamedChild(i), receiver)
		}
	}
	walk(body, false)
	if receiver := fn.ChildByFieldName("receiver"); receiver != nil {
		walk(receiver, true)
	}
	if params := fn.ChildByFieldName("type_parameters"); params != nil {
		for decl := range params.NamedChildren() {
			if decl.Type() == "type_parameter_declaration" {
				for param := range decl.NamedChildren() {
					// Constraints are wrapped in type_constraint, not identifiers.
					mark(param, "shadowed")
				}
			}
		}
	}
	// Named parameters, named results and the method receiver have body-wide scope.
	for _, field := range []string{"receiver", "parameters", "result"} {
		if list := fn.ChildByFieldName(field); list != nil && list.Type() == "parameter_list" {
			for name := range extractGoParamNames(list, src) {
				if graph.IsGoBuiltinFunction(name) {
					bindings[name] = "shadowed"
				}
			}
		}
	}
	return bindings
}
