package resolver

import (
	"maps"
	"slices"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/graph/store_pebble"
	"github.com/zzet/gortex/internal/parser/languages"
)

func TestRebuildNgRxEffectSQLiteOracle(t *testing.T) {
	// Use the class-property capture from TestCaptureNgRxEffects and real
	// action creators in a separate TS extraction.
	const effects = `import { createEffect, ofType } from '@ngrx/effects';
import { Injectable } from '@angular/core';

@Injectable()
export class UserEffects {
  loadUsers$ = createEffect(() => this.actions$.pipe(
    ofType(LoadUsers, UserActions.ReloadUsers),
    mergeMap(() => this.api.load())
  ));
}
`
	const actions = `import { createAction } from '@ngrx/store';
export const LoadUsers = createAction('[Users] Load');
export const ReloadUsers = createAction('[Users] Reload');
`
	extractor := languages.NewTypeScriptExtractor()
	effect, err := extractor.Extract("effects.ts", []byte(effects))
	require.NoError(t, err)
	t.Cleanup(effect.ReleaseTree)
	action, err := extractor.Extract("actions.ts", []byte(actions))
	require.NoError(t, err)
	t.Cleanup(action.ReleaseTree)

	const from = "effects.ts::UserEffects.loadUsers$"
	i := slices.IndexFunc(effect.Nodes, func(n *graph.Node) bool { return n.ID == from })
	require.NotEqual(t, -1, i)
	require.Equal(t, "loadUsers$", effect.Nodes[i].Meta["ngrx_effect"])
	var markers []*graph.Edge
	for _, e := range effect.Edges {
		if e.Meta["via"] == "ngrx-effect" {
			markers = append(markers, e)
		}
	}
	wantMarkers := []*graph.Edge{
		{From: from, To: "unresolved::*.LoadUsers", Kind: graph.EdgeCalls, FilePath: "effects.ts", Line: 6,
			Meta: map[string]any{"via": "ngrx-effect", "ngrx_action": "LoadUsers"}},
		{From: from, To: "unresolved::*.ReloadUsers", Kind: graph.EdgeCalls, FilePath: "effects.ts", Line: 6,
			Meta: map[string]any{"via": "ngrx-effect", "ngrx_action": "ReloadUsers"}},
	}
	require.ElementsMatch(t, wantMarkers, markers)
	requireRebuildNgRxEffectOracle(t, []store_pebble.ParseFacts{
		roundTripRebuildRNBridgeFacts(t, rebuildEventFacts("effects.ts", extractor.Language(), effect.Nodes, effect.Edges...)),
		roundTripRebuildRNBridgeFacts(t, rebuildEventFacts("actions.ts", extractor.Language(), action.Nodes, action.Edges...)),
	}, map[graph.EdgeIdentity]string{
		graph.EdgeIdentityFor(wantMarkers[0]): "actions.ts::LoadUsers",
		graph.EdgeIdentityFor(wantMarkers[1]): "actions.ts::ReloadUsers",
	}, 2)
}

