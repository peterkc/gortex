package store_pebble

import (
	"strings"
	"testing"

	"github.com/cockroachdb/pebble/v2"
	"github.com/stretchr/testify/require"
	"github.com/zzet/gortex/internal/graph"
)

func TestRebuildObserverPredecessorCustody(t *testing.T) {
	for _, name := range []string{"authentic", "missing direct", "stale receiver", "forged event call"} {
		t.Run(name, func(t *testing.T) {
			const (
				channelFile = "channel.go"
				appFile     = "app.ts"
				topic       = "event::emitter::bus::ready"
			)
			receiver := map[string]any{"receiver": "Bus"}
			registrar := &graph.Node{ID: channelFile + "::Bus.subscribe", Name: "subscribe", Kind: graph.KindMethod,
				FilePath: channelFile, Language: "go", StartLine: 2, Meta: receiver}
			dispatcher := &graph.Node{ID: channelFile + "::Bus.notify", Name: "notify", Kind: graph.KindMethod,
				FilePath: channelFile, Language: "go", StartLine: 8, Meta: receiver}
			field := &graph.Node{ID: channelFile + "::Bus.callbacks", Name: "callbacks", Kind: graph.KindField,
				FilePath: channelFile, Language: "go", Meta: receiver}
			caller := &graph.Node{ID: appFile + "::wire", Name: "wire", Kind: graph.KindFunction, FilePath: appFile, Language: "typescript"}
			onEvent := &graph.Node{ID: appFile + "::onEvent", Name: "onEvent", Kind: graph.KindFunction, FilePath: appFile, Language: "typescript"}
			onPlain := &graph.Node{ID: appFile + "::onPlain", Name: "onPlain", Kind: graph.KindFunction, FilePath: appFile, Language: "typescript"}
			channel := ParseFacts{
				EffectiveLanguage: "go",
				File:              graph.FileMetaRow{FilePath: channelFile, NodeCount: 3},
				Nodes:             []*graph.Node{registrar, dispatcher, field},
				Edges: []*graph.Edge{
					{From: registrar.ID, To: field.ID, Kind: graph.EdgeWrites, FilePath: channelFile, Line: 3},
					{From: dispatcher.ID, To: "unresolved::Lock", Kind: graph.EdgeCalls, FilePath: channelFile, Line: 9,
						Meta: map[string]any{"recv_field": "callbacks"}},
					{From: registrar.ID, To: topic, Kind: graph.EdgeListensOn, FilePath: channelFile, Line: 4},
				},
			}
			app := ParseFacts{
				EffectiveLanguage: "typescript",
				File:              graph.FileMetaRow{FilePath: appFile, NodeCount: 4},
				Nodes: []*graph.Node{
					caller, onEvent, onPlain,
					{ID: topic, Name: "ready", Kind: graph.KindString, FilePath: appFile, Language: "typescript"},
				},
				Edges: []*graph.Edge{
					{From: caller.ID, To: topic, Kind: graph.EdgeEmits, FilePath: appFile, Line: 5},
					{From: caller.ID, To: onEvent.ID, Kind: graph.EdgeReferences, FilePath: appFile, Line: 5},
					{From: caller.ID, To: registrar.ID, Kind: graph.EdgeCalls, FilePath: appFile, Line: 6},
					{From: caller.ID, To: onPlain.ID, Kind: graph.EdgeReads, FilePath: appFile, Line: 6},
				},
			}
			b := newBuild(t, FullProfile)
			var receiptsFacts []ParseFacts
			for _, f := range []ParseFacts{channel, app} {
				b.AddBatch(f.Nodes, f.Edges)
				raw, err := EncodeParseFacts(f)
				require.NoError(t, err)
				receipt, err := DecodeParseFacts(raw)
				require.NoError(t, err)
				receiptsFacts = append(receiptsFacts, receipt)
			}
			receipts := func(yield func(ParseFacts, error) bool) {
				for _, f := range receiptsFacts {
					if !yield(f, nil) {
						return
					}
				}
			}
			ctx := t.Context()
			const spillAt = 1 << 20
			stats, err := b.RebuildCapabilities(ctx, receipts,
				func(string) string { return "" }, func(string, string) (string, string) { return "", "" },
				ReceiverMutationRules{
					OwnerKey:        func(_ string, receiver string) string { return receiver },
					BareCallName:    func(to string) string { return strings.TrimPrefix(to, "unresolved::") },
					IsStdlibMutator: func(name string) bool { return name == "Lock" },
				}, spillAt)
			require.NoError(t, err)
			require.Equal(t, uint64(1), stats.Emitted)

			eventCall := func(from, to, file string, line int) *graph.Edge {
				return &graph.Edge{From: from, To: to, Kind: graph.EdgeCalls, FilePath: file, Line: line,
					Origin: graph.OriginASTInferred, Confidence: 0.5, ConfidenceLabel: graph.ConfidenceLabelFor(graph.EdgeCalls, 0.5),
					Meta: map[string]any{
						"via":             "event.channel",
						"event_topic":     "bus::ready",
						"event_transport": "emitter",
						"synthesized_by":  "event-channel",
						"provenance":      "heuristic",
					},
				}
			}
			eventStats, err := b.RebuildEventChannel(ctx, receipts, spillAt, []string{"event::emitter::"},
				func(string, string) (string, int) { return "emitter", 4 },
				func(_, _ string, emitter, listener *graph.Edge) *graph.Edge {
					return eventCall(emitter.From, listener.From, emitter.FilePath, emitter.Line)
				})
			require.NoError(t, err)
			require.Equal(t, uint64(1), eventStats.Pairs)

			direct := &graph.Edge{From: registrar.ID, To: field.ID, Kind: graph.EdgeAccessesField, FilePath: channelFile, Line: 3,
				Origin: graph.OriginASTResolved, Meta: map[string]any{"access": "write"}}
			indirect := &graph.Edge{From: dispatcher.ID, To: field.ID, Kind: graph.EdgeAccessesField, FilePath: channelFile, Line: 9,
				Origin: graph.OriginASTInferred, Meta: map[string]any{"access": "write", "indirect": true, "via": "Lock"}}
			call := eventCall(caller.ID, registrar.ID, appFile, 5)
			for _, check := range []struct {
				edge  *graph.Edge
				proof rebuildEdgeProof
			}{
				{direct, rebuildProofDirect},
				{indirect, rebuildProofReceiver},
				{call, rebuildProofEventChannel},
			} {
				key := pendingEdgeKey(check.edge)
				live, err := b.pendingEdge(key)
				require.NoError(t, err)
				require.Equal(t, check.edge, live)
				custody, err := readRebuildEdgeCustody(b.pending, key)
				require.NoError(t, err)
				require.Equal(t, rebuildEdgeCustody{proof: check.proof}, custody)
			}
			switch name {
			case "missing direct":
				require.NoError(t, b.pending.Delete(rebuildLinkRecorderKey(pendingEdgeKey(direct)), pebble.NoSync))
			case "stale receiver":
				// Change only the payload; deliberately bypass recorder invalidation.
				indirect.Via = "replacement"
				value, err := encodeEdge(indirect)
				require.NoError(t, err)
				require.NoError(t, b.pending.Set(pendingEdgeKey(indirect), value, pebble.NoSync))
			case "forged event call":
				// An ordinary replacement can reproduce the producer's exact tags and bytes.
				require.NoError(t, b.addEdge(call))
				got, err := readRebuildEdgeCustody(b.pending, pendingEdgeKey(call))
				require.NoError(t, err)
				require.Zero(t, got)
			}

			actualRows := func() map[string]string {
				rows := make(map[string]string)
				for _, family := range []byte{familyNode, familyEdge} {
					require.NoError(t, pendingVisit(ctx, b.pending, []byte{family}, func(r row) error {
						rows[string(r.key)] = string(r.value)
						return nil
					}))
				}
				return rows
			}
			makeCall := func(from *graph.Node, to, field string) *graph.Edge {
				return &graph.Edge{From: from.ID, To: to, Kind: graph.EdgeCalls, FilePath: from.FilePath, Line: from.StartLine,
					Origin: graph.OriginASTInferred, Confidence: 0.5, ConfidenceLabel: graph.ConfidenceLabelFor(graph.EdgeCalls, 0.5),
					Meta: map[string]any{
						"via": "observer.channel", "channel_field": field, "synthesized_by": "observer-channel", "provenance": "heuristic",
					},
				}
			}
			want := actualRows()
			observerStats, err := b.RebuildObserver(ctx, receipts, spillAt,
				func(name string) bool { return name == "subscribe" },
				func(name string) bool { return name == "notify" },
				func(from, to *graph.Node) bool {
					return from != nil && to != nil && from.WorkspaceID == to.WorkspaceID
				}, 40, makeCall)
			if name != "authentic" {
				require.ErrorIs(t, err, ErrRebuildUnknownProvenance)
				require.Zero(t, observerStats)
				require.Equal(t, want, actualRows())
				return
			}
			require.NoError(t, err)
			require.Equal(t, uint64(2), observerStats.Edges)
			for _, to := range []string{onEvent.ID, onPlain.ID} {
				e := makeCall(dispatcher, to, field.ID)
				key := pendingEdgeKey(e)
				raw, err := encodeEdge(e)
				require.NoError(t, err)
				want[string(key)] = string(raw)
				want[string(pendingReverseKey(e, key))] = string(key)
				custody, err := readRebuildEdgeCustody(b.pending, key)
				require.NoError(t, err)
				require.Equal(t, rebuildEdgeCustody{proof: rebuildProofObserver}, custody)
			}
			require.Equal(t, want, actualRows())
		})
	}
}
