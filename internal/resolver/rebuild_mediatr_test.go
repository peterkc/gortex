package resolver

import (
	"fmt"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/zeebo/blake3"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/graph/store_pebble"
	"github.com/zzet/gortex/internal/parser/languages"
)

func TestRebuildMediatRSQLiteOracle(t *testing.T) {
	const file = "App.cs"
	const source = `using MediatR;
public class CreateOrderHandler : IRequestHandler<CreateOrder, int> {
  public Task<int> Handle(CreateOrder r, CancellationToken ct) { return Task.FromResult(0); }
}
public class EmailHandler : INotificationHandler<OrderPlaced> {
  public Task Handle(OrderPlaced n, CancellationToken ct) { return Task.CompletedTask; }
}
public class SmsHandler : INotificationHandler<OrderPlaced> {
  public Task Handle(OrderPlaced n, CancellationToken ct) { return Task.CompletedTask; }
}
public class Controller {
  private IMediator _mediator;
  public async Task Place() {
    await _mediator.Send(new CreateOrder(1));
    await _mediator.Publish(new OrderPlaced(1));
  }
}
`
	extractor := languages.NewCSharpExtractor()
	raw := []byte(source)
	parsed, err := extractor.Extract(file, raw)
	require.NoError(t, err)
	t.Cleanup(parsed.ReleaseTree)
	facts := rebuildEventFacts(file, extractor.Language(), parsed.Nodes, parsed.Edges...)
	facts.File.ContentHash = fmt.Sprintf("%x", blake3.Sum256(raw))
	facts.File.Size = len(raw)
	facts = roundTripRebuildRNBridgeFacts(t, facts)

	want := runRebuildMediatRSQLiteOracle(t, facts)
	targets := map[string]struct {
		line                    int
		kind, message, receiver string
	}{
		file + "::CreateOrderHandler.Handle": {14, "request", "CreateOrder", "_mediator"},
		file + "::EmailHandler.Handle":       {15, "notification", "OrderPlaced", "_mediator"},
		file + "::SmsHandler.Handle":         {15, "notification", "OrderPlaced", ""},
	}
	for _, e := range want.edges {
		if e.Meta["via"] != mediatrVia {
			continue
		}
		expected, ok := targets[e.To]
		require.True(t, ok, "unexpected MediatR target %q", e.To)
		delete(targets, e.To)
		require.Equal(t, file+"::Controller.Place", e.From)
		require.Equal(t, file, e.FilePath)
		require.Equal(t, graph.EdgeCalls, e.Kind)
		require.Equal(t, expected.line, e.Line)
		require.Equal(t, expected.kind, e.Meta["mediatr_kind"])
		require.Equal(t, expected.message, e.Meta["mediatr_request_type"])
		if expected.receiver == "" {
			require.NotContains(t, e.Meta, "mediatr_receiver", "fanout is a fresh payload")
		} else {
			require.Equal(t, expected.receiver, e.Meta["mediatr_receiver"], "rebind preserves the source payload")
		}
		require.Equal(t, graph.OriginASTInferred, e.Origin)
		require.Equal(t, ConfidenceTyped, e.Confidence)
		require.Equal(t, SynthMediatR, e.Meta[MetaSynthesizedBy])
		require.Equal(t, ProvenanceFramework, e.Meta[MetaProvenance])
	}
	require.Empty(t, targets, "missing MediatR rows")
}