func TestRebuildNgRxEffectSQLiteElection(t *testing.T) {
	const callerFile, getterFile, siteFile = "caller.ts", "getter.ts", "sites.ts"
	const lang, workspace = "typescript", "workspace"
	node := func(file, name string, kind graph.NodeKind) *graph.Node {
		n := rebuildEventNode(file, name, lang, workspace)
		n.Kind = kind
		return n
	}
	caller := node(callerFile, "effect", graph.KindVariable)
	local := node(callerFile, "dispatch", graph.KindVariable)
	field := node(callerFile, "dispatch", graph.KindField)
	field.ID = callerFile + "::Receiver.dispatch"
	remote := node(getterFile, "dispatch", graph.KindConstant)
	getter := node(getterFile, "useStore", graph.KindFunction)
	typed := node(getterFile, "typed", graph.KindType)
	foreign := node(siteFile, "typed", graph.KindType)
	foreign.WorkspaceID = "outside"
	orphan := node(siteFile, "orphan", graph.KindFunction)
	orphan.WorkspaceID = "outside"
	firstRetry := node(getterFile, "retry", graph.KindConstant)
	secondRetry := node(siteFile, "retry", graph.KindVariable)

	// The site file is not the caller node's file. No source or target tags
	// are required on nodes; only the bound Call carries store_binding.
	localCall := rebuildNgRxEffectCall(caller.ID, siteFile, "dispatch", 10)
	bound := rebuildNgRxEffectCall(caller.ID, siteFile, "dispatch", 11)
	bound.Meta["store_binding"] = "useStore"
	bound.Meta[MetaProvenance] = "fixture-source"
	scoped := rebuildNgRxEffectCall(caller.ID, siteFile, "typed", 12)
	missing := rebuildNgRxEffectCall(siteFile+"::missing", siteFile, "orphan", 13)
	stale := rebuildNgRxEffectCall(caller.ID, siteFile, "retry", 14)
	stale.To = firstRetry.ID
	stale.Origin = graph.OriginASTInferred
	stale.Confidence, stale.ConfidenceLabel = 0.6, "INFERRED"
	stale.Meta[MetaSynthesizedBy], stale.Meta[MetaProvenance] = SynthNgRxEffect, ProvenanceHeuristic
	stale.Meta["payload"] = "keep on restub"
	unmarked := rebuildNgRxEffectCall(caller.ID, siteFile, "dispatch", 15)
	delete(unmarked.Meta, "via")

	requireRebuildNgRxEffectOracle(t, []store_pebble.ParseFacts{
		rebuildEventFacts(callerFile, lang, []*graph.Node{caller, local, field}),
		rebuildEventFacts(getterFile, lang, []*graph.Node{getter, remote, typed, firstRetry}),
		rebuildEventFacts(siteFile, lang, []*graph.Node{foreign, orphan, secondRetry},
			localCall, bound, scoped, missing, stale, unmarked),
	}, map[graph.EdgeIdentity]string{
		graph.EdgeIdentityFor(localCall): local.ID,
		graph.EdgeIdentityFor(bound):     remote.ID,
		graph.EdgeIdentityFor(scoped):    typed.ID,
		graph.EdgeIdentityFor(missing):   orphan.ID,
		graph.EdgeIdentityFor(stale):     "unresolved::*.retry",
	}, 4)
}

func TestRebuildNgRxEffectSQLiteNoOp(t *testing.T) {
	for _, tc := range []struct {
		name, file, language, via string
		kind                      graph.EdgeKind
		withAction, allowed       bool
	}{
		{"web without via", "call.ts", "typescript", "", graph.EdgeCalls, true, false},
		{"web with inexact via", "call.ts", "typescript", "ngrx-effect.extra", graph.EdgeCalls, true, false},
		{"via on non-Call", "call.ts", "typescript", "ngrx-effect", graph.EdgeDefines, true, false},
		{"via without web", "call.go", "go", "ngrx-effect", graph.EdgeCalls, true, false},
		{"exact Call via without action or effect tag", "call.ts", "typescript", "ngrx-effect", graph.EdgeCalls, false, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			caller := rebuildEventNode(tc.file, "effect", tc.language, "")
			action := rebuildEventNode(tc.file, "load", tc.language, "")
			call := rebuildNgRxEffectCall(caller.ID, tc.file, "load", 9)
			call.Kind = tc.kind
			if tc.via == "" {
				delete(call.Meta, "via")
			} else {
				call.Meta["via"] = tc.via
			}
			if tc.withAction {
				caller.Meta = map[string]any{"ngrx_effect": "effect"}
			} else {
				delete(call.Meta, "ngrx_action")
			}
			facts := rebuildEventFacts(tc.file, tc.language, []*graph.Node{caller, action}, call)
			b, native := openRebuildEventStores(t, facts)
			before := rebuildEventSQLiteRows(native)
			require.Len(t, before.edges, 1)
			require.Equal(t, before, rebuildEventSavedRows(t, b))
			selection, err := NewFrameworkSynthesizerSelection([]string{SynthNgRxEffect})
			require.NoError(t, err)
			frozen, err := FreezeRebuildFrameworkSelection(t.Context(), b.RebuildView(), selection)
			require.NoError(t, err)
			require.Equal(t, tc.allowed, frozen.allows(SynthNgRxEffect))

			report := RunFrameworkSynthesizersWithSelection(native, selection)
			require.NoError(t, report.Err)
			require.Zero(t, report.Total)
			require.Equal(t, before, rebuildEventSQLiteRows(native))
			receipts := func(_ func(store_pebble.ParseFacts, error) bool) {
				t.Fatal("NgRx effect stage without moves must not consume receipts")
			}
			stats, err := RunRebuildNgRxEffect(t.Context(), b, receipts, frozen)
			require.NoError(t, err)
			require.Zero(t, stats)
			require.Equal(t, before, rebuildEventSavedRows(t, b))
		})
	}
}

