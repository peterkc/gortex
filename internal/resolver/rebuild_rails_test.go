package resolver

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/graph/store_pebble"
	"github.com/zzet/gortex/internal/parser/languages"
)

func TestRebuildRailsSQLiteOracle(t *testing.T) {
	const (
		caller         = "shop/app/controllers/users_controller.rb"
		foreignService = "shop/app/controllers/user_service.rb"
		service        = "shop/app/services/user_service.rb"
		user           = "shop/app/models/user.rb"
		widget         = "shop/app/models/widget.rb"
		foreignModel   = "shop/lib/widget.rb"
	)
	extractor := languages.NewRubyExtractor()
	var receipts []store_pebble.ParseFacts
	for _, input := range []struct{ file, source string }{
		{caller, `class UsersController
  def index
    UserService.perform()
    UserService.preview()
    User.find(1)
    Widget.find(1)
  end
end
`},
		{foreignService, `class UserService
  def self.perform
  end
  def self.preview
  end
end
`},
		{user, "class User < ApplicationRecord\nend\n"},
		{widget, "class Widget\nend\n"},
		{foreignModel, "class Widget < ApplicationRecord\nend\n"},
		{service, `class UserService
  def self.perform
  end
end
`},
	} {
		parsed, err := extractor.Extract(input.file, []byte(input.source))
		require.NoError(t, err)
		t.Cleanup(parsed.ReleaseTree)
		receipts = append(receipts, roundTripRebuildRNBridgeFacts(t,
			rebuildEventFacts(input.file, extractor.Language(), parsed.Nodes, parsed.Edges...)))
	}
	selected, err := NewFrameworkSynthesizerSelection([]string{SynthRailsResolve})
	require.NoError(t, err)

	b, native := openRebuildEventStores(t, receipts...)
	before := rebuildEventSavedRows(t, b)
	require.Equal(t, rebuildEventSQLiteRows(native), before)

	// Check that extraction supplied the rows that make both losing candidates viable.
	require.Len(t, native.FindNodesByName("Widget"), 2)
	var modelOwners, foreignMembers []string
	var widgetRef bool
	for _, e := range before.edges {
		switch {
		case e.Kind == graph.EdgeModelsTable:
			modelOwners = append(modelOwners, e.From)
		case e.Kind == graph.EdgeMemberOf && e.To == foreignService+"::UserService":
			foreignMembers = append(foreignMembers, e.From)
		case e.Kind == graph.EdgeCalls && e.FilePath == caller && e.Line == 6:
			widgetRef = e.Meta["recv_const"] == "Widget"
		}
	}
	require.ElementsMatch(t, []string{user + "::User", foreignModel + "::Widget"}, modelOwners)
	require.ElementsMatch(t, []string{foreignService + "::UserService.perform", foreignService + "::UserService.preview"}, foreignMembers)
	require.True(t, widgetRef, "the unresolved Widget call must carry recv_const")

	frozen, err := FreezeRebuildFrameworkSelection(t.Context(), b.RebuildView(), selected)
	require.NoError(t, err)
	require.True(t, frozen.allows(SynthRailsResolve))
	require.NoError(t, FreezeRebuildRailsInputs(t.Context(), b, frozen))

	require.Equal(t, 3, ResolveRailsRefs(native))
	want := rebuildEventSQLiteRows(native)
	type callSite struct {
		line int
		to   string
	}
	var calls []callSite
	for _, e := range want.edges {
		if e.Kind == graph.EdgeCalls && e.From == caller+"::UsersController.index" {
			calls = append(calls, callSite{e.Line, e.To})
		}
	}
	require.ElementsMatch(t, []callSite{
		{3, service + "::UserService.perform"}, // The service dir beats the caller's dir.
		{4, service + "::UserService"},         // Only the losing class owns preview.
		{5, user + "::User"},                   // An inherited finder binds to the model.
		{6, "unresolved::*.find"},              // The elected Widget is not a model. Do not fall back to the model.
	}, calls)
	require.Equal(t, before.nodes, want.nodes)
	require.Len(t, want.edges, len(before.edges))
	_, err = RunRebuildRails(t.Context(), b, rebuildGRPCReceipts(receipts...), frozen)
	require.NoError(t, err)
	require.Equal(t, want, rebuildEventSavedRows(t, b))

	t.Run("missing service receipt", func(t *testing.T) {
		b := openRebuildGRPCBuild(t, receipts...)
		before := rebuildEventSavedRows(t, b)
		frozen, err := FreezeRebuildFrameworkSelection(t.Context(), b.RebuildView(), selected)
		require.NoError(t, err)
		require.True(t, frozen.allows(SynthRailsResolve))
		require.NoError(t, FreezeRebuildRailsInputs(t.Context(), b, frozen))

		// The preferred service is saved and captured, but its receipt is absent.
		_, err = RunRebuildRails(t.Context(), b, rebuildGRPCReceipts(receipts[:len(receipts)-1]...), frozen)
		require.ErrorIs(t, err, store_pebble.ErrRebuildUnknownProvenance)
		require.Equal(t, before, rebuildEventSavedRows(t, b))
	})
}

