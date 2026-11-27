package resolver

import (
	"fmt"
	"slices"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/zeebo/blake3"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/graph/store_pebble"
	"github.com/zzet/gortex/internal/parser/languages"
)

func TestRebuildSidekiqSQLiteOracle(t *testing.T) {
	const file = "app.rb"
	const source = `class EmailJob
  include Sidekiq::Job
  def perform(id)
  end
end

class Controller
  def notify(id)
    EmailJob.perform_async(id)
  end
end
`
	extractor := languages.NewRubyExtractor()
	raw := []byte(source)
	parsed, err := extractor.Extract(file, raw)
	require.NoError(t, err)
	t.Cleanup(parsed.ReleaseTree)
	facts := rebuildEventFacts(file, extractor.Language(), parsed.Nodes, parsed.Edges...)
	facts.File.ContentHash = fmt.Sprintf("%x", blake3.Sum256(raw))
	facts.File.Size = len(raw)
	facts = roundTripRebuildRNBridgeFacts(t, facts)

	want := runRebuildSidekiqSQLiteOracle(t, 1, 1, facts)
	i := slices.IndexFunc(want.edges, func(e *graph.Edge) bool { return e.Meta["via"] == sidekiqVia })
	require.GreaterOrEqual(t, i, 0, "Ruby extractor must emit the dispatch")
	edge := want.edges[i]
	require.Equal(t, file+"::Controller.notify", edge.From)
	require.Equal(t, file+"::EmailJob.perform", edge.To)
	require.Equal(t, graph.EdgeCalls, edge.Kind)
	require.Equal(t, 9, edge.Line)
	require.Equal(t, "EmailJob", edge.Meta["sidekiq_worker"])
	require.Equal(t, graph.OriginASTInferred, edge.Origin)
	require.Equal(t, ConfidenceTyped, edge.Confidence)
	require.Equal(t, SynthSidekiq, edge.Meta[MetaSynthesizedBy])
	require.Equal(t, ProvenanceFramework, edge.Meta[MetaProvenance])
}

func TestRebuildSidekiqSQLiteScopedElection(t *testing.T) {
	const app, jobs, other = "app.rb", "jobs.rb", "other.rb"
	const ruby, stub = "ruby", "unresolved::*.perform"
	caller := rebuildEventNode(app, "Controller.notify", ruby, "local")
	caller.Kind = graph.KindMethod
	worker := func(file, name, workspace string) *graph.Node {
		return &graph.Node{ID: file + "::" + name + ".perform", Name: "perform", Kind: graph.KindMethod,
			FilePath: file, Language: ruby, WorkspaceID: workspace, Meta: map[string]any{"sidekiq_worker": name}}
	}
	local := worker(app, "Job", "local")
	exact := worker(jobs, "Local::Job", "local")
	fallback := worker(jobs, "Later", "local")
	fallback.Kind = graph.KindFunction
	remote := worker(other, "Remote::Job", "remote")
	appNodes := []*graph.Node{caller, local}
	jobNodes := []*graph.Node{exact, fallback, worker(jobs, "One::Ambiguous", "local"), worker(jobs, "Two::Ambiguous", "local")}
	cases := []struct {
		name, worker, fromTarget, toTarget string
	}{
		{"full name beats caller-file simple match", "Local::Job", stub, exact.ID},
		{"full name exists outside workspace", "Remote::Job", local.ID, stub},
		{"missing full name permits simple match", "Missing::Later", stub, fallback.ID},
		{"simple name is ambiguous", "Missing::Ambiguous", stub, stub},
		{"caller file breaks a simple-name tie", "Missing::Job", stub, local.ID},
	}
	var calls []*graph.Edge
	for i, tc := range cases {
		call := &graph.Edge{From: caller.ID, To: tc.fromTarget, Kind: graph.EdgeCalls, FilePath: app, Line: i + 10,
			Meta: map[string]any{"via": sidekiqVia, "sidekiq_worker": tc.worker, "witness": tc.name}}
		if tc.fromTarget != stub {
			call.Origin = graph.OriginASTInferred
			call.Confidence = ConfidenceTyped
			call.ConfidenceLabel = graph.ConfidenceLabelFor(graph.EdgeCalls, ConfidenceTyped)
			StampSynthesizedTyped(call, SynthSidekiq)
		}
		calls = append(calls, call)
	}
	// Workspace IDs are effective facts; the portable parser codec rejects them.
	want := runRebuildSidekiqSQLiteOracle(t, 3, 4,
		rebuildEventFacts(app, ruby, appNodes, calls...),
		rebuildEventFacts(jobs, ruby, jobNodes),
		rebuildEventFacts(other, ruby, []*graph.Node{remote}),
	)
	require.Len(t, want.edges, len(cases))
	for i, tc := range cases {
		line := i + 10
		j := slices.IndexFunc(want.edges, func(e *graph.Edge) bool { return e.Line == line })
		require.GreaterOrEqual(t, j, 0, "%s: missing call row", tc.name)
		edge := want.edges[j]
		require.Equal(t, tc.toTarget, edge.To, tc.name)
		require.Equal(t, tc.name, edge.Meta["witness"])
		if tc.toTarget == stub {
			require.Zero(t, edge.Confidence, tc.name)
			require.Empty(t, edge.ConfidenceLabel, tc.name)
			require.NotContains(t, edge.Meta, MetaSynthesizedBy, tc.name)
			require.NotContains(t, edge.Meta, MetaProvenance, tc.name)
		} else {
			require.Equal(t, ConfidenceTyped, edge.Confidence, tc.name)
			require.Equal(t, SynthSidekiq, edge.Meta[MetaSynthesizedBy], tc.name)
		}
	}
}

