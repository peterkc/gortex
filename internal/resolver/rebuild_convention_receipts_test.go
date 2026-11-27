package resolver

import (
	"context"
	"iter"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/graph/store_pebble"
)

func TestRebuildConventionReceiptEntrants(t *testing.T) {
	for _, tc := range []struct {
		stage, file, lang, name, target, via, candidate string
		freeze                                          func(context.Context, *store_pebble.BuildStore, FrameworkSynthesizerSelection) error
		run                                             func(context.Context, *store_pebble.BuildStore, iter.Seq2[store_pebble.ParseFacts, error], FrameworkSynthesizerSelection) (uint64, error)
	}{
		{SynthFastAPIResolve, "App/app.py", "python", "load", "load", "fastapi.Depends", "App/dependencies/load.py", FreezeRebuildFastAPIInputs, RunRebuildFastAPI},
		{SynthReactResolve, "App/view.tsx", "typescript", "Auth", "AuthContext", "react_context", "App/providers/auth.ts", FreezeRebuildReactResolveInputs, RunRebuildReactResolve},
		{SynthSwiftUIResolve, "App/Panel.swift", "swift", "ProfileView", "ProfileView", "", "App/Views/Profile.swift", nil, RunRebuildSwiftUI},
		{SynthVaporResolve, "App/routes.swift", "swift", "UserController", "UserController", "", "App/Controllers/User.swift", nil, RunRebuildVapor},
	} {
		t.Run(tc.stage, func(t *testing.T) {
			source := &graph.Node{ID: "source", Kind: graph.KindFunction, FilePath: tc.file, Language: tc.lang}
			winner := &graph.Node{ID: "winner", Name: tc.name, Kind: graph.KindType, FilePath: tc.candidate, Language: tc.lang}
			rival := *winner
			rival.ID, rival.Name = "rival", "Other"
			call := &graph.Edge{From: source.ID, To: "unresolved::" + tc.target, Kind: graph.EdgeCalls, FilePath: tc.file, Line: 1,
				Meta: map[string]any{"via": tc.via}}
			caller := rebuildEventFacts(tc.file, tc.lang, []*graph.Node{source}, call)
			candidates := rebuildEventFacts(tc.candidate, tc.lang, []*graph.Node{winner, &rival})
			b := openRebuildGRPCBuild(t, caller, candidates)
			before := rebuildEventSavedRows(t, b)
			selection, err := NewFrameworkSynthesizerSelection([]string{tc.stage})
			require.NoError(t, err)
			if tc.freeze != nil {
				require.NoError(t, tc.freeze(t.Context(), b, selection))
			}
			entrant := rival
			entrant.Name = tc.name
			candidates.Nodes = []*graph.Node{winner, &entrant}
			moves, err := tc.run(t.Context(), b, rebuildGRPCReceipts(caller, candidates), selection)
			var refused *store_pebble.RebuildUnknownProvenanceError
			require.ErrorAs(t, err, &refused)
			require.Equal(t, &store_pebble.RebuildUnknownProvenanceError{Count: 1, First: graph.EdgeIdentityFor(call)}, refused)
			require.Zero(t, moves)
			require.Equal(t, before, rebuildEventSavedRows(t, b))
		})
	}
}

func TestRebuildConventionReceiptSourceChangesElection(t *testing.T) {
	for _, tc := range []struct {
		name, live, raw string
		receiptSource   bool
	}{
		{name: "enters Python source", live: "web/app.txt", raw: "web/app.py", receiptSource: true},
		{name: "breaks live tie", live: "web/app.py", raw: "one/dependencies/app.py", receiptSource: true},
		{name: "missing source proof", live: "one/dependencies/app.py", raw: "one/dependencies/app.py"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			source := &graph.Node{ID: "source", Kind: graph.KindFunction, FilePath: tc.live, Language: "python"}
			call := &graph.Edge{From: source.ID, To: "unresolved::load", Kind: graph.EdgeCalls, FilePath: tc.live, Line: 1,
				Meta: map[string]any{"via": "fastapi.Depends"}}
			first := &graph.Node{ID: "first", Name: "load", Kind: graph.KindFunction, FilePath: "one/dependencies/load.py", Language: "python"}
			second := *first
			second.ID, second.FilePath = "second", "two/dependencies/load.py"
			caller := rebuildEventFacts(tc.live, "python", []*graph.Node{source}, call)
			candidates := []store_pebble.ParseFacts{rebuildEventFacts(first.FilePath, "python", []*graph.Node{first})}
			if tc.live != "web/app.txt" {
				candidates = append(candidates, rebuildEventFacts(second.FilePath, "python", []*graph.Node{&second}))
			}
			b := openRebuildGRPCBuild(t, append(candidates, caller)...)
			before := rebuildEventSavedRows(t, b)
			selection, err := NewFrameworkSynthesizerSelection([]string{SynthFastAPIResolve})
			require.NoError(t, err)
			require.NoError(t, FreezeRebuildFastAPIInputs(t.Context(), b, selection))
			caller.Nodes = nil
			receipts := append(candidates, caller)
			if tc.receiptSource {
				raw := *source
				raw.FilePath = tc.raw
				receipts = append(receipts, rebuildEventFacts(tc.raw, "python", []*graph.Node{&raw}))
			}
			moves, err := RunRebuildFastAPI(t.Context(), b, rebuildGRPCReceipts(receipts...), selection)
			var refused *store_pebble.RebuildUnknownProvenanceError
			require.ErrorAs(t, err, &refused)
			require.Equal(t, &store_pebble.RebuildUnknownProvenanceError{Count: 1, First: graph.EdgeIdentityFor(call)}, refused)
			require.Zero(t, moves)
			require.Equal(t, before, rebuildEventSavedRows(t, b))
		})
	}
}
