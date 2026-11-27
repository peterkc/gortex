package resolver

import (
	"errors"
	"fmt"
	"slices"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/zeebo/blake3"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/graph/store_pebble"
	"github.com/zzet/gortex/internal/parser/languages"
)

func TestRebuildLaravelEventSQLiteOracle(t *testing.T) {
	const file = "app.php"
	const source = `<?php
namespace App\Listeners;
class TypedNotice {
  public function handle(OrderShipped $event) {}
}
class SendEmail {
  public function handle($event) {}
}
class Ordinary {
  public function handle($value) {}
}
class EventServiceProvider {
  protected $listen = [
    OrderShipped::class => [SendEmail::class, SendEmail::class],
  ];
}
class OrderController {
  public function ship($order) {
    event(new OrderShipped($order));
  }
}
`
	extractor := languages.NewPHPExtractor()
	raw := []byte(source)
	parsed, err := extractor.Extract(file, raw)
	require.NoError(t, err)
	t.Cleanup(parsed.ReleaseTree)
	primary := file + "::SendEmail.handle"
	typed := file + "::TypedNotice.handle"
	for _, id := range []string{primary, typed, file + "::Ordinary.handle", file + "::EventServiceProvider"} {
		i := slices.IndexFunc(parsed.Nodes, func(n *graph.Node) bool { return n.ID == id })
		require.GreaterOrEqual(t, i, 0, "missing PHP declaration %q", id)
		switch id {
		case typed:
			require.Equal(t, "OrderShipped", parsed.Nodes[i].Meta["laravel_listener_type"])
		case file + "::EventServiceProvider":
			require.Equal(t, "OrderShipped=>SendEmail,SendEmail", parsed.Nodes[i].Meta["laravel_listen_map"])
		default:
			require.NotContains(t, parsed.Nodes[i].Meta, "laravel_listener_type")
		}
	}
	i := slices.IndexFunc(parsed.Edges, func(e *graph.Edge) bool { return e.Meta["via"] == laravelEventVia })
	require.GreaterOrEqual(t, i, 0, "PHP extractor must emit the dispatch")
	parsed.Edges[i].Meta["witness"] = "original dispatch"
	facts := rebuildEventFacts(file, extractor.Language(), parsed.Nodes, parsed.Edges...)
	facts.File.ContentHash = fmt.Sprintf("%x", blake3.Sum256(raw))
	facts.File.Size = len(raw)
	facts = roundTripRebuildRNBridgeFacts(t, facts)

	// Native uses the last MemberOf in stream order, not key order, the
	// receiver hint, the owner's node name, or its language/workspace.
	const foreignFile, lang, workspace = "other.py", "python", "other"
	foreign := rebuildEventNode(foreignFile, "Zulu.handle", lang, workspace)
	foreign.Kind, foreign.Name = graph.KindMethod, "handle"
	foreign.Meta = map[string]any{"receiver": "Zulu"}
	wrongOwner := rebuildEventNode(foreignFile, "z.Zulu", lang, workspace)
	wrongOwner.Kind = graph.KindType
	owner := rebuildEventNode(foreignFile, "pkg.SendEmail", lang, workspace)
	owner.Kind, owner.Name = graph.KindType, "NotSendEmail"
	foreignFacts := rebuildEventFacts(foreignFile, lang, []*graph.Node{foreign, wrongOwner, owner},
		&graph.Edge{From: foreign.ID, To: wrongOwner.ID, Kind: graph.EdgeMemberOf, FilePath: foreignFile, Line: 3},
		&graph.Edge{From: foreign.ID, To: owner.ID, Kind: graph.EdgeMemberOf, FilePath: foreignFile, Line: 3},
	)
	b, native := openRebuildEventStores(t, facts, foreignFacts)
	require.Equal(t, rebuildEventSQLiteRows(native), rebuildEventSavedRows(t, b), "before Laravel Event")
	selection, err := NewFrameworkSynthesizerSelection([]string{SynthLaravelEvent})
	require.NoError(t, err)
	frozen, err := FreezeRebuildFrameworkSelection(t.Context(), b.RebuildView(), selection)
	require.NoError(t, err)
	require.True(t, frozen.allows(SynthLaravelEvent))

	require.Equal(t, 3, ResolveLaravelEventCalls(native), "a repeated map entry must not add listeners")
	want := rebuildEventSQLiteRows(native)
	var dispatches []*graph.Edge
	for _, e := range want.edges {
		if e.Meta["via"] == laravelEventVia {
			dispatches = append(dispatches, e)
		}
	}
	require.Len(t, dispatches, 3)
	for i, to := range []string{primary, typed, foreign.ID} {
		meta := map[string]any{
			"via":                laravelEventVia,
			"laravel_event_type": "OrderShipped",
			MetaSynthesizedBy:    SynthLaravelEvent,
			MetaProvenance:       ProvenanceFramework,
		}
		if to == primary {
			meta["witness"] = "original dispatch"
		}
		require.Equal(t, &graph.Edge{
			From:            file + "::OrderController.ship",
			To:              to,
			Kind:            graph.EdgeCalls,
			FilePath:        file,
			Line:            19,
			Origin:          graph.OriginASTInferred,
			Confidence:      ConfidenceTyped,
			ConfidenceLabel: graph.ConfidenceLabelFor(graph.EdgeCalls, ConfidenceTyped),
			Meta:            meta,
		}, dispatches[i])
	}

	before := rebuildEventSavedRows(t, b)
	suppressed := foreignFacts
	suppressed.Edges = foreignFacts.Edges[1:] // omit the competing owner, not the winner
	stats, err := RunRebuildLaravelEvent(t.Context(), b, rebuildGRPCReceipts(facts, suppressed), frozen)
	require.ErrorIs(t, err, store_pebble.ErrRebuildLaravelEvent)
	require.ErrorIs(t, err, store_pebble.ErrRebuildUnknownProvenance)
	var missing *store_pebble.RebuildUnknownProvenanceError
	require.True(t, errors.As(err, &missing))
	require.Equal(t, file+"::OrderController.ship", missing.First.From)
	require.Zero(t, stats)
	require.Equal(t, before, rebuildEventSavedRows(t, b), "incomplete owner proof must leave rows unchanged")

	stats, err = RunRebuildLaravelEvent(t.Context(), b, rebuildGRPCReceipts(facts, foreignFacts), frozen)
	require.NoError(t, err)
	require.Equal(t, store_pebble.RebuildLaravelEventStats{Stubs: 1, Resolved: 3, Moves: 1, Adds: 2}, stats)
	require.Equal(t, want, rebuildEventSavedRows(t, b), "after one Laravel Event turn")

	// A second turn sees tagged fanout rows with no raw parse ordinal.
	stats, err = RunRebuildLaravelEvent(t.Context(), b, rebuildGRPCReceipts(facts, foreignFacts), frozen)
	require.ErrorIs(t, err, store_pebble.ErrRebuildLaravelEvent)
	require.ErrorIs(t, err, store_pebble.ErrRebuildUnknownProvenance)
	require.Zero(t, stats)
	require.Equal(t, want, rebuildEventSavedRows(t, b), "unproved repeat must leave rows unchanged")
}