func TestRebuildSidekiqFrozenSelectionUsesRubyPerformNodes(t *testing.T) {
	const file = "app.rb"
	selected, err := NewFrameworkSynthesizerSelection([]string{SynthSidekiq})
	require.NoError(t, err)
	disabled, err := NewFrameworkSynthesizerSelection(nil)
	require.NoError(t, err)
	for _, tc := range []struct {
		name, language, symbol string
		kind                   graph.NodeKind
		selection              FrameworkSynthesizerSelection
		admitted               bool
	}{
		{"untagged Ruby method", "ruby", "perform", graph.KindMethod, selected, true},
		{"untagged Ruby function", "ruby", "perform", graph.KindFunction, selected, true},
		{"wrong language", "python", "perform", graph.KindFunction, selected, false},
		{"wrong name", "ruby", "enqueue", graph.KindMethod, selected, false},
		{"wrong kind", "ruby", "perform", graph.KindVariable, selected, false},
		{"not selected", "ruby", "perform", graph.KindMethod, disabled, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			// No Meta and no Calls: the native gate only needs the node census.
			n := &graph.Node{ID: file + "::" + tc.symbol, Name: tc.symbol, Kind: tc.kind, FilePath: file, Language: tc.language}
			b := openRebuildGRPCBuild(t, rebuildEventFacts(file, "ruby", []*graph.Node{n}))
			frozen, err := FreezeRebuildFrameworkSelection(t.Context(), b.RebuildView(), tc.selection)
			require.NoError(t, err)
			require.Equal(t, tc.admitted, frozen.allows(SynthSidekiq))
		})
	}
}

