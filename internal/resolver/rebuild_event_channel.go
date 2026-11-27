package resolver

import (
	"context"
	"iter"

	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/graph/store_pebble"
)

// RunRebuildEventChannel executes registry position three. The worker has
// already applied the private gRPC and Temporal turns.
func RunRebuildEventChannel(ctx context.Context, b *store_pebble.BuildStore, receipts iter.Seq2[store_pebble.ParseFacts, error],
	selection FrameworkSynthesizerSelection, spillAt int,
) (store_pebble.RebuildEventChannelStats, error) {
	if !selection.allows(SynthEventChannel) {
		return store_pebble.RebuildEventChannelStats{}, nil
	}
	prefixes := []string{"event::pubsub::", "event::emitter::"}
	return b.RebuildEventChannel(ctx, receipts, spillAt, prefixes, func(event, transport string) (string, int) {
		if transport == "" {
			transport = transportFromEventID(event)
		}
		if !eventChannelInProcess(transport) {
			return transport, 0
		}
		if transport == emitterLiteralTransport {
			return transport, emitterLiteralFanoutCap
		}
		return transport, maxEventChannelFanout
	}, func(event, transport string, emitter, listener *graph.Edge) *graph.Edge {
		return &graph.Edge{
			From:            emitter.From,
			To:              listener.From,
			Kind:            graph.EdgeCalls,
			FilePath:        emitter.FilePath,
			Line:            emitter.Line,
			Confidence:      0.5,
			ConfidenceLabel: graph.ConfidenceLabelFor(graph.EdgeCalls, 0.5),
			Origin:          graph.OriginASTInferred,
			Meta: map[string]any{
				"via":             eventChannelVia,
				"event_topic":     topicFromEventID(event, transport),
				"event_transport": transport,
				MetaSynthesizedBy: SynthEventChannel,
				MetaProvenance:    ProvenanceHeuristic,
			},
		}
	})
}