func TestRebuildLaravelEventSQLiteMissAndFrozenGate(t *testing.T) {
	const file, php = "events.php", "php"
	const stub = "unresolved::*.handle"
	selected, err := NewFrameworkSynthesizerSelection([]string{SynthLaravelEvent})
	require.NoError(t, err)
	disabled, err := NewFrameworkSynthesizerSelection(nil)
	require.NoError(t, err)
	for _, tc := range []struct {
		name, language, symbol, via string
		nodeKind                    graph.NodeKind
		edgeKind                    graph.EdgeKind
		selection                   FrameworkSynthesizerSelection
		listenMap, admitted, demote bool
	}{
		{"no listener sources", php, "handle", laravelEventVia, graph.KindMethod, graph.EdgeCalls, selected, false, true, false},
		{"map with no handlers", php, "handle", laravelEventVia, graph.KindFunction, graph.EdgeCalls, selected, true, true, true},
		{"not selected", php, "handle", laravelEventVia, graph.KindMethod, graph.EdgeCalls, disabled, true, false, false},
		{"non-PHP handle", "python", "handle", laravelEventVia, graph.KindMethod, graph.EdgeCalls, selected, true, false, false},
		{"wrong name", php, "Handle", laravelEventVia, graph.KindMethod, graph.EdgeCalls, selected, true, false, false},
		{"wrong node kind", php, "handle", laravelEventVia, graph.KindVariable, graph.EdgeCalls, selected, true, false, false},
		{"wrong via", php, "handle", "laravel-event-other", graph.KindMethod, graph.EdgeCalls, selected, true, false, false},
		{"wrong edge kind", php, "handle", laravelEventVia, graph.KindMethod, graph.EdgeReferences, selected, true, false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			caller := rebuildEventNode(file, "publish", php, "")
			handle := rebuildEventNode(file, tc.symbol, tc.language, "")
			handle.Kind = tc.nodeKind
			provider := rebuildEventNode(file, "EventServiceProvider", php, "")
			provider.Kind = graph.KindType
			if tc.listenMap {
				provider.Meta = map[string]any{"laravel_listen_map": `App\Events\OrderShipped => Vendor\MissingHandler`}
			}
			call := &graph.Edge{
				From:            caller.ID,
				To:              handle.ID,
				Kind:            tc.edgeKind,
				FilePath:        file,
				Line:            9,
				Origin:          "stale",
				Confidence:      ConfidenceTyped,
				ConfidenceLabel: graph.ConfidenceLabelFor(tc.edgeKind, ConfidenceTyped),
				Meta: map[string]any{
					"via":                tc.via,
					"laravel_event_type": "OrderShipped",
					"witness":            "stale dispatch",
					MetaSynthesizedBy:    SynthLaravelEvent,
					MetaProvenance:       ProvenanceFramework,
				},
			}
			facts := rebuildEventFacts(file, php, []*graph.Node{caller, handle, provider}, call)
			b, native := openRebuildEventStores(t, facts)
			before := rebuildEventSQLiteRows(native)
			require.Equal(t, before, rebuildEventSavedRows(t, b))
			frozen, err := FreezeRebuildFrameworkSelection(t.Context(), b.RebuildView(), tc.selection)
			require.NoError(t, err)
			require.Equal(t, tc.admitted, frozen.allows(SynthLaravelEvent))

			report := RunFrameworkSynthesizersWithSelection(native, tc.selection)
			require.NoError(t, report.Err)
			require.Zero(t, report.Total, "demotion is not a match")
			want := rebuildEventSQLiteRows(native)
			if tc.demote {
				require.Equal(t, []*graph.Edge{{
					From:     caller.ID,
					To:       stub,
					Kind:     graph.EdgeCalls,
					FilePath: file,
					Line:     9,
					Origin:   graph.OriginASTInferred,
					Meta: map[string]any{
						"via":                laravelEventVia,
						"laravel_event_type": "OrderShipped",
						"witness":            "stale dispatch",
					},
				}}, want.edges)
			} else {
				require.Equal(t, before, want)
			}
			stats, err := RunRebuildLaravelEvent(t.Context(), b, rebuildGRPCReceipts(facts), frozen)
			require.NoError(t, err)
			if !tc.admitted {
				require.Zero(t, stats)
			}
			require.Equal(t, want, rebuildEventSavedRows(t, b))
		})
	}
}
