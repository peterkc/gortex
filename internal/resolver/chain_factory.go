package resolver

import (
	"strings"

	"github.com/zzet/gortex/internal/graph"
)

// ResolveFactoryChains binds method calls on a static-factory / fluent-builder
// chain (`New().With(x).Build().Run()`) whose return types and methods live in
// different files — the cross-file completion the in-extractor (file-local)
// chain walk cannot do without a semantic provider.
//
// The extractor stamps the receiver expression on the call edge as
// Meta["receiver_expr"] when it could not type the chain itself. This pass
// re-walks that expression over the whole graph: the base segment's factory
// return type, then each hop's method return type, with a conformance walk to
// an implementor/subtype when a hop's method is declared on a supertype. The
// final method is bound on the resulting type, and the call edge re-targeted.
//
// It only ever touches edges still on an `unresolved::` placeholder, so an
// LSP-/compiler-resolved chain (already bound to a real node) is never
// overridden. Runs in the framework-synthesizer settle window, after the
// implements/extends edges exist, so the conformance walk sees them.
func ResolveFactoryChains(g graph.Store) int {
	return resolveFactoryChains(g, nil)
}

// resolveFactoryChains is the census-multiplexed form: cands, when non-nil,
// replaces the calls+references decode with the shared walk's pre-matched
// candidates (calls first, then references — each edge's resolution is
// independent, so the interleaving of the combined kind scan is not
// load-bearing).
func resolveFactoryChains(g graph.Store, cands *frameworkPassCandidates) int {
	if g == nil {
		return 0
	}
	// Scoped to the two kinds this pass ever acts on (below), instead of
	// AllEdges() decoding every kind in the graph — calls+references is a
	// fraction of the total edge count on a large multi-repo graph.
	stream := edgesByKinds(g, []graph.EdgeKind{graph.EdgeCalls, graph.EdgeReferences})
	if cands != nil {
		merged := append(append([]*graph.Edge(nil), cands.calls...), cands.refs...)
		stream = frameworkEdgeSeq(merged)
	}
	lookup := factoryChainLookup{
		nodes:    func(name string, _ func(*graph.Node) bool) []*graph.Node { return g.FindNodesByName(name) },
		incoming: g.GetInEdges,
		node:     g.GetNode,
	}
	resolved := 0
	var batch []graph.EdgeReindex
	for e := range stream {
		expr, method := factoryChainCall(e)
		if expr == "" || method == "" {
			continue
		}
		target, conformanceWalked := factoryChainTarget(lookup, e.From, expr, method)
		if target == "" {
			continue
		}
		batch = append(batch, rebindFactoryChain(e, target, conformanceWalked))
		resolved++
	}
	if len(batch) > 0 {
		g.ReindexEdges(batch)
	}
	return resolved
}

type factoryChainLookup struct {
	nodes    func(name string, role func(*graph.Node) bool) []*graph.Node
	incoming func(string) []*graph.Edge
	node     func(string) *graph.Node
}

func factoryChainCall(e *graph.Edge) (expr, method string) {
	if e == nil || !graph.IsUnresolvedTarget(e.To) {
		return "", ""
	}
	expr, _ = e.Meta["receiver_expr"].(string)
	if expr == "" {
		return "", ""
	}
	method = graph.UnresolvedName(e.To)
	if i := strings.LastIndexByte(method, '.'); i >= 0 {
		method = method[i+1:]
	}
	return expr, method
}

func factoryChainTarget(g factoryChainLookup, from, expr, method string) (string, bool) {
	recvType := walkChainExprType(g, expr)
	if recvType == "" {
		return "", false
	}
	target, conformanceWalked := resolveMemberByTypeConformant(g, recvType, method)
	if target == "" || target == from {
		return "", false
	}
	return target, conformanceWalked
}

func rebindFactoryChain(e *graph.Edge, target string, conformanceWalked bool) graph.EdgeReindex {
	oldTo := e.To
	e.To = target
	e.Origin = graph.OriginASTInferred
	e.Meta["via"] = "factory_chain"
	if conformanceWalked {
		e.Meta["conformance_walked"] = true
	}
	return graph.EdgeReindex{Edge: e, OldTo: oldTo}
}

// walkChainExprType returns the type a factory-chain receiver expression
// evaluates to, walking the graph: the base segment's factory return type (or
// the base itself when it names a known type), then each subsequent segment's
// method return type (conformance-aware). Returns "" on the first hop it cannot
// type.
func walkChainExprType(g factoryChainLookup, expr string) string {
	parts := strings.Split(stripChainArgs(strings.ReplaceAll(expr, "::", ".")), ".")
	if len(parts) == 0 || strings.TrimSpace(parts[0]) == "" {
		return ""
	}
	currentType := graphFactoryReturnType(g, strings.TrimSpace(parts[0]))
	if currentType == "" {
		if graphHasType(g, strings.TrimSpace(parts[0])) {
			currentType = strings.TrimSpace(parts[0])
		} else {
			return ""
		}
	}
	for i := 1; i < len(parts); i++ {
		seg := strings.TrimSpace(parts[i])
		if seg == "" {
			return ""
		}
		n, _ := findMethodNodeConformant(g, currentType, seg)
		if n == nil {
			return ""
		}
		rt, _ := n.Meta["return_type"].(string)
		if rt == "" {
			return ""
		}
		currentType = rt
	}
	return currentType
}

