package resolver

import (
	"sort"
	"strings"

	"github.com/zzet/gortex/internal/graph"
)

// laravelEventVia is the Meta["via"] tag the PHP extractor stamps on a
// Laravel event-dispatch placeholder.
const laravelEventVia = "laravel-event"

// ResolveLaravelEventCalls binds Laravel event dispatches to their
// listeners' handle methods by event type, from two listener sources: a
// typed `handle(OrderShipped $e)` under a Listeners namespace, and the
// `$listen` map of an EventServiceProvider (encoded on the provider class
// node). A dispatch fans out to every matching listener. Type-keyed, so
// edges land at the typed framework tier.
//
// Returns the number of publisher → listener edges synthesized.
func ResolveLaravelEventCalls(g graph.Store) int {
	if g == nil {
		return 0
	}
	// handle methods indexed by their owning class simple name.
	handleByClass := map[string][]*graph.Node{}
	listenersByType := map[string][]*graph.Node{}
	classByMethod := map[string]string{}
	for e := range graph.EdgesLightSeq(g, graph.EdgeMemberOf) {
		if e != nil && e.From != "" && e.To != "" {
			classByMethod[e.From] = laravelSimpleName(e.To)
		}
	}
	var listenMaps []string
	for _, n := range nodesByKindsOrAll(g, graph.KindMethod, graph.KindFunction, graph.KindType) {
		if n == nil {
			continue
		}
		if n.Kind == graph.KindType {
			if m := laravelListenMap(n); m != "" {
				listenMaps = append(listenMaps, m)
			}
			continue
		}
		if n.Name == "handle" {
			handleByClass[classByMethod[n.ID]] = append(handleByClass[classByMethod[n.ID]], n)
		}
		if t := laravelListenerType(n); t != "" {
			name := laravelSimpleName(t)
			listenersByType[name] = append(listenersByType[name], n)
		}
	}
	// Source 2: fold the $listen maps into listenersByType via handleByClass.
	for _, m := range listenMaps {
		laravelListenEntries(m, func(event, class string) {
			listenersByType[event] = append(listenersByType[event], handleByClass[class]...)
		})
	}
	if len(listenersByType) == 0 {
		return 0
	}

	resolved := 0
	var reindex []graph.EdgeReindex
	var batch []*graph.Edge
	for e := range g.EdgesByKind(graph.EdgeCalls) {
		evType := laravelCallType(e)
		if evType == "" {
			continue
		}
		listeners := laravelDedupSorted(listenersByType[laravelSimpleName(evType)])
		if len(listeners) == 0 {
			resolved += laravelRebind(e, nil, &reindex)
			continue
		}
		resolved += laravelRebind(e, listeners[0], &reindex)
		for _, l := range listeners[1:] {
			batch = append(batch, laravelFanoutEdge(e, l.ID, evType))
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

func laravelListenMap(n *graph.Node) string {
	m, _ := n.Meta["laravel_listen_map"].(string)
	return m
}

func laravelListenerType(n *graph.Node) string {
	t, _ := n.Meta["laravel_listener_type"].(string)
	return t
}

func laravelListenEntries(listenMap string, visit func(event, class string)) {
	for _, entry := range strings.Split(listenMap, ";") {
		event, rest, ok := strings.Cut(entry, "=>")
		if !ok {
			continue
		}
		event = laravelSimpleName(strings.TrimSpace(event))
		for _, class := range strings.Split(rest, ",") {
			visit(event, laravelSimpleName(strings.TrimSpace(class)))
		}
	}
}

func laravelCallType(e *graph.Edge) string {
	if e == nil {
		return ""
	}
	if v, _ := e.Meta["via"].(string); v != laravelEventVia {
		return ""
	}
	evType, _ := e.Meta["laravel_event_type"].(string)
	return evType
}

func laravelRebind(e *graph.Edge, target *graph.Node, reindex *[]graph.EdgeReindex) int {
	var id string
	hit := 0
	if target != nil {
		id, hit = target.ID, 1
	}
	oldTo := e.To
	if rebindLaravelEventCall(e, id, target != nil) {
		*reindex = append(*reindex, graph.EdgeReindex{Edge: e, OldTo: oldTo})
	}
	return hit
}

func rebindLaravelEventCall(e *graph.Edge, target string, matched bool) bool {
	if !matched {
		target = "unresolved::*.handle"
	}
	if e.To == target {
		return false
	}
	e.To = target
	e.Origin = graph.OriginASTInferred
	if matched {
		e.Confidence = ConfidenceTyped
		e.ConfidenceLabel = graph.ConfidenceLabelFor(graph.EdgeCalls, ConfidenceTyped)
		StampSynthesizedTyped(e, SynthLaravelEvent)
	} else {
		e.Confidence = 0
		e.ConfidenceLabel = ""
		UnstampSynthesized(e)
	}
	return true
}

func laravelFanoutEdge(e *graph.Edge, listener, evType string) *graph.Edge {
	return &graph.Edge{
		From: e.From, To: listener, Kind: graph.EdgeCalls,
		FilePath: e.FilePath, Line: e.Line,
		Origin:          graph.OriginASTInferred,
		Confidence:      ConfidenceTyped,
		ConfidenceLabel: graph.ConfidenceLabelFor(graph.EdgeCalls, ConfidenceTyped),
		Meta: map[string]any{
			"via":                laravelEventVia,
			"laravel_event_type": evType,
			MetaSynthesizedBy:    SynthLaravelEvent,
			MetaProvenance:       ProvenanceFramework,
		},
	}
}

// laravelDedupSorted dedups listener nodes by ID and sorts for deterministic
// fan-out.
func laravelDedupSorted(in []*graph.Node) []*graph.Node {
	seen := map[string]bool{}
	out := make([]*graph.Node, 0, len(in))
	for _, n := range in {
		if n != nil && !seen[n.ID] {
			seen[n.ID] = true
			out = append(out, n)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

// laravelSimpleName returns the last segment of a `\`- or `::`-qualified PHP
// name (or a node ID's symbol part).
func laravelSimpleName(s string) string {
	if i := strings.LastIndex(s, "::"); i >= 0 {
		s = s[i+2:]
	}
	if i := strings.LastIndexByte(s, '\\'); i >= 0 {
		s = s[i+1:]
	}
	if i := strings.LastIndexByte(s, '.'); i >= 0 {
		s = s[i+1:]
	}
	return strings.TrimSpace(s)
}
