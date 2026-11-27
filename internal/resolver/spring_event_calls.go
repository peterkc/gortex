package resolver

import (
	"sort"
	"strings"

	"github.com/zzet/gortex/internal/graph"
)

// springEventVia is the Meta["via"] tag the Java extractor stamps on a
// Spring publishEvent placeholder.
const springEventVia = "spring-event"

// ResolveSpringEventCalls binds Spring application-event publishers to their
// listeners by event type: a `publishEvent(new OrderPlaced(...))` reaches
// every `@EventListener void on(OrderPlaced e)` and
// `ApplicationListener<OrderPlaced>.onApplicationEvent`. Listeners for a
// base event type also receive a published subtype when the type hierarchy
// edge is present. Type-keyed, so edges land at the typed framework tier.
//
// Returns the number of publisher → listener edges synthesized.
func ResolveSpringEventCalls(g graph.Store) int {
	if g == nil {
		return 0
	}
	listenersByType := map[string][]*graph.Node{}
	for _, n := range nodesByKindsOrAll(g, graph.KindMethod, graph.KindFunction) {
		if t := springListenerType(n); t != "" {
			listenersByType[springSimpleType(t)] = append(listenersByType[springSimpleType(t)], n)
		}
	}
	if len(listenersByType) == 0 {
		return 0
	}
	parents := springTypeParents(g)

	resolved := 0
	var reindex []graph.EdgeReindex
	var batch []*graph.Edge
	for e := range g.EdgesByKind(graph.EdgeCalls) {
		evType := springEventType(e)
		if evType == "" {
			continue
		}
		listeners := springListenersFor(springSimpleType(evType), listenersByType, parents)

		oldTo := e.To
		if len(listeners) == 0 {
			if rebindSpringEvent(e, evType, "", false) {
				reindex = append(reindex, graph.EdgeReindex{Edge: e, OldTo: oldTo})
			}
			continue
		}

		// First listener reuses the placeholder edge; the rest fan out.
		if rebindSpringEvent(e, evType, listeners[0].ID, true) {
			reindex = append(reindex, graph.EdgeReindex{Edge: e, OldTo: oldTo})
		}
		resolved++
		for _, l := range listeners[1:] {
			batch = append(batch, springEventFanout(e, evType, l.ID))
			resolved++
		}
	}
	if len(reindex) > 0 {
		g.ReindexEdges(reindex)
	}
	for _, ne := range batch {
		g.AddEdge(ne)
	}
	return resolved
}

func springListenerType(n *graph.Node) string {
	if n == nil {
		return ""
	}
	t, _ := n.Meta["spring_listener_type"].(string)
	return t
}

func springEventType(e *graph.Edge) string {
	if e == nil {
		return ""
	}
	if via, _ := e.Meta["via"].(string); via != springEventVia {
		return ""
	}
	t, _ := e.Meta["spring_event_type"].(string)
	return t
}

// rebindSpringEvent returns true only when the target changes and needs reindexing.
func rebindSpringEvent(e *graph.Edge, evType, listener string, matched bool) bool {
	if !matched {
		if graph.IsUnresolvedTarget(e.To) {
			return false
		}
		e.To = "unresolved::*." + evType
		e.Confidence = 0
		e.ConfidenceLabel = ""
		UnstampSynthesized(e)
		return true
	}
	if e.To == listener {
		return false
	}
	e.To = listener
	e.Origin = graph.OriginASTInferred
	e.Confidence = ConfidenceTyped
	e.ConfidenceLabel = graph.ConfidenceLabelFor(graph.EdgeCalls, ConfidenceTyped)
	StampSynthesizedTyped(e, SynthSpringEvent)
	return true
}

func springEventFanout(e *graph.Edge, evType, listener string) *graph.Edge {
	return &graph.Edge{
		From: e.From, To: listener, Kind: graph.EdgeCalls,
		FilePath: e.FilePath, Line: e.Line,
		Origin:          graph.OriginASTInferred,
		Confidence:      ConfidenceTyped,
		ConfidenceLabel: graph.ConfidenceLabelFor(graph.EdgeCalls, ConfidenceTyped),
		Meta: map[string]any{
			"via":               springEventVia,
			"spring_event_type": evType,
			MetaSynthesizedBy:   SynthSpringEvent,
			MetaProvenance:      ProvenanceFramework,
		},
	}
}

// springListenersFor returns the listener methods for an event type and its
// ancestors, deduped and sorted by node ID for deterministic fan-out.
func springListenersFor(evType string, byType map[string][]*graph.Node, parents map[string][]string) []*graph.Node {
	seenType := map[string]bool{}
	seenNode := map[string]bool{}
	var out []*graph.Node
	queue := []string{evType}
	for len(queue) > 0 {
		t := queue[0]
		queue = queue[1:]
		if seenType[t] {
			continue
		}
		seenType[t] = true
		for _, n := range byType[t] {
			if n != nil && !seenNode[n.ID] {
				seenNode[n.ID] = true
				out = append(out, n)
			}
		}
		queue = append(queue, parents[t]...)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

// springTypeParents builds a simple-name child → parent-type map from the
// graph's extends/implements edges, so a published subtype can reach a
// base-type listener.
func springTypeParents(g graph.Store) map[string][]string {
	parents := map[string][]string{}
	add := func(kind graph.EdgeKind) {
		for e := range graph.EdgesLightSeq(g, kind) {
			if e == nil || e.From == "" || e.To == "" {
				continue
			}
			child := springSimpleType(e.From)
			parent := springSimpleType(e.To)
			if child != "" && parent != "" && child != parent {
				parents[child] = append(parents[child], parent)
			}
		}
	}
	add(graph.EdgeExtends)
	add(graph.EdgeImplements)
	return parents
}

// springSimpleType reduces a node ID / type ref to its simple type name:
// strips the unresolved marker, any `::` / `.` qualifier, and generics.
func springSimpleType(s string) string {
	if graph.IsUnresolvedTarget(s) {
		s = graph.UnresolvedName(s)
	}
	if i := strings.LastIndex(s, "::"); i >= 0 {
		s = s[i+2:]
	}
	if i := strings.IndexByte(s, '<'); i >= 0 {
		s = s[:i]
	}
	if i := strings.LastIndexByte(s, '.'); i >= 0 {
		s = s[i+1:]
	}
	return strings.TrimSpace(s)
}