func TestRebuildMediatRSQLiteCollisionOrder(t *testing.T) {
	const file, outside, lang = "Publish.cs", "Outside.cs", "csharp"
	const callerID = file + "::Controller.Publish"
	const primaryID, fanoutID = file + "::Email.Handle", file + "::Sms.Handle"
	caller := &graph.Node{ID: callerID, Name: "Publish", Kind: graph.KindMethod,
		FilePath: file, Language: lang, WorkspaceID: "main"}
	handler := func(file, id, workspace string) *graph.Node {
		return &graph.Node{ID: id, Name: "Handle", Kind: graph.KindMethod, FilePath: file,
			Language: lang, WorkspaceID: workspace,
			Meta: map[string]any{"mediatr_kind": "notification", "mediatr_request_type": "Placed"}}
	}
	nodes := []*graph.Node{caller, handler(file, primaryID, "main"), handler(file, fanoutID, "main")}
	remote := rebuildEventFacts(outside, lang, []*graph.Node{handler(outside, outside+"::Other.Handle", "other")})
	for _, oldTargets := range [][2]string{{"z", "a"}, {"a", "z"}} {
		t.Run(oldTargets[0]+"_first", func(t *testing.T) {
			mover := func(old string) *graph.Edge {
				return &graph.Edge{From: callerID, To: "unresolved::" + old, Kind: graph.EdgeCalls, FilePath: file, Line: 10,
					Meta: map[string]any{"via": mediatrVia, "mediatr_kind": "notification",
						"mediatr_request_type": "Placed", "mediatr_receiver": old}}
			}
			incumbent := &graph.Edge{From: callerID, To: fanoutID, Kind: graph.EdgeCalls, FilePath: file, Line: 10,
				Meta: map[string]any{"witness": "incumbent fanout destination"}}
			// SQLite sees receipt order, not the order of the private edge keys.
			// Both rebinds land on Email; the fanout adds must not replace Sms.
			first, second := mover(oldTargets[0]), mover(oldTargets[1])
			facts := rebuildEventFacts(file, lang, nodes, first, second, incumbent)
			want := runRebuildMediatRSQLiteOracle(t, facts, remote)
			require.Len(t, want.edges, 2)
			require.Equal(t, primaryID, want.edges[0].To)
			require.Equal(t, oldTargets[0], want.edges[0].Meta["mediatr_receiver"])
			require.Equal(t, SynthMediatR, want.edges[0].Meta[MetaSynthesizedBy])
			require.Equal(t, incumbent, want.edges[1])
		})
	}
}

func runRebuildMediatRSQLiteOracle(t *testing.T, facts ...store_pebble.ParseFacts) rebuildEventRows {
	t.Helper()
	b, native := openRebuildEventStores(t, facts...)
	before := rebuildEventSQLiteRows(native)
	require.Equal(t, before, rebuildEventSavedRows(t, b), "before MediatR")
	selection, err := NewFrameworkSynthesizerSelection([]string{SynthMediatR})
	require.NoError(t, err)
	frozen, err := FreezeRebuildFrameworkSelection(t.Context(), b.RebuildView(), selection)
	require.NoError(t, err)
	require.True(t, frozen.allows(SynthMediatR))

	require.NoError(t, RunFrameworkSynthesizersWithSelection(native, selection).Err)
	want := rebuildEventSQLiteRows(native)
	_, err = RunRebuildMediatR(t.Context(), b, rebuildGRPCReceipts(facts...), frozen)
	require.NoError(t, err)
	require.Equal(t, want, rebuildEventSavedRows(t, b), "after one MediatR pass")
	return want
}

func TestRebuildMediatRRequiresRequestTypeForAnchor(t *testing.T) {
	const file = "Dispatch.cs"
	handler := &graph.Node{ID: file + "::Handler", Kind: graph.KindMethod, FilePath: file, Language: "csharp",
		Meta: map[string]any{"mediatr_kind": "request", "mediatr_request_type": "Other"}}
	call := &graph.Edge{From: "caller", To: "settled", Kind: graph.EdgeCalls, FilePath: file, Line: 1,
		Meta: map[string]any{"via": mediatrVia, "mediatr_kind": "request", "mediatr_request_type": "Missing"}}
	b := openRebuildGRPCBuild(t, rebuildEventFacts(file, "csharp", []*graph.Node{handler}, call))
	before := rebuildEventSavedRows(t, b)
	raw := *handler
	raw.Meta = map[string]any{"mediatr_kind": "request"}
	receipt := rebuildEventFacts(file, "csharp", []*graph.Node{&raw}, call)
	_, native := openRebuildEventStores(t, receipt)
	require.Zero(t, ResolveMediatRCalls(native))
	require.Contains(t, rebuildEventSQLiteRows(native).edges, call)
	selection, err := NewFrameworkSynthesizerSelection([]string{SynthMediatR})
	require.NoError(t, err)
	stats, err := RunRebuildMediatR(t.Context(), b, rebuildGRPCReceipts(receipt), selection)
	var refused *store_pebble.RebuildUnknownProvenanceError
	require.ErrorAs(t, err, &refused)
	want := graph.EdgeIdentityFor(call)
	want.To = "unresolved::*.Handle"
	require.Equal(t, &store_pebble.RebuildUnknownProvenanceError{Count: 1, First: want}, refused)
	require.Zero(t, stats)
	require.Equal(t, before, rebuildEventSavedRows(t, b))
}
