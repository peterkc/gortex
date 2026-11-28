package contracts

import (
	"strings"

	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/parser"
)

// applyBodyFactsToHTTPContract overrides Meta keys with AST-derived
// facts when they're more authoritative than the regex enricher's
// output. Called after enrichHTTPContract has run; AST output wins
// on the keys it can confidently produce.
//
// Phase 1 invariant: this function may not regress an existing
// Meta value to a strictly less informative one — i.e. if the
// regex pass set response_type but the AST can't, leave the regex
// value alone. The AST only writes when it has a confident answer.
//
// Both provider and consumer contracts are handled. The semantics
// differ by Role:
//   - Provider: WriteJSON/Encode → response_type; Decode/BodyParser/
//     ShouldBind*/Unmarshal → request_type
//   - Consumer: Marshal/MarshalIndent → request_type;
//     Decode/Unmarshal → response_type (response from server is
//     decoded into a typed variable)
func applyBodyFactsToHTTPContract(c *Contract, fileNodes []*graph.Node, tree *parser.ParseTree) {
	if c == nil || tree == nil {
		return
	}
	handler := findContractHandlerNode(c, fileNodes)
	if handler == nil {
		return
	}
	bf := MakeBodyFacts(tree, handler)
	if _, isNop := bf.(nopBodyFacts); isNop {
		return
	}

	switch c.Role {
	case RoleProvider:
		applyProviderFacts(c, fileNodes, bf)
	case RoleConsumer:
		applyConsumerFacts(c, fileNodes, bf)
	}
}

// applyProviderFacts handles the server-side enrichment.
func applyProviderFacts(c *Contract, fileNodes []*graph.Node, bf BodyFacts) {
	applyProviderFactsWithLookup(c, bf, func(name string) string { return resolveTypeInFile(name, fileNodes) })
}

func applyProviderFactsWithLookup(c *Contract, bf BodyFacts, lookup func(string) string) {
	if writes := bf.StatusWrites(); len(writes) > 0 {
		mergeIntStrings(c.Meta, "status_codes", writes)
	}
	if reads := bf.QueryReads(); len(reads) > 0 {
		mergeStringList(c.Meta, "query_params", reads)
	}
	// Request type: take the first non-Marshal request binding
	// (Marshal is consumer-side, see applyConsumerFacts).
	for _, rb := range bf.RequestBindings() {
		if rb.Helper == "Marshal" || rb.Helper == "MarshalIndent" {
			continue
		}
		applyRequestBindingToContract(c, bf, rb, lookup)
		break
	}
	calls := bf.ResponseCalls()
	if len(calls) == 0 {
		return
	}
	rc := lastSuccessResponseCall(calls)
	applyResponseCallToContract(c, bf, rc, lookup)
}

// applyConsumerFacts handles the client-side enrichment.
//   - Marshal(req)   → request_type from VarBinding(req).TypeID
//   - Decode(&out)   → response_type from VarBinding(out).TypeID
//   - Unmarshal(b,&out) → response_type from VarBinding(out).TypeID
//   - var anonymous composite (Marshal(&Req{}))
//     → request_type from CompositeType
func applyConsumerFacts(c *Contract, fileNodes []*graph.Node, bf BodyFacts) {
	for _, rb := range bf.RequestBindings() {
		switch rb.Helper {
		case "Marshal", "MarshalIndent":
			applyConsumerBinding(c, fileNodes, bf, rb, "request")
		case "Decode", "Unmarshal":
			applyConsumerBinding(c, fileNodes, bf, rb, "response")
		}
	}
}