func runRebuildSidekiqSQLiteOracle(t *testing.T, landed int, firstMoves uint64, facts ...store_pebble.ParseFacts) rebuildEventRows {
	t.Helper()
	b, native := openRebuildEventStores(t, facts...)
	before := rebuildEventSQLiteRows(native)
	require.Equal(t, before, rebuildEventSavedRows(t, b), "before Sidekiq")
	selection, err := NewFrameworkSynthesizerSelection([]string{SynthSidekiq})
	require.NoError(t, err)
	frozen, err := FreezeRebuildFrameworkSelection(t.Context(), b.RebuildView(), selection)
	require.NoError(t, err)
	require.True(t, frozen.allows(SynthSidekiq))

	report := RunFrameworkSynthesizersWithSelection(native, selection)
	require.NoError(t, report.Err)
	require.Equal(t, landed, report.Total)
	want := rebuildEventSQLiteRows(native)
	stats, err := RunRebuildSidekiq(t.Context(), b, rebuildGRPCReceipts(facts...), frozen)
	require.NoError(t, err)
	require.Equal(t, firstMoves, stats.Moves)
	require.Equal(t, want, rebuildEventSavedRows(t, b), "after one Sidekiq stage")

	report = RunFrameworkSynthesizersWithSelection(native, selection)
	require.NoError(t, report.Err)
	require.Equal(t, landed, report.Total, "native counts stationary hits too")
	require.Equal(t, want, rebuildEventSQLiteRows(native), "native replay")
	stats, err = RunRebuildSidekiq(t.Context(), b, rebuildGRPCReceipts(facts...), frozen)
	require.NoError(t, err)
	require.Zero(t, stats.Moves, "private replay")
	require.Equal(t, want, rebuildEventSavedRows(t, b), "private replay")
	return want
}

func TestRebuildSidekiqProvesFullNamePriority(t *testing.T) {
	const file, remoteFile, ruby = "app.rb", "remote.rb", "ruby"
	caller := rebuildEventNode(file, "enqueue", ruby, "local")
	worker := func(path, registered, workspace string) *graph.Node {
		n := rebuildEventNode(path, "perform", ruby, workspace)
		n.Kind = graph.KindMethod
		n.Meta = map[string]any{"sidekiq_worker": registered}
		return n
	}
	local := worker(file, "Job", "local")
	for _, tc := range []struct {
		name, live, raw, liveTo, plannedTo string
	}{
		{"missing exact out-of-scope full name", "Remote::Job", "Other::Job", local.ID, "unresolved::*.perform"},
		{"known declaration enters the full name", "Other::Job", "Remote::Job", "unresolved::*.perform", local.ID},
	} {
		t.Run(tc.name, func(t *testing.T) {
			remote := worker(remoteFile, tc.live, "remote")
			call := &graph.Edge{From: caller.ID, To: tc.liveTo, Kind: graph.EdgeCalls, FilePath: file, Line: 9,
				Meta: map[string]any{"via": sidekiqVia, "sidekiq_worker": "Remote::Job"}}
			app := rebuildEventFacts(file, ruby, []*graph.Node{caller, local}, call)
			b := openRebuildGRPCBuild(t, app, rebuildEventFacts(remoteFile, ruby, []*graph.Node{remote}))
			before := rebuildEventSavedRows(t, b)
			selection, err := NewFrameworkSynthesizerSelection([]string{SynthSidekiq})
			require.NoError(t, err)
			frozen, err := FreezeRebuildFrameworkSelection(t.Context(), b.RebuildView(), selection)
			require.NoError(t, err)
			require.True(t, frozen.allows(SynthSidekiq))
			changed := *remote
			changed.Meta = map[string]any{"sidekiq_worker": tc.raw}
			record := rebuildEventFacts(remoteFile, ruby, []*graph.Node{&changed})
			stats, err := RunRebuildSidekiq(t.Context(), b, rebuildGRPCReceipts(app, record), frozen)
			var refusal *store_pebble.RebuildUnknownProvenanceError
			require.ErrorIs(t, err, store_pebble.ErrRebuildSidekiq)
			require.ErrorAs(t, err, &refusal)
			first := graph.EdgeIdentityFor(call)
			first.To = tc.plannedTo
			require.Equal(t, &store_pebble.RebuildUnknownProvenanceError{Count: 1, First: first}, refusal)
			require.Zero(t, stats)
			require.Equal(t, before, rebuildEventSavedRows(t, b))
		})
	}
}