func TestRebuildRailsConflictingEFCoreReceipt(t *testing.T) {
	const modelPath, callerPath = "shop/app/models/Widget.cs", "shop/app/controllers/widgets_controller.rb"
	model := &graph.Node{ID: modelPath + "::Widget", Name: "Widget", Kind: graph.KindType, Language: "csharp", FilePath: modelPath}
	fluent := &graph.Node{ID: modelPath, Name: modelPath, Kind: graph.KindFile, Language: "csharp", FilePath: modelPath,
		Meta: map[string]any{"ef_fluent": []map[string]any{efMappingAction("First", "Widget", "widgets", "", "table", 0, 8)}}}
	conflicting := *fluent
	conflicting.Meta = map[string]any{"ef_fluent": []map[string]any{
		efMappingAction("First", "Widget", "widgets", "", "table", 0, 8),
		efMappingAction("Second", "Widget", "other_widgets", "", "table", 0, 12),
	}}
	caller := &graph.Node{ID: callerPath + "::find", Name: "index", Kind: graph.KindMethod, Language: "ruby", FilePath: callerPath}
	call := &graph.Edge{From: caller.ID, To: "unresolved::*.find", Kind: graph.EdgeCalls, FilePath: callerPath, Line: 3,
		Meta: map[string]any{"recv_const": "Widget"}}
	ruby := rebuildEventFacts(callerPath, "ruby", []*graph.Node{caller}, call)
	settled := rebuildEventFacts(modelPath, "csharp", []*graph.Node{model, fluent})
	selection, err := NewFrameworkSynthesizerSelection([]string{SynthCSharpEFCoreModels, SynthRailsResolve})
	require.NoError(t, err)
	for _, fixture := range []struct {
		name string
		file *graph.Node
		want int
	}{
		{"matching mapping", fluent, 1},
		{"conflicting receipt", &conflicting, 0},
	} {
		t.Run(fixture.name, func(t *testing.T) {
			raw := rebuildEventFacts(modelPath, "csharp", []*graph.Node{model, fixture.file})
			build := openRebuildGRPCBuild(t, ruby, settled)
			_, native := openRebuildEventStores(t, ruby, raw)
			require.Equal(t, fixture.want, efResolveModels(t, native))
			require.Equal(t, fixture.want, ResolveRailsRefs(native))
			before := rebuildEventSavedRows(t, build)
			frozen, err := FreezeRebuildFrameworkSelection(t.Context(), build.RebuildView(), selection)
			require.NoError(t, err)
			require.True(t, frozen.allows(SynthCSharpEFCoreModels))
			require.True(t, frozen.allows(SynthRailsResolve))
			require.NoError(t, FreezeRebuildRailsInputs(t.Context(), build, frozen))
			seq := rebuildGRPCReceipts(ruby, raw)
			changed, err := RunRebuildEFCoreModels(t.Context(), build, seq, frozen)
			if fixture.want == 0 {
				require.ErrorIs(t, err, store_pebble.ErrRebuildEFCoreModels)
				var refused *store_pebble.RebuildUnknownProvenanceError
				require.ErrorAs(t, err, &refused)
				require.Equal(t, graph.EdgeIdentity{From: model.ID, To: "db::orm::widgets", Kind: graph.EdgeModelsTable, FilePath: modelPath, Line: 8}, refused.First)
				require.Zero(t, changed)
				require.Equal(t, before, rebuildEventSavedRows(t, build))
			} else {
				require.NoError(t, err)
				require.EqualValues(t, 1, changed)
			}
			// The worker stops on the EF refusal. Calling Rails directly also
			// proves that the rejected turn left no usable model witness.
			_, err = RunRebuildRails(t.Context(), build, seq, frozen)
			require.NoError(t, err)
			want, got := rebuildEventSQLiteRows(native), rebuildEventSavedRows(t, build)
			require.Equal(t, want.edges, got.edges)
			if fixture.want == 0 {
				// Only the SQLite input includes the conflicting receipt's node.
				require.Equal(t, before.nodes, got.nodes)
			} else {
				require.Equal(t, want.nodes, got.nodes)
			}
		})
	}
}

