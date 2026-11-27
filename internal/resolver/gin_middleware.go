package resolver

import (
	"sort"

	"github.com/zzet/gortex/internal/graph"
)

// ginMiddlewareVia tags the synthesized dispatcher→handler edges.
const ginMiddlewareVia = "gin_middleware_chain"

// ginFanoutCap bounds the dispatcher×handler fan-out so a pathological repo
// (a custom chain plus hundreds of registered handlers) cannot explode the
// edge set.
const ginFanoutCap = 512

// ResolveGinMiddlewareCalls bridges a Gin middleware-chain dispatcher to the
// handlers it dispatches to. The Go extractor stamps gin_dispatcher=true on the
// method that invokes `c.handlers[idx](c)` (the indexed-slice indirection a
// call graph drops) and gin_handlers=[names] on each function that registers
// routes/middleware with `.GET`/`.Use`/`.Handle`. This pass resolves those
// names to their definitions and emits a tier-tagged dispatcher→handler call
// edge per pair, so request→handler reachability flows through get_call_chain
// where a same-file scanner sees a dead end. Gated on a dispatcher existing
// (so it is inert outside a Gin-style chain) and repo-scoped via
// sameDispatchBoundary (per the intra-process dispatch discipline).
func ResolveGinMiddlewareCalls(g graph.Store) int {
	if g == nil {
		return 0
	}

	var dispatchers []*graph.Node
	var registrars []*graph.Node
	nameIndex := map[string][]*graph.Node{}
	for _, n := range nodesByKindsOrAll(g, graph.KindMethod, graph.KindFunction) {
		if n == nil {
			continue
		}
		nameIndex[n.Name] = append(nameIndex[n.Name], n)
		if n.Meta == nil {
			continue
		}
		if ginMiddlewareDispatcher(n) {
			dispatchers = append(dispatchers, n)
		}
		if _, ok := n.Meta["gin_handlers"]; ok {
			registrars = append(registrars, n)
		}
	}
	// Gated on the dispatcher existing: no chain dispatcher in the graph means
	// no Gin-style indirection to bridge.
	if len(dispatchers) == 0 || len(registrars) == 0 {
		return 0
	}

	// Collect the distinct handler names registered anywhere, deterministically.
	handlerNames := map[string]bool{}
	for _, r := range registrars {
		for _, name := range ginHandlerNames(r.Meta["gin_handlers"]) {
			handlerNames[name] = true
		}
	}
	names := make([]string, 0, len(handlerNames))
	for name := range handlerNames {
		names = append(names, name)
	}
	sort.Strings(names)

	sort.Slice(dispatchers, func(i, j int) bool { return dispatchers[i].ID < dispatchers[j].ID })

	var batch []*graph.Edge
	seen := map[string]bool{}
	for _, d := range dispatchers {
		for _, name := range names {
			cands := nameIndex[name]
			sort.Slice(cands, func(i, j int) bool { return cands[i].ID < cands[j].ID })
			for _, h := range cands {
				if !ginMiddlewareTarget(d, h) {
					continue
				}
				key := d.ID + "\x00" + h.ID
				if seen[key] {
					continue
				}
				seen[key] = true
				batch = append(batch, ginMiddlewareEdge(d, h))
				if len(batch) >= ginFanoutCap {
					break
				}
			}
		}
	}

	for _, e := range batch {
		g.AddEdge(e)
	}
	return len(batch)
}

func ginMiddlewareDispatcher(n *graph.Node) bool {
	if n == nil {
		return false
	}
	dispatcher, _ := n.Meta["gin_dispatcher"].(bool)
	return dispatcher
}

// A dispatcher only reaches handlers in its own dispatch boundary.
func ginMiddlewareTarget(dispatcher, handler *graph.Node) bool {
	return handler != nil && handler.ID != dispatcher.ID && sameDispatchBoundary(dispatcher, handler)
}

// ginHandlerNames coerces the gin_handlers Meta value (stamped as []string,
// possibly []any after a serialization round-trip) into a name slice.
func ginHandlerNames(v any) []string {
	switch t := v.(type) {
	case []string:
		return t
	case []any:
		out := make([]string, 0, len(t))
		for _, e := range t {
			if s, ok := e.(string); ok && s != "" {
				out = append(out, s)
			}
		}
		return out
	}
	return nil
}

// ginMiddlewareEdge builds one dispatcher→handler speculative call edge.
func ginMiddlewareEdge(from, to *graph.Node) *graph.Edge {
	return &graph.Edge{
		From:            from.ID,
		To:              to.ID,
		Kind:            graph.EdgeCalls,
		FilePath:        from.FilePath,
		Line:            from.StartLine,
		Confidence:      0.4,
		ConfidenceLabel: graph.ConfidenceLabelFor(graph.EdgeCalls, 0.4),
		Origin:          graph.OriginSpeculative,
		Meta: map[string]any{
			"via":             ginMiddlewareVia,
			"speculative":     true,
			MetaSynthesizedBy: SynthGinMiddleware,
			MetaProvenance:    ProvenanceHeuristic,
		},
	}
}