// applyConsumerBinding stamps either request_type or response_type
// (per kind) from a binding's variable lookup.
func applyConsumerBinding(c *Contract, fileNodes []*graph.Node, bf BodyFacts, rb RequestBinding, kind string) {
	typeKey := "request_type"
	exprKey := "request_expr"
	if kind == "response" {
		typeKey = "response_type"
		exprKey = "response_expr"
	}
	if existing, _ := c.Meta[typeKey].(string); existing != "" {
		return
	}
	resolved := ""
	repeated := false
	switch {
	case rb.CompositeType != "":
		resolved = resolveTypeInFile(rb.CompositeType, fileNodes)
	case rb.VarName != "":
		b := bf.VarBinding(rb.VarName)
		if b.TypeID != "" {
			resolved = resolveTypeInFile(b.TypeID, fileNodes)
			repeated = b.Repeated
		}
	}
	if resolved == "" {
		return
	}
	c.Meta[typeKey] = resolved
	if repeated {
		repeatedKey := "request_repeated"
		if kind == "response" {
			repeatedKey = "response_repeated"
		}
		c.Meta[repeatedKey] = true
	}
	c.Meta["schema_source"] = "extracted"
	delete(c.Meta, exprKey)
}

// lastSuccessResponseCall picks the response call that most likely
// represents the success path. Heuristic: the last call whose status
// is 2xx or unknown; falls back to the last call.
func lastSuccessResponseCall(calls []ResponseCall) ResponseCall {
	for i := len(calls) - 1; i >= 0; i-- {
		c := calls[i]
		if !c.StatusKnown {
			return c
		}
		if c.StatusCode >= 200 && c.StatusCode < 300 {
			return c
		}
	}
	return calls[len(calls)-1]
}

// applyResponseCallToContract turns one ResponseCall's value
// argument into Meta updates: response_type, response_envelope,
// response_repeated, schema_source.
func applyResponseCallToContract(c *Contract, bf BodyFacts, rc ResponseCall, lookup func(string) string) {
	if rc.ValueArg == nil {
		return
	}
	switch rc.ValueArg.Kind() {
	case "composite_literal":
		applyCompositeResponse(c, bf, rc.ValueArg, lookup)
	case "identifier":
		applyIdentResponse(c, bf, rc.ValueExpr, lookup)
	case "interpreted_string_literal", "raw_string_literal":
		applyLiteralResponse(c, "string")
	case "int_literal":
		applyLiteralResponse(c, "int")
	case "float_literal":
		applyLiteralResponse(c, "float64")
	case "true", "false":
		applyLiteralResponse(c, "bool")
	case "unary_expression":
		// `&Foo{...}` or `&value` — unwrap.
		inner := unaryOperand(rc.ValueArg)
		if inner == nil {
			return
		}
		switch inner.Kind() {
		case "composite_literal":
			applyCompositeResponse(c, bf, inner, lookup)
		case "identifier":
			applyIdentResponse(c, bf, inner.Text(), lookup)
		}
	}
}

// applyLiteralResponse stamps a primitive response_type when the
// helper's value argument is a string / int / float / bool literal.
func applyLiteralResponse(c *Contract, typeName string) {
	c.Meta["response_type"] = typeName
	c.Meta["schema_source"] = "extracted"
	delete(c.Meta, "response_expr")
}

// applyRequestBindingToContract stamps request_type from one request
// binding. Resolution order:
//  1. CompositeType (Decode(&Foo{})) — direct, no var lookup needed
//  2. VarBinding(VarName).TypeID — chase the variable to its declared
//     type (e.g. `var req CreateRequest; Decode(&req)` → CreateRequest)
//  3. Leave c.Meta alone (regex pass might have set it, or it stays
//     unset)
func applyRequestBindingToContract(c *Contract, bf BodyFacts, rb RequestBinding, lookup func(string) string) {
	if rb.CompositeType != "" {
		c.Meta["request_type"] = lookup(rb.CompositeType)
		c.Meta["schema_source"] = "extracted"
		delete(c.Meta, "request_expr")
		return
	}
	if rb.VarName == "" {
		return
	}
	b := bf.VarBinding(rb.VarName)
	if b.TypeID == "" {
		return
	}
	c.Meta["request_type"] = lookup(b.TypeID)
	c.Meta["schema_source"] = "extracted"
	delete(c.Meta, "request_expr")
}