func TestRebuildRailsCallerNodeCap(t *testing.T) {
	const callerPath = "shop/app/controllers/users_controller.rb"
	const missPath = "shop/app/controllers/admin_controller.rb"
	const servicePath = "shop/app/services/user_service.rb"
	caller := &graph.Node{ID: callerPath + "::index", Name: "index", Kind: graph.KindMethod, Language: "ruby", FilePath: callerPath}
	miss := &graph.Node{ID: missPath + "::index", Name: "index", Kind: graph.KindMethod, Language: "ruby", FilePath: missPath}
	service := &graph.Node{ID: servicePath + "::UserService", Name: "UserService", Kind: graph.KindType, Language: "ruby", FilePath: servicePath}
	call := &graph.Edge{From: caller.ID, To: "unresolved::*.perform", Kind: graph.EdgeCalls, FilePath: callerPath, Line: 3,
		Meta: map[string]any{"recv_const": service.Name}}
	noMatch := &graph.Edge{From: miss.ID, To: call.To, Kind: graph.EdgeCalls, FilePath: missPath, Line: 3,
		Meta: map[string]any{"recv_const": "MissingService"}}
	facts := []store_pebble.ParseFacts{
		rebuildEventFacts(callerPath, "ruby", []*graph.Node{caller}, call),
		rebuildEventFacts(missPath, "ruby", []*graph.Node{miss}, noMatch),
		rebuildEventFacts(servicePath, "ruby", []*graph.Node{service}),
	}
	selected, err := NewFrameworkSynthesizerSelection([]string{SynthRailsResolve})
	require.NoError(t, err)
	padding := strings.Repeat("x", 16<<20)
	for _, tc := range []struct {
		name   string
		large  *graph.Node
		refuse bool
	}{
		{"matching Call", caller, true},
		{"unmatched Call", miss, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			build := openRebuildGRPCBuild(t, facts...)
			frozen, err := FreezeRebuildFrameworkSelection(t.Context(), build.RebuildView(), selected)
			require.NoError(t, err)
			require.True(t, frozen.allows(SynthRailsResolve))
			require.NoError(t, FreezeRebuildRailsInputs(t.Context(), build, frozen))
			large := *tc.large
			large.Meta = map[string]any{"padding": padding}
			build.AddNode(&large)
			before := rebuildEventSavedRows(t, build).edges
			stats, err := RunRebuildRails(t.Context(), build, rebuildGRPCReceipts(facts...), frozen)
			after := rebuildEventSavedRows(t, build).edges
			if tc.refuse {
				require.ErrorIs(t, err, store_pebble.ErrRebuildRails)
				require.ErrorIs(t, err, store_pebble.ErrRebuildLinkInputSize)
				require.ErrorContains(t, err, "rails caller node")
				require.Zero(t, stats)
				require.Equal(t, before, after)
				return
			}
			require.NoError(t, err)
			require.EqualValues(t, 1, stats.Moves)
			want := *call
			want.To, want.Origin, want.Confidence = service.ID, graph.OriginASTInferred, 0.8
			want.ConfidenceLabel = graph.ConfidenceLabelFor(graph.EdgeCalls, 0.8)
			want.Meta = map[string]any{"recv_const": service.Name, MetaSynthesizedBy: SynthRailsResolve}
			require.ElementsMatch(t, []*graph.Edge{&want, noMatch}, after)
		})
	}
}