// stripChainArgs removes call-argument groups from a chain expression so only
// the dotted segment names remain (`New().With(x).Build()` → `New.With.Build`).
func stripChainArgs(expr string) string {
	var b strings.Builder
	depth := 0
	for _, r := range expr {
		switch r {
		case '(', '[', '{':
			depth++
		case ')', ']', '}':
			if depth > 0 {
				depth--
			}
		default:
			if depth == 0 {
				b.WriteRune(r)
			}
		}
	}
	return b.String()
}

// graphFactoryReturnType returns the declared return type of a free function /
// constructor named name (the chain seed). A receiver-less declaration wins
// over a same-named method; ambiguity among free functions drops.
func graphFactoryReturnType(g factoryChainLookup, name string) string {
	fnRT, methodRT := "", ""
	role := func(n *graph.Node) bool {
		if n == nil || n.Kind != graph.KindFunction && n.Kind != graph.KindMethod {
			return false
		}
		rt, _ := n.Meta["return_type"].(string)
		return rt != ""
	}
	for _, n := range g.nodes(name, role) {
		if !role(n) {
			continue
		}
		rt, _ := n.Meta["return_type"].(string)
		if _, hasRecv := n.Meta["receiver"]; hasRecv {
			methodRT = rt
		} else {
			if fnRT != "" && fnRT != rt {
				return "" // ambiguous free function
			}
			fnRT = rt
		}
	}
	if fnRT != "" {
		return fnRT
	}
	return methodRT
}

// graphHasType reports whether the graph holds a type/interface named name.
func graphHasType(g factoryChainLookup, name string) bool {
	role := func(n *graph.Node) bool { return n != nil && isTypeNodeKind(n.Kind) }
	for _, n := range g.nodes(name, role) {
		if role(n) {
			return true
		}
	}
	return false
}

func isTypeNodeKind(k graph.NodeKind) bool {
	return k == graph.KindType || k == graph.KindInterface
}

// resolveMemberByTypeConformant binds member to typeName's method, or — when
// typeName declares it nowhere — to the method on a unique implementor/subtype
// of typeName (the conformance walk via implements/extends edges). The second
// return reports whether a conformance hop was needed.
func resolveMemberByTypeConformant(g factoryChainLookup, typeName, member string) (string, bool) {
	if typeName != "" && member != "" {
		role := func(n *graph.Node) bool { return n != nil && n.Kind == graph.KindMethod && factoryChainMethodRole(n, typeName) }
		if direct := uniqueMemberByType(g.nodes(member, role), typeName); direct != "" {
			return direct, false
		}
	}
	if n, walked := findMethodNodeConformant(g, typeName, member); n != nil && walked {
		return n.ID, true
	}
	return "", false
}

// findMethodNodeConformant returns the method node named member on typeName,
// or — via the implements/extends conformance walk — on a unique subtype /
// implementor of typeName. The second return reports whether the conformance
// walk supplied the match.
func findMethodNodeConformant(g factoryChainLookup, typeName, member string) (*graph.Node, bool) {
	if n := findMethodNodeByType(g, typeName, member); n != nil {
		return n, false
	}
	var match *graph.Node
	role := func(n *graph.Node) bool { return n != nil && isTypeNodeKind(n.Kind) }
	for _, tn := range g.nodes(typeName, role) {
		if !role(tn) {
			continue
		}
		for _, ie := range g.incoming(tn.ID) {
			if ie == nil || (ie.Kind != graph.EdgeImplements && ie.Kind != graph.EdgeExtends) {
				continue
			}
			impl := g.node(ie.From)
			if impl == nil || impl.Name == "" {
				continue
			}
			if n := findMethodNodeByType(g, impl.Name, member); n != nil {
				if match != nil && match.ID != n.ID {
					return nil, true // ambiguous across implementors — drop
				}
				match = n
			}
		}
	}
	return match, match != nil
}

// findMethodNodeByType returns the sole method named member whose
// Meta["receiver"] is typeName, or nil when none or more than one exists.
func findMethodNodeByType(g factoryChainLookup, typeName, member string) *graph.Node {
	var match *graph.Node
	role := func(n *graph.Node) bool { return factoryChainMethodRole(n, typeName) }
	for _, n := range g.nodes(member, role) {
		if !role(n) {
			continue
		}
		if match != nil && match.ID != n.ID {
			return nil
		}
		match = n
	}
	return match
}

func factoryChainMethodRole(n *graph.Node, typeName string) bool {
	if n == nil || n.Kind != graph.KindMethod && n.Kind != graph.KindFunction {
		return false
	}
	recv, _ := n.Meta["receiver"].(string)
	return recv == typeName
}