// applyCompositeResponse handles `WriteJSON(w, code, Foo{...})` and
// `WriteJSON(w, code, map[string]any{...})`. For map composites we
// walk the entries into an envelope.
func applyCompositeResponse(c *Contract, bf BodyFacts, valueNode *Node, lookup func(string) string) {
	if valueNode == nil || valueNode.Inner() == nil {
		return
	}
	typ := compositeTypeNode(valueNode.Inner())
	if typ == nil {
		return
	}
	switch typ.Type() {
	case "map_type":
		entries := bf.MapLiteralEntries(valueNode)
		if len(entries) == 0 {
			return
		}
		env := buildEnvelopeFromEntries(bf, entries, lookup)
		if len(env) == 0 {
			return
		}
		c.Meta["response_envelope"] = env
		if len(env) == 1 {
			if t, _ := env[0]["type"].(string); t != "" {
				c.Meta["response_type"] = t
			}
			if r, _ := env[0]["repeated"].(bool); r {
				c.Meta["response_repeated"] = true
			}
		}
		c.Meta["schema_source"] = "extracted"
		delete(c.Meta, "response_expr")
	case "slice_type", "array_type":
		// `[]Foo{...}` — record the element type as response, marked
		// repeated. Walk the slice element via bindingFromComposite.
		if goBF, ok := bf.(*goBodyFacts); ok {
			b := goBF.bindingFromComposite(valueNode.Inner())
			if b.TypeID != "" {
				c.Meta["response_type"] = lookup(b.TypeID)
				if b.Repeated {
					c.Meta["response_repeated"] = true
				}
				c.Meta["schema_source"] = "extracted"
				delete(c.Meta, "response_expr")
			}
		}
	default:
		// Named type: `WriteJSON(w, code, Foo{...})` → response = Foo.
		if goBF, ok := bf.(*goBodyFacts); ok {
			b := goBF.bindingFromComposite(valueNode.Inner())
			if b.TypeID != "" {
				c.Meta["response_type"] = lookup(b.TypeID)
				c.Meta["schema_source"] = "extracted"
				delete(c.Meta, "response_expr")
			}
		}
	}
}

// applyIdentResponse handles `WriteJSON(w, code, repos)`. Looks up
// repos's binding; if it resolves to a known type or a method call,
// stamps the response_type or leaves a clean response_expr for the
// post-pass to trace.
func applyIdentResponse(c *Contract, bf BodyFacts, ident string, lookup func(string) string) {
	ident = strings.TrimSpace(ident)
	if ident == "" {
		return
	}
	b := bf.VarBinding(ident)
	switch b.Kind {
	case BindingComposite, BindingSliceLit, BindingMakeSlice, BindingMakeMap:
		if b.TypeID != "" {
			c.Meta["response_type"] = lookup(b.TypeID)
			if b.Repeated {
				c.Meta["response_repeated"] = true
			}
			c.Meta["schema_source"] = "extracted"
			delete(c.Meta, "response_expr")
			return
		}
	case BindingStringLit, BindingIntLit, BindingFloatLit, BindingBoolLit,
		BindingPathValue, BindingFormValue, BindingHeaderValue, BindingQueryGet:
		c.Meta["response_type"] = b.TypeID
		c.Meta["schema_source"] = "extracted"
		delete(c.Meta, "response_expr")
		return
	case BindingMethodCall, BindingFuncCall:
		// Defer to the post-pass (resolveCallReturnTypes) which will
		// walk the graph from b.CallExpr to the method's return
		// type. We just clean up response_expr to be the bare ident
		// so the post-pass's `isLikelyIdentifier` branch matches.
		if rt, _ := c.Meta["response_type"].(string); rt == "" {
			c.Meta["response_expr"] = ident
		}
	}
}

