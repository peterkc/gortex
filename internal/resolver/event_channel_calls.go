package resolver

import (
	"sort"
	"strings"

	"github.com/zzet/gortex/internal/graph"
)

// eventChannelVia is the Edge.Meta["via"] marker on a synthesized
// event-channel call edge.
const eventChannelVia = "event.channel"

// maxEventChannelFanout caps the emitter/listener count a single topic
// may have before the pairing is skipped. An in-process channel with
// dozens of emitters or listeners is almost always a generic logging /
// telemetry bus where an emitter→listener call edge per pair would be
// pure noise (and quadratic). Real domain event channels pair a handful
// of publishers with a handful of handlers.
const maxEventChannelFanout = 32

// emitterLiteralTransport labels the fallback emitter-literal channel:
// a bare `recv.on('event', handler)` / `recv.emit('event')` pair the
// import-gated pub/sub extractor does not recognise (no `events` /
// `eventemitter` import), keyed only by the receiver scope + event-name
// string literal. Its topic node ID is event::emitter::<recv>::<literal>.
const emitterLiteralTransport = "emitter"

// emitterLiteralFanoutCap is the tighter fan-out cap applied to the
// emitter-literal channel. A topic-node-backed pub/sub pair carries
// transport + import evidence and may fan out to maxEventChannelFanout;
// an emitter-literal pair is correlated by nothing but a bare string, so
// it gets a much tighter cap to keep a common literal ("data", "error")
// from fanning a publisher out to every unrelated same-named listener.
const emitterLiteralFanoutCap = 6

// ResolveEventChannelCalls is the framework-dispatch synthesizer for
// in-process and cross-language event channels. The pub/sub extractor
// already materialises a shared KindEvent topic node per (transport,
// topic) pair, with EdgeEmits from every publishing function and
// EdgeListensOn from every subscribing function. Message brokers
// (Kafka / NATS / RabbitMQ / Redis) get their producer↔consumer pairing
// from the contracts layer (EdgeProducesTopic / EdgeConsumesTopic), so
// this pass deliberately covers only the channels the contracts layer
// does not: in-process emitters (Node EventEmitter, Socket.IO) and the
// native cross-language bridges (React Native's NativeEventEmitter,
// where a Swift/ObjC/Kotlin `sendEvent` is handled by a JS
// `addListener`). For each such topic it synthesizes a `calls` edge from
// each emitting function to each listening function — the runtime
// dispatch the static call graph cannot see ("who actually runs when
// this event fires?").
//
// Full recompute and idempotent: every edge is re-derived from the emit
// / listen edges, graph.AddEdge dedupes by edge key, and graph.EvictFile
// drops the synthesized edge in both directions when either endpoint's
// file is reindexed — so a removed emitter or listener cannot leave a
// dangling edge. Edges ride at ast_inferred (the pairing is a name-keyed
// heuristic, not a typed dispatch) and carry full provenance.
//
// Returns the number of event-channel call edges synthesized this pass.
func ResolveEventChannelCalls(g graph.Store) int {
	if g == nil {
		return 0
	}

	type emitSite struct {
		from      string
		filePath  string
		line      int
		transport string
	}
	emittersByEvent := map[string][]emitSite{}
	for e := range g.EdgesByKind(graph.EdgeEmits) {
		if e == nil || e.To == "" || e.From == "" {
			continue
		}
		if !isPubsubEventNode(e.To) && !isEmitterEventNode(e.To) {
			continue
		}
		emittersByEvent[e.To] = append(emittersByEvent[e.To], emitSite{
			from:      e.From,
			filePath:  e.FilePath,
			line:      e.Line,
			transport: edgeTransport(e),
		})
	}
	if len(emittersByEvent) == 0 {
		return 0
	}

	listenersByEvent := map[string][]string{}
	for e := range graph.EdgesLightSeq(g, graph.EdgeListensOn) {
		if e == nil || e.To == "" || e.From == "" {
			continue
		}
		if _, ok := emittersByEvent[e.To]; !ok {
			continue
		}
		listenersByEvent[e.To] = append(listenersByEvent[e.To], e.From)
	}

	var batch []*graph.Edge
	synthesized := 0
	// Stable iteration so a re-run produces edges in the same order
	// (Date/rand are unavailable in this layer anyway; this keeps logs
	// and any downstream ordering deterministic).
	eventIDs := make([]string, 0, len(emittersByEvent))
	for id := range emittersByEvent {
		eventIDs = append(eventIDs, id)
	}
	sort.Strings(eventIDs)

	for _, eventID := range eventIDs {
		emitters := emittersByEvent[eventID]
		listeners := listenersByEvent[eventID]
		if len(listeners) == 0 {
			continue
		}
		// Only pair channels the contracts broker-pairing layer ignores.
		transport := ""
		for _, em := range emitters {
			if em.transport != "" {
				transport = em.transport
				break
			}
		}
		if transport == "" {
			transport = transportFromEventID(eventID)
		}
		if !eventChannelInProcess(transport) {
			continue
		}
		fanoutCap := maxEventChannelFanout
		if transport == emitterLiteralTransport {
			fanoutCap = emitterLiteralFanoutCap
		}
		if len(emitters) > fanoutCap || len(listeners) > fanoutCap {
			continue
		}
		topic := topicFromEventID(eventID, transport)

		// Dedupe (from→to) pairs, keeping the emit site with the lowest
		// line as the representative so the edge key is stable across runs
		// even when a function emits the same event from several lines.
		type pairKey struct{ from, to string }
		rep := map[pairKey]emitSite{}
		for _, em := range emitters {
			for _, to := range listeners {
				if em.from == "" || to == "" || em.from == to {
					continue
				}
				if !sameDispatchBoundaryIDs(g, em.from, to) {
					continue
				}
				k := pairKey{from: em.from, to: to}
				if cur, ok := rep[k]; !ok || em.line < cur.line {
					rep[k] = em
				}
			}
		}
		for k, em := range rep {
			batch = append(batch, &graph.Edge{
				From:            k.from,
				To:              k.to,
				Kind:            graph.EdgeCalls,
				FilePath:        em.filePath,
				Line:            em.line,
				Confidence:      0.5,
				ConfidenceLabel: graph.ConfidenceLabelFor(graph.EdgeCalls, 0.5),
				Origin:          graph.OriginASTInferred,
				Meta: map[string]any{
					"via":             eventChannelVia,
					"event_topic":     topic,
					"event_transport": transport,
					MetaSynthesizedBy: SynthEventChannel,
					MetaProvenance:    ProvenanceHeuristic,
				},
			})
			synthesized++
		}
	}

	for _, e := range batch {
		g.AddEdge(e)
	}
	return synthesized
}