func requireRebuildNgRxEffectOracle(t *testing.T, facts []store_pebble.ParseFacts,
	moves map[graph.EdgeIdentity]string, resolved uint64,
) {
	t.Helper()
	b, native := openRebuildEventStores(t, facts...)
	want := rebuildEventSQLiteRows(native)
	require.Equal(t, want, rebuildEventSavedRows(t, b))
	selection, err := NewFrameworkSynthesizerSelection([]string{SynthNgRxEffect})
	require.NoError(t, err)
	frozen, err := FreezeRebuildFrameworkSelection(t.Context(), b.RebuildView(), selection)
	require.NoError(t, err)
	require.True(t, frozen.allows(SynthNgRxEffect))

	changed := 0
	for i, e := range want.edges {
		to, ok := moves[graph.EdgeIdentityFor(e)]
		if !ok {
			continue
		}
		require.NotEqual(t, e.To, to)
		moved := *e
		moved.To = to
		moved.Origin = graph.OriginASTInferred
		moved.Meta = maps.Clone(e.Meta)
		if graph.IsUnresolvedTarget(to) {
			moved.Confidence, moved.ConfidenceLabel = 0, ""
			delete(moved.Meta, MetaSynthesizedBy)
			delete(moved.Meta, MetaProvenance)
		} else {
			moved.Confidence, moved.ConfidenceLabel = 0.6, "INFERRED"
			moved.Meta[MetaSynthesizedBy] = SynthNgRxEffect
			if _, ok := moved.Meta[MetaProvenance]; !ok {
				moved.Meta[MetaProvenance] = ProvenanceHeuristic
			}
		}
		want.edges[i] = &moved
		changed++
	}
	require.Len(t, moves, changed)
	want.sort()

	receipts := rebuildGRPCReceipts(facts...)
	for pass, wantMoves := range []uint64{uint64(changed), 0} {
		report := RunFrameworkSynthesizersWithSelection(native, selection)
		require.NoError(t, report.Err)
		require.EqualValues(t, resolved, report.Total, "native pass %d", pass+1)
		require.Equal(t, want, rebuildEventSQLiteRows(native), "native pass %d", pass+1)
		stats, err := RunRebuildNgRxEffect(t.Context(), b, receipts, frozen)
		require.NoError(t, err)
		require.Equal(t, store_pebble.RebuildNgRxEffectStats{
			Stubs: uint64(len(moves)), Resolved: resolved, Moves: wantMoves,
		}, stats, "private pass %d", pass+1)
		require.Equal(t, want, rebuildEventSavedRows(t, b), "private pass %d", pass+1)
	}
}

func rebuildNgRxEffectCall(from, file, action string, line int) *graph.Edge {
	return &graph.Edge{
		From: from, To: "unresolved::*." + action, Kind: graph.EdgeCalls, FilePath: file, Line: line,
		Meta: map[string]any{"via": "ngrx-effect", "ngrx_action": action},
	}
}

