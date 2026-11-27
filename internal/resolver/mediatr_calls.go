package resolver

import (
	"sort"

	"github.com/zzet/gortex/internal/graph"
)

// mediatrVia is the Meta["via"] tag the C# extractor stamps on a MediatR
// Send/Publish placeholder.
const mediatrVia = "mediatr-dispatch"

// ResolveMediatRCalls binds .NET MediatR dispatches to their handler's
// Handle method by request type: `_mediator.Send(new CreateOrder())` →
// `CreateOrderHandler.Handle` (one handler), and `_bus.Publish(new
// OrderPlaced())` → every `INotificationHandler<OrderPlaced>` (fan-out).
// Type-keyed via the handler base list, so edges land at the typed
// framework tier.
//
// Returns the number of caller → handler edges synthesized.
func ResolveMediatRCalls(g graph.Store) int {
	if g == nil {
		return 0
	}
	// index: kind → request type → handler Handle methods.
	index := map[string]map[string][]*graph.Node{"request": {}, "notification": {}}
	for _, n := range nodesByKindsOrAll(g, graph.KindMethod, graph.KindFunction) {
		kind, reqType := mediatrHandlerKey(n)
		if kind == "" {
			continue
		}
		index[kind][reqType] = append(index[kind][reqType], n)
	}
	if len(index["request"]) == 0 && len(index["notification"]) == 0 {
		return 0
	}

	resolved := 0
	var reindex []graph.EdgeReindex
	var batch []*graph.Edge
	for e := range g.EdgesByKind(graph.EdgeCalls) {
		kind, reqType := mediatrCallKey(e)
		if reqType == "" {
			continue
		}
		cands := index[kind][reqType]

		if kind == "request" {
			// A request has a single handler; never guess on ambiguity.
			target := pickStoreAction(g, e, sameBoundaryCandidates(g, e.From, cands))
			resolved += mediatrRebind(e, target, &reindex)
			continue
		}

		// A notification fans out to every handler.
		handlers := sameBoundaryCandidates(g, e.From, cands)
		sort.Slice(handlers, func(i, j int) bool { return handlers[i].ID < handlers[j].ID })
		if len(handlers) == 0 {
			resolved += mediatrRebind(e, nil, &reindex)
			continue
		}
		resolved += mediatrRebind(e, handlers[0], &reindex)
		for _, h := range handlers[1:] {
			batch = append(batch, mediatrFanoutEdge(e, h.ID, reqType))
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

func mediatrRouteKey(meta map[string]any) (kind, reqType string) {
	kind, _ = meta["mediatr_kind"].(string)
	if kind != "request" && kind != "notification" {
		return "", ""
	}
	reqType, _ = meta["mediatr_request_type"].(string)
	return kind, reqType
}

func mediatrHandlerKey(n *graph.Node) (kind, reqType string) {
	if n == nil {
		return "", ""
	}
	kind, reqType = mediatrRouteKey(n.Meta)
	if reqType == "" {
		return "", ""
	}
	return kind, reqType
}

func mediatrCallKey(e *graph.Edge) (kind, reqType string) {
	if e == nil {
		return "", ""
	}
	if v, _ := e.Meta["via"].(string); v != mediatrVia {
		return "", ""
	}
	return mediatrRouteKey(e.Meta)
}

// mediatrRebind returns one for a hit, even if the Call already targets it.
func mediatrRebind(e *graph.Edge, target *graph.Node, reindex *[]graph.EdgeReindex) int {
	var id string
	hit := 0
	if target != nil {
		id, hit = target.ID, 1
	}
	oldTo := e.To
	if rebindMediatRCall(e, id, target != nil) {
		*reindex = append(*reindex, graph.EdgeReindex{Edge: e, OldTo: oldTo})
	}
	return hit
}

func rebindMediatRCall(e *graph.Edge, target string, matched bool) bool {
	if !matched {
		target = "unresolved::*.Handle"
	}
	if e.To == target {
		return false
	}
	e.To = target
	e.Origin = graph.OriginASTInferred
	if matched {
		e.Confidence = ConfidenceTyped
		e.ConfidenceLabel = graph.ConfidenceLabelFor(graph.EdgeCalls, ConfidenceTyped)
		StampSynthesizedTyped(e, SynthMediatR)
	} else {
		e.Confidence = 0
		e.ConfidenceLabel = ""
		UnstampSynthesized(e)
	}
	return true
}

func mediatrFanoutEdge(e *graph.Edge, handler, reqType string) *graph.Edge {
	return &graph.Edge{
		From: e.From, To: handler, Kind: graph.EdgeCalls,
		FilePath: e.FilePath, Line: e.Line,
		Origin:          graph.OriginASTInferred,
		Confidence:      ConfidenceTyped,
		ConfidenceLabel: graph.ConfidenceLabelFor(graph.EdgeCalls, ConfidenceTyped),
		Meta: map[string]any{
			"via":                  mediatrVia,
			"mediatr_request_type": reqType,
			"mediatr_kind":         "notification",
			MetaSynthesizedBy:      SynthMediatR,
			MetaProvenance:         ProvenanceFramework,
		},
	}
}