const (
	pubsubEventIDPrefix  = "event::pubsub::"
	emitterEventIDPrefix = "event::emitter::"
)

// isPubsubEventNode reports whether an ID is a pub/sub KindEvent topic
// node (event::pubsub::<transport>::<topic>).
func isPubsubEventNode(id string) bool {
	return strings.HasPrefix(id, pubsubEventIDPrefix)
}

// isEmitterEventNode reports whether an ID is an emitter-literal KindEvent
// topic node (event::emitter::<receiver-scope>::<literal>) — the fallback
// channel for the in-process `.on` / `.emit` pairs the import-gated
// pub/sub extractor does not recognise.
func isEmitterEventNode(id string) bool {
	return strings.HasPrefix(id, emitterEventIDPrefix)
}

// edgeTransport reads the transport label an emit/listen edge carries.
func edgeTransport(e *graph.Edge) string {
	if e == nil || e.Meta == nil {
		return ""
	}
	t, _ := e.Meta["transport"].(string)
	return t
}

// transportFromEventID recovers the transport segment of an event node
// ID when the edge Meta did not carry it. Emitter-literal nodes carry no
// transport segment in their ID — the transport is implicitly "emitter".
func transportFromEventID(id string) string {
	if isEmitterEventNode(id) {
		return emitterLiteralTransport
	}
	rest := strings.TrimPrefix(id, pubsubEventIDPrefix)
	if t, _, ok := strings.Cut(rest, "::"); ok {
		return t
	}
	return ""
}

// topicFromEventID recovers the topic segment of an event node ID. For an
// emitter-literal node (event::emitter::<recv>::<literal>) the topic is
// the receiver-scoped literal; for a pub/sub node it is the segment after
// the transport.
func topicFromEventID(id, transport string) string {
	if isEmitterEventNode(id) {
		return strings.TrimPrefix(id, emitterEventIDPrefix)
	}
	return strings.TrimPrefix(id, pubsubEventIDPrefix+transport+"::")
}

// eventChannelInProcess reports whether a transport is an in-process or
// native-bridge channel this pass should pair — i.e. one the contracts
// broker-pairing layer (Kafka / NATS / RabbitMQ / Redis) does not handle.
func eventChannelInProcess(transport string) bool {
	switch transport {
	case "eventemitter", "socketio", emitterLiteralTransport:
		return true
	}
	// Native cross-language bridges register their event channel under an
	// "rn_*" / "native*" transport so a native `sendEvent` pairs with the
	// JS `addListener` handler.
	return strings.HasPrefix(transport, "rn_") ||
		strings.HasPrefix(transport, "native") ||
		strings.HasPrefix(transport, "rn-")
}
