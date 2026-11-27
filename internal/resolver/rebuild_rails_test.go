package resolver

import (
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