func TestRebuildNgRxEffectRefusesUnprovedMove(t *testing.T) {
	const source, remote = "effects.ts", "actions.ts"
	caller := rebuildEventNode(source, "effect", "typescript", "workspace")
	local := rebuildEventNode(source, "Load", "typescript", caller.WorkspaceID)
	target := rebuildEventNode(remote, local.Name, local.Language, caller.WorkspaceID)
	getter := rebuildEventNode(remote, "useStore", local.Language, "")
	getter.Kind = graph.KindVariable
	call := &graph.Edge{From: caller.ID, To: "unresolved::*.Load", Kind: graph.EdgeCalls, FilePath: source, Line: 5,
		Meta: map[string]any{"via": "ngrx-effect", "ngrx_action": local.Name, "store_binding": getter.Name}}
	left := rebuildEventFacts(source, local.Language, []*graph.Node{caller, local}, call)
	right := rebuildEventFacts(remote, target.Language, []*graph.Node{target, getter})
	first := graph.EdgeIdentityFor(call)
	first.To = target.ID
	for _, mismatch := range []string{"raw action name", "winning declaration name", "losing declaration name", "getter eligibility"} {
		t.Run(mismatch, func(t *testing.T) {
			b := openRebuildGRPCBuild(t, left, right)
			before := rebuildEventSavedRows(t, b)
			selection, err := NewFrameworkSynthesizerSelection([]string{SynthNgRxEffect})
			require.NoError(t, err)
			frozen, err := FreezeRebuildFrameworkSelection(t.Context(), b.RebuildView(), selection)
			require.NoError(t, err)
			require.True(t, frozen.allows(SynthNgRxEffect))
			l, r := left, right
			switch mismatch {
			case "raw action name":
				e := *call
				e.Meta = maps.Clone(call.Meta)
				e.Meta["ngrx_action"] = "Other"
				l.Edges = []*graph.Edge{&e}
			case "winning declaration name":
				n := *target
				n.Name = "Other"
				r.Nodes = []*graph.Node{&n, getter}
			case "losing declaration name":
				n := *local
				n.Name = "Other"
				l.Nodes = []*graph.Node{caller, &n}
			case "getter eligibility":
				n := *getter
				n.Meta = map[string]any{"store_factory": false}
				r.Nodes = []*graph.Node{target, &n}
			}
			stats, err := RunRebuildNgRxEffect(t.Context(), b, rebuildGRPCReceipts(l, r), frozen)
			if mismatch == "losing declaration name" {
				require.NoError(t, err)
				require.Equal(t, store_pebble.RebuildNgRxEffectStats{Stubs: 1, Resolved: 1, Moves: 1}, stats)
				requireRebuildStoreCandidateMove(t, b, before, call, target.ID, SynthNgRxEffect, ConfidenceHeuristic)
				return
			}
			var refusal *store_pebble.RebuildUnknownProvenanceError
			require.ErrorIs(t, err, store_pebble.ErrRebuildUnknownProvenance)
			require.ErrorAs(t, err, &refusal)
			require.Equal(t, &store_pebble.RebuildUnknownProvenanceError{Count: 1, First: first}, refusal)
			require.Zero(t, stats)
			require.Equal(t, before, rebuildEventSavedRows(t, b))
		})
	}
}

func TestRebuildNgRxEffectEmptyCohort(t *testing.T) {
	const file, lang = "effects.ts", "typescript"
	caller := rebuildEventNode(file, "effect", lang, "")
	other := rebuildEventNode(file, "Other", lang, "")
	call := rebuildNgRxEffectCall(caller.ID, file, "Load", 9)
	call.To = other.ID
	call.Origin = graph.OriginASTInferred
	call.Confidence, call.ConfidenceLabel = 0.6, "INFERRED"
	call.Meta[MetaSynthesizedBy], call.Meta[MetaProvenance] = SynthNgRxEffect, ProvenanceHeuristic
	wantTo := "unresolved::*.Load"
	facts := rebuildEventFacts(file, lang, []*graph.Node{caller, other}, call)
	t.Run("exact Call proves activation", func(t *testing.T) {
		requireRebuildNgRxEffectOracle(t, []store_pebble.ParseFacts{facts},
			map[graph.EdgeIdentity]string{graph.EdgeIdentityFor(call): wantTo}, 0)
	})
	t.Run("known declaration enters deciding bucket", func(t *testing.T) {
		b := openRebuildGRPCBuild(t, facts)
		before := rebuildEventSavedRows(t, b)
		selection, err := NewFrameworkSynthesizerSelection([]string{SynthNgRxEffect})
		require.NoError(t, err)
		frozen, err := FreezeRebuildFrameworkSelection(t.Context(), b.RebuildView(), selection)
		require.NoError(t, err)
		require.True(t, frozen.allows(SynthNgRxEffect))
		changed := *other
		changed.Name = "Load"
		recorded := rebuildEventFacts(file, lang, []*graph.Node{caller, &changed}, call)
		stats, err := RunRebuildNgRxEffect(t.Context(), b, rebuildGRPCReceipts(recorded), frozen)
		first := graph.EdgeIdentityFor(call)
		first.To = wantTo
		var refusal *store_pebble.RebuildUnknownProvenanceError
		require.ErrorAs(t, err, &refusal)
		require.Equal(t, &store_pebble.RebuildUnknownProvenanceError{Count: 1, First: first}, refusal)
		require.Zero(t, stats)
		require.Equal(t, before, rebuildEventSavedRows(t, b))
	})
}