// buildEnvelopeFromEntries converts AST map-literal entries into the
// `[]map[string]any` shape the dashboard renders. Each entry's value
// expression is matched against the body's bindings to resolve a
// type; when no fact is available the row stays with just an `expr`
// field for the post-pass to chase.
func buildEnvelopeFromEntries(bf BodyFacts, entries []KeyValue, lookup func(string) string) []map[string]any {
	out := make([]map[string]any, 0, len(entries))
	for _, e := range entries {
		row := map[string]any{"name": e.Key, "expr": e.ValueExpr}
		// Direct composite literals carry their type inline.
		if e.ValueNode != nil && e.ValueNode.Kind() == "composite_literal" {
			if goBF, ok := bf.(*goBodyFacts); ok {
				b := goBF.bindingFromComposite(e.ValueNode.Inner())
				if b.TypeID != "" {
					row["type"] = lookup(b.TypeID)
					if b.Repeated {
						row["repeated"] = true
					}
				}
			}
		} else {
			// Bare identifier: chase its binding.
			ident := strings.TrimLeft(e.ValueExpr, "&*")
			if isPlainIdent(ident) {
				b := bf.VarBinding(ident)
				if b.TypeID != "" {
					row["type"] = lookup(b.TypeID)
				}
				if b.Repeated {
					row["repeated"] = true
				}
			}
		}
		out = append(out, row)
	}
	return out
}

// findContractHandlerNode locates the handler graph node referenced
// by a contract's SymbolID.
func findContractHandlerNode(c *Contract, fileNodes []*graph.Node) *graph.Node {
	for _, n := range fileNodes {
		if n.ID == c.SymbolID {
			return n
		}
	}
	return nil
}

// unaryOperand returns the operand of a unary_expression Node.
func unaryOperand(n *Node) *Node {
	if n == nil || n.Inner() == nil {
		return nil
	}
	op := n.Inner().ChildByFieldName("operand")
	if op == nil {
		return nil
	}
	return NewNode(op, sourceOf(n))
}

// sourceOf retrieves the source bytes a Node was built with. Internal
// helper that reaches into the Node struct.
func sourceOf(n *Node) []byte {
	if n == nil {
		return nil
	}
	return n.src
}

// mergeIntStrings adds new ints into an existing []int meta field
// (or creates it) without duplicates. Preserves source order.
func mergeIntStrings(meta map[string]any, key string, additions []int) {
	if meta == nil {
		return
	}
	seen := map[int]bool{}
	out := []int{}
	if existing, ok := meta[key].([]int); ok {
		for _, v := range existing {
			if !seen[v] {
				seen[v] = true
				out = append(out, v)
			}
		}
	}
	for _, v := range additions {
		if !seen[v] {
			seen[v] = true
			out = append(out, v)
		}
	}
	if len(out) > 0 {
		meta[key] = out
	}
}

// mergeStringList adds strings to a []string Meta field, dedup,
// preserving source order.
func mergeStringList(meta map[string]any, key string, additions []string) {
	if meta == nil {
		return
	}
	seen := map[string]bool{}
	out := []string{}
	if existing, ok := meta[key].([]string); ok {
		for _, v := range existing {
			if !seen[v] {
				seen[v] = true
				out = append(out, v)
			}
		}
	}
	for _, v := range additions {
		if v == "" || seen[v] {
			continue
		}
		seen[v] = true
		out = append(out, v)
	}
	if len(out) > 0 {
		meta[key] = out
	}
}

// isPlainIdent reports whether s is a simple Go identifier (no dots,
// brackets, parens). Used to gate the var-binding lookup so a value
// expression like `len(repos)` isn't sent to BodyFacts.VarBinding.
func isPlainIdent(s string) bool {
	if s == "" {
		return false
	}
	for i, r := range s {
		if r == '_' ||
			(r >= 'a' && r <= 'z') ||
			(r >= 'A' && r <= 'Z') ||
			(i > 0 && r >= '0' && r <= '9') {
			continue
		}
		return false
	}
	return true
}
