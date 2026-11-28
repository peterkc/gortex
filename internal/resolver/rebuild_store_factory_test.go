package resolver

import (
	"context"
	"iter"
	"maps"
	"slices"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/graph/store_pebble"
	"github.com/zzet/gortex/internal/parser/languages"
)

func TestRebuildStoreFactorySQLiteOracle(t *testing.T) {
	// The indexed fixture TestStoreFactory_Zustand_TS, before main linking.
	const store = `import { create } from 'zustand'

export const useStore = create((set, get) => ({
  fetchUser: async () => {},
  reset: () => {},
}))
`
	const caller = `import { useStore } from './store'

export function hardReset() {
  useStore.getState().reset()
}

export function loginFlow() {
  const { fetchUser } = useStore.getState()
  fetchUser()
}
`
	facts := make([]store_pebble.ParseFacts, 0, 2)
	extractor := languages.NewTypeScriptExtractor()
	for _, src := range []struct{ file, body string }{{"store.ts", store}, {"caller.ts", caller}} {
		parsed, err := extractor.Extract(src.file, []byte(src.body))
		require.NoError(t, err)
		t.Cleanup(parsed.ReleaseTree)
		facts = append(facts, roundTripRebuildRNBridgeFacts(t,
			rebuildEventFacts(src.file, extractor.Language(), parsed.Nodes, parsed.Edges...)))
	}

	moves := map[graph.EdgeIdentity]string{}
	for _, want := range []struct {
		caller, member, target, actionID string
		line                             int
	}{
		{"hardReset", "reset", "unresolved::*.reset", "store.ts::useStore.reset@5", 4},
		{"loginFlow", "fetchUser", "unresolved::fetchUser", "store.ts::useStore.fetchUser@4", 9},
	} {
		key := graph.EdgeIdentity{From: "caller.ts::" + want.caller, To: want.target,
			Kind: graph.EdgeCalls, FilePath: "caller.ts", Line: want.line}
		i := slices.IndexFunc(facts[1].Edges, func(e *graph.Edge) bool { return graph.EdgeIdentityFor(e) == key })
		require.NotEqual(t, -1, i, "%+v", key)
		require.Equal(t, "store-factory", facts[1].Edges[i].Meta["via"])
		require.Equal(t, "useStore", facts[1].Edges[i].Meta["store_binding"])
		require.Equal(t, want.member, facts[1].Edges[i].Meta["store_action"])
		i = slices.IndexFunc(facts[0].Nodes, func(n *graph.Node) bool { return n.ID == want.actionID })
		require.NotEqual(t, -1, i, "%s", want.actionID)
		action := facts[0].Nodes[i]
		require.Equal(t, graph.KindFunction, action.Kind)
		require.Equal(t, "useStore", action.Meta["store_factory"])
		require.Equal(t, want.member, action.Meta["store_member"])
		moves[key] = action.ID
	}

	requireRebuildStoreFactoryOracle(t, facts, moves, 2, 2)
}

func TestRebuildStoreFactorySQLiteElection(t *testing.T) {
	const callerFile, getterFile = "caller.ts", "getter.ts"
	const callerID, callerAction, getterAction = callerFile + "::call", callerFile + "::reset", getterFile + "::reset"
	for _, tc := range []struct {
		name, target string
	}{
		{"getter beats caller file", getterAction},
		{"ambiguous getter uses caller file", callerAction},
		{"workspace masks getter action", callerAction},
	} {
		t.Run(tc.name, func(t *testing.T) {
			// Surgical facts: raw extraction binds same-file actions before #16.
			caller := rebuildEventNode(callerFile, "call", "typescript", "local")
			local := rebuildStoreFactoryAction(callerFile, "local")
			remote := rebuildStoreFactoryAction(getterFile, "local")
			getter := rebuildEventNode(getterFile, "useStore", "typescript", "local")
			getter.Kind = graph.KindConstant
			call := rebuildStoreFactoryMarker(callerFile, callerID, "unresolved::reset", "reset")
			calls := []*graph.Edge{call}
			moves := map[graph.EdgeIdentity]string{graph.EdgeIdentityFor(call): tc.target}
			var extra []store_pebble.ParseFacts
			switch tc.name {
			case "ambiguous getter uses caller file":
				// Move the competing getter to a third file so fallback, not
				// choosing the caller's getter, is necessary to find the action.
				other := rebuildEventNode("other.ts", "useStore", "typescript", "local")
				other.Kind = graph.KindVariable
				extra = append(extra, rebuildEventFacts(other.FilePath, "typescript", []*graph.Node{other}))
			case "workspace masks getter action":
				remote.WorkspaceID = "foreign"
				getter.WorkspaceID = "foreign"
				// With no caller node, native does not apply the workspace mask.
				unknown := rebuildStoreFactoryMarker(callerFile, callerFile+"::unknown", "unresolved::reset", "reset")
				calls = append(calls, unknown)
				moves[graph.EdgeIdentityFor(unknown)] = getterAction
			}
			facts := []store_pebble.ParseFacts{
				rebuildEventFacts(callerFile, "typescript", []*graph.Node{caller, local}, calls...),
				rebuildEventFacts(getterFile, "typescript", []*graph.Node{getter, remote}),
			}
			count := uint64(len(calls))
			requireRebuildStoreFactoryOracle(t, append(facts, extra...), moves, count, count)
		})
	}
}

func TestRebuildStoreFactorySQLiteNoMatch(t *testing.T) {
	const file = "stale.ts"
	const oldTarget, restub = file + "::former", "unresolved::*.removed"
	for _, tc := range []struct {
		name, language string
		actionTable    bool
	}{
		{"nonempty join restubs", "typescript", true},
		{"empty action table skips calls and receipts", "typescript", false},
		{"frozen selection excludes no-web graph", "go", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			// Surgical facts model a marker on a previously synthesized edge.
			caller := rebuildEventNode(file, "call", tc.language, "")
			former := rebuildEventNode(file, "former", tc.language, "")
			stub := rebuildStoreFactoryMarker(file, caller.ID, oldTarget, "removed")
			stub.Origin = graph.OriginASTInferred
			stub.Confidence = 0.75
			stub.ConfidenceLabel = graph.ConfidenceLabelFor(graph.EdgeCalls, 0.75)
			stub.Meta[MetaSynthesizedBy] = SynthStoreFactory
			stub.Meta[MetaProvenance] = ProvenanceHeuristic
			stub.Meta["fixture"] = "must survive restub"
			nodes := []*graph.Node{caller, former}
			if tc.actionTable {
				unrelated := rebuildStoreFactoryAction(file, "")
				unrelated.Language = tc.language
				nodes = append(nodes, unrelated)
			}
			facts := roundTripRebuildRNBridgeFacts(t, rebuildEventFacts(file, tc.language, nodes, stub))
			if tc.actionTable && tc.language == "typescript" {
				requireRebuildStoreFactoryOracle(t, []store_pebble.ParseFacts{facts},
					map[graph.EdgeIdentity]string{graph.EdgeIdentityFor(stub): restub}, 1, 0)
				return
			}

			native := openRebuildTemporalOracle(t, facts)
			private := openRebuildGRPCBuild(t, facts)
			before := rebuildEventSQLiteRows(native)
			require.Equal(t, before, rebuildEventSavedRows(t, private))
			selection, err := NewFrameworkSynthesizerSelection([]string{SynthStoreFactory})
			require.NoError(t, err)
			frozen, err := FreezeRebuildFrameworkSelection(t.Context(), private.RebuildView(), selection)
			require.NoError(t, err)
			require.Equal(t, tc.language == "typescript", frozen.allows(SynthStoreFactory))

			report := RunFrameworkSynthesizersWithSelection(native, selection)
			require.NoError(t, report.Err)
			require.Zero(t, report.Total)
			require.Equal(t, before, rebuildEventSQLiteRows(native))
			receipts := func(_ func(store_pebble.ParseFacts, error) bool) {
				t.Fatal("an empty or excluded Store Factory turn must not consume receipts")
			}
			stats, err := RunRebuildStoreFactory(t.Context(), private, receipts, frozen)
			require.NoError(t, err)
			require.Zero(t, stats)
			require.Equal(t, before, rebuildEventSavedRows(t, private))
		})
	}
}

func TestRebuildStoreFactoryOrder(t *testing.T) {
	const file = "order.ts"
	const reset, start, middle, end = file + "::reset", file + "::start", file + "::middle", file + "::end"
	type call struct{ old, member, payload string }
	for _, tc := range []struct {
		name  string
		calls []call
		moves uint64
		want  map[string]any
	}{
		{
			name: "single convergence",
			calls: []call{
				{"unresolved::reset", "reset", "first payload"},
				{"unresolved::*.reset", "reset", "second payload"},
			},
			moves: 2,
			want:  map[string]any{reset: "first payload"},
		},
		{
			name: "single convergence reversed",
			calls: []call{
				{"unresolved::*.reset", "reset", "second payload"},
				{"unresolved::reset", "reset", "first payload"},
			},
			moves: 2,
			want:  map[string]any{reset: "second payload"},
		},
		{
			// The first receipt targets middle, a moving old key that sorts before start.
			name: "chain",
			calls: []call{
				{start, "middle", "from start"},
				{middle, "end", "from middle"},
			},
			moves: 2,
			want:  map[string]any{middle: "from start", end: "from middle"},
		},
		{
			name: "cycle",
			calls: []call{
				{start, "middle", "from start"},
				{middle, "start", "from middle"},
			},
			moves: 2,
			want:  map[string]any{middle: "from start", start: "from middle"},
		},
		{
			// Both winners arrive before their lexically earlier competitors.
			name: "independent convergences",
			calls: []call{
				{"unresolved::z", "middle", "middle first"},
				{"unresolved::y", "end", "end first"},
				{"unresolved::a", "middle", "middle second"},
				{"unresolved::b", "end", "end second"},
			},
			moves: 4,
			want:  map[string]any{middle: "middle first", end: "end first"},
		},
		{
			name: "independent convergences reversed",
			calls: []call{
				{"unresolved::b", "end", "end second"},
				{"unresolved::a", "middle", "middle second"},
				{"unresolved::y", "end", "end first"},
				{"unresolved::z", "middle", "middle first"},
			},
			moves: 4,
			want:  map[string]any{middle: "middle second", end: "end second"},
		},
		{
			name: "stationary incumbent",
			calls: []call{
				{start, "reset", "incoming"},
				{reset, "reset", "incumbent"},
			},
			moves: 1,
			want:  map[string]any{reset: "incumbent"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			caller := rebuildEventNode(file, "call", "typescript", "")
			nodes := []*graph.Node{caller}
			for _, member := range []string{"reset", "start", "middle", "end"} {
				action := rebuildEventNode(file, member, "typescript", "")
				action.Meta = map[string]any{"store_factory": "useStore"}
				nodes = append(nodes, action)
			}
			edges := make([]*graph.Edge, 0, len(tc.calls))
			for _, call := range tc.calls {
				e := rebuildStoreFactoryMarker(file, caller.ID, call.old, call.member)
				e.Meta["fixture"] = call.payload
				edges = append(edges, e)
			}
			// Seed SQLite and replay receipts with the same order, not old-key order.
			facts := roundTripRebuildRNBridgeFacts(t, rebuildEventFacts(file, "typescript", nodes, edges...))
			b, native := openRebuildEventStores(t, facts)
			require.Equal(t, rebuildEventSQLiteRows(native), rebuildEventSavedRows(t, b))
			selection, err := NewFrameworkSynthesizerSelection([]string{SynthStoreFactory})
			require.NoError(t, err)
			frozen, err := FreezeRebuildFrameworkSelection(t.Context(), b.RebuildView(), selection)
			require.NoError(t, err)
			require.True(t, frozen.allows(SynthStoreFactory))

			stats, err := RunRebuildStoreFactory(t.Context(), b, rebuildGRPCReceipts(facts), frozen)
			require.NoError(t, err)
			require.Equal(t, store_pebble.RebuildStoreFactoryStats{Stubs: uint64(len(tc.calls)), Resolved: uint64(len(tc.calls)), Moves: tc.moves}, stats)
			require.Equal(t, len(tc.calls), ResolveStoreFactoryCalls(native))
			want := rebuildEventSQLiteRows(native)
			require.Len(t, want.edges, len(tc.want))
			payloads := make(map[string]any, len(want.edges))
			for _, e := range want.edges {
				payloads[e.To] = e.Meta["fixture"]
			}
			require.Equal(t, tc.want, payloads)
			require.Equal(t, want, rebuildEventSavedRows(t, b))
		})
	}
}

func TestRebuildStoreDispatchReceiptOnlyCaller(t *testing.T) {
	const file, lang = "store.ts", "typescript"
	caller := rebuildEventNode(file, "submit", lang, "local")
	foreign := *caller
	foreign.WorkspaceID = "foreign"
	for _, stage := range []struct {
		name   string
		target *graph.Node
		call   *graph.Edge
		native func(graph.Store) int
		run    func(
			context.Context, *store_pebble.BuildStore, iter.Seq2[store_pebble.ParseFacts, error], FrameworkSynthesizerSelection,
		) (store_pebble.RebuildStoreFactoryStats, error)
	}{
		{SynthStoreFactory, rebuildStoreFactoryAction(file, "local"), rebuildStoreFactoryMarker(file, caller.ID, "unresolved::*.reset", "reset"), ResolveStoreFactoryCalls, RunRebuildStoreFactory},
		{SynthVuexDispatch, rebuildVuexTestNode(file, "action", "", "reset", "action", graph.KindFunction), rebuildVuexTestCall(caller.ID, "unresolved::*.reset", file, "reset", "action", 9), ResolveVuexDispatchCalls, RunRebuildVuexDispatch},
		{SynthNgRxEffect, rebuildEventNode(file, "reset", lang, "local"), rebuildNgRxEffectCall(caller.ID, file, "reset", 9), ResolveNgRxEffects, RunRebuildNgRxEffect},
	} {
		t.Run(stage.name, func(t *testing.T) {
			stage.target.WorkspaceID = "local"
			input := rebuildEventFacts(file, lang, []*graph.Node{stage.target}, stage.call)
			selection, err := NewFrameworkSynthesizerSelection([]string{stage.name})
			require.NoError(t, err)
			for _, tc := range []struct {
				name   string
				caller *graph.Node
			}{
				{"absent caller", nil},
				{"matching workspace", caller},
				{"other workspace", &foreign},
			} {
				t.Run(tc.name, func(t *testing.T) {
					b := openRebuildGRPCBuild(t, input)
					before := rebuildEventSavedRows(t, b)
					receipt := input
					if tc.caller != nil {
						receipt.Nodes = []*graph.Node{stage.target, tc.caller}
					}
					native := openRebuildTemporalOracle(t, receipt)
					resolved := stage.native(native)
					stats, err := stage.run(t.Context(), b, rebuildGRPCReceipts(receipt), selection)
					if tc.caller == &foreign {
						require.Zero(t, resolved)
						var refused *store_pebble.RebuildUnknownProvenanceError
						require.ErrorAs(t, err, &refused)
						want := graph.EdgeIdentityFor(stage.call)
						want.To = stage.target.ID
						require.Equal(t, &store_pebble.RebuildUnknownProvenanceError{Count: 1, First: want}, refused)
						require.Zero(t, stats)
						require.Equal(t, before, rebuildEventSavedRows(t, b))
						return
					}
					require.Equal(t, 1, resolved)
					require.NoError(t, err)
					require.Equal(t, store_pebble.RebuildStoreFactoryStats{Stubs: 1, Resolved: 1, Moves: 1}, stats)
					require.Equal(t, rebuildEventSQLiteRows(native).edges, rebuildEventSavedRows(t, b).edges)
				})
			}
		})
	}
}

func TestRebuildStoreDispatchReceiptOnlyCallerAnchor(t *testing.T) {
	const file, lang, reset = "caller.ts", "typescript", "unresolved::*.reset"
	caller := rebuildEventNode(file, "submit", lang, "caller")
	for _, stage := range []struct {
		name   string
		action func(file, workspace string) *graph.Node
		call   *graph.Edge
		native func(graph.Store) int
		run    func(
			context.Context, *store_pebble.BuildStore, iter.Seq2[store_pebble.ParseFacts, error], FrameworkSynthesizerSelection,
		) (store_pebble.RebuildStoreFactoryStats, error)
	}{
		{
			name:   SynthStoreFactory,
			action: rebuildStoreFactoryAction,
			call:   rebuildStoreFactoryMarker(file, caller.ID, "unresolved::reset", "reset"),
			native: ResolveStoreFactoryCalls,
			run:    RunRebuildStoreFactory,
		},
		{
			name: SynthVuexDispatch,
			action: func(file, workspace string) *graph.Node {
				n := rebuildVuexTestNode(file, "action", "", "reset", "action", graph.KindFunction)
				n.WorkspaceID = workspace
				return n
			},
			call:   rebuildVuexTestCall(caller.ID, "unresolved::reset", file, "reset", "action", 9),
			native: ResolveVuexDispatchCalls,
			run:    RunRebuildVuexDispatch,
		},
	} {
		t.Run(stage.name, func(t *testing.T) {
			// Globally there are two actions and no file winner. The caller
			// exists only in the receipts, and its workspace has neither action.
			first := rebuildEventFacts("first.ts", lang, []*graph.Node{stage.action("first.ts", "first")})
			second := rebuildEventFacts("second.ts", lang, []*graph.Node{stage.action("second.ts", "second")})
			live := rebuildEventFacts(file, lang, nil, stage.call)
			receipt := rebuildEventFacts(file, lang, []*graph.Node{caller}, stage.call)
			b := openRebuildGRPCBuild(t, first, second, live)
			native := openRebuildTemporalOracle(t, first, second, receipt)
			selection, err := NewFrameworkSynthesizerSelection([]string{stage.name})
			require.NoError(t, err)

			require.Zero(t, stage.native(native))
			want := rebuildEventSQLiteRows(native).edges
			require.Len(t, want, 1)
			moved := graph.EdgeIdentityFor(stage.call)
			moved.To = reset
			require.Equal(t, moved, graph.EdgeIdentityFor(want[0]))

			stats, err := stage.run(t.Context(), b, rebuildGRPCReceipts(first, second, receipt), selection)
			require.NoError(t, err)
			require.Equal(t, store_pebble.RebuildStoreFactoryStats{Stubs: 1, Moves: 1}, stats)
			require.Equal(t, want, rebuildEventSavedRows(t, b).edges)
		})
	}
}

func TestRebuildStoreFactoryRefusesUnprovedMove(t *testing.T) {
	const callerFile, storeFile = "caller.ts", "store.ts"
	caller := rebuildEventNode(callerFile, "call", "typescript", "workspace")
	local := rebuildStoreFactoryAction(callerFile, caller.WorkspaceID)
	remote := rebuildStoreFactoryAction(storeFile, caller.WorkspaceID)
	getter := rebuildEventNode(storeFile, "useStore", "typescript", "")
	getter.Kind = graph.KindVariable
	call := rebuildStoreFactoryMarker(callerFile, caller.ID, "unresolved::reset", "reset")
	callerFacts := rebuildEventFacts(callerFile, "typescript", []*graph.Node{caller, local}, call)
	storeFacts := rebuildEventFacts(storeFile, "typescript", []*graph.Node{remote, getter})
	for _, input := range []string{"caller workspace", "loser workspace", "getter eligibility", "raw marker", "raw binding"} {
		t.Run(input, func(t *testing.T) {
			b := openRebuildGRPCBuild(t, callerFacts, storeFacts)
			before := rebuildEventSavedRows(t, b)
			selection, err := NewFrameworkSynthesizerSelection([]string{SynthStoreFactory})
			require.NoError(t, err)
			frozen, err := FreezeRebuildFrameworkSelection(t.Context(), b.RebuildView(), selection)
			require.NoError(t, err)
			require.True(t, frozen.allows(SynthStoreFactory))
			left, right := callerFacts, storeFacts
			left.Nodes = slices.Clone(left.Nodes)
			right.Nodes = slices.Clone(right.Nodes)
			switch input {
			case "caller workspace":
				n := *caller
				n.WorkspaceID = "other"
				left.Nodes[0] = &n
			case "loser workspace":
				n := *local
				n.WorkspaceID = "other"
				left.Nodes[1] = &n
			case "getter eligibility":
				n := *getter
				n.Meta = map[string]any{"store_factory": false}
				right.Nodes[1] = &n
			case "raw marker", "raw binding":
				e := *call
				e.Meta = maps.Clone(e.Meta)
				field := "store_action"
				if input == "raw binding" {
					field = "store_binding"
				}
				e.Meta[field] = "other"
				left.Edges = []*graph.Edge{&e}
			}
			stats, err := RunRebuildStoreFactory(t.Context(), b, rebuildGRPCReceipts(left, right), frozen)
			if input == "loser workspace" {
				require.NoError(t, err)
				require.Equal(t, store_pebble.RebuildStoreFactoryStats{Stubs: 1, Resolved: 1, Moves: 1}, stats)
				requireRebuildStoreCandidateMove(t, b, before, call, remote.ID, SynthStoreFactory, 0.75)
				return
			}
			require.ErrorIs(t, err, store_pebble.ErrRebuildUnknownProvenance)
			var refusal *store_pebble.RebuildUnknownProvenanceError
			require.ErrorAs(t, err, &refusal)
			first := graph.EdgeIdentityFor(call)
			first.To = remote.ID
			require.Equal(t, &store_pebble.RebuildUnknownProvenanceError{Count: 1, First: first}, refusal)
			require.Zero(t, stats)
			require.Equal(t, before, rebuildEventSavedRows(t, b))
		})
	}
}

func requireRebuildStoreFactoryOracle(t *testing.T, facts []store_pebble.ParseFacts,
	moves map[graph.EdgeIdentity]string, stubs, resolved uint64,
) {
	t.Helper()
	native := openRebuildTemporalOracle(t, facts...)
	private := openRebuildGRPCBuild(t, facts...)
	before := rebuildEventSQLiteRows(native)
	require.Equal(t, before, rebuildEventSavedRows(t, private))
	selection, err := NewFrameworkSynthesizerSelection([]string{SynthStoreFactory})
	require.NoError(t, err)
	frozen, err := FreezeRebuildFrameworkSelection(t.Context(), private.RebuildView(), selection)
	require.NoError(t, err)
	require.True(t, frozen.allows(SynthStoreFactory))

	want := before
	want.edges = slices.Clone(before.edges)
	var changed int
	for i, e := range want.edges {
		to, ok := moves[graph.EdgeIdentityFor(e)]
		if !ok {
			continue
		}
		moved := *e
		moved.To = to
		moved.Origin = graph.OriginASTInferred
		moved.Meta = maps.Clone(e.Meta)
		if strings.HasPrefix(to, "unresolved::") {
			moved.Confidence, moved.ConfidenceLabel = 0, ""
			delete(moved.Meta, MetaSynthesizedBy)
			delete(moved.Meta, MetaProvenance)
		} else {
			moved.Confidence = 0.75
			moved.ConfidenceLabel = graph.ConfidenceLabelFor(graph.EdgeCalls, 0.75)
			moved.Meta[MetaSynthesizedBy] = SynthStoreFactory
			moved.Meta[MetaProvenance] = ProvenanceHeuristic
		}
		want.edges[i] = &moved
		changed++
	}
	require.Len(t, moves, changed)
	want.sort()

	receipts := rebuildGRPCReceipts(facts...)
	for _, wantMoves := range []uint64{uint64(len(moves)), 0} {
		report := RunFrameworkSynthesizersWithSelection(native, selection)
		require.NoError(t, report.Err)
		require.EqualValues(t, resolved, report.Total)
		gotNative := rebuildEventSQLiteRows(native)
		require.Equal(t, want, gotNative)
		// Destinations are disjoint from all old keys in these fixtures,
		// so missing old keys count the native moves at this turn.
		live := make(map[graph.EdgeIdentity]struct{}, len(gotNative.edges))
		for _, e := range gotNative.edges {
			live[graph.EdgeIdentityFor(e)] = struct{}{}
		}
		var nativeMoves uint64
		for _, e := range before.edges {
			if _, ok := live[graph.EdgeIdentityFor(e)]; !ok {
				nativeMoves++
			}
		}
		require.Equal(t, wantMoves, nativeMoves)
		stats, err := RunRebuildStoreFactory(t.Context(), private, receipts, frozen)
		require.NoError(t, err)
		require.Equal(t, stubs, stats.Stubs)
		require.Equal(t, resolved, stats.Resolved)
		require.Equal(t, nativeMoves, stats.Moves)
		require.Equal(t, gotNative, rebuildEventSavedRows(t, private))
		before = gotNative
	}
}

func rebuildStoreFactoryAction(file, workspace string) *graph.Node {
	n := rebuildEventNode(file, "reset", "typescript", workspace)
	// Missing store_member uses the node name.
	n.Meta = map[string]any{"store_factory": "useStore"}
	return n
}

func rebuildStoreFactoryMarker(file, from, to, member string) *graph.Edge {
	return &graph.Edge{From: from, To: to, Kind: graph.EdgeCalls, FilePath: file, Line: 9,
		Meta: map[string]any{"via": "store-factory", "store_binding": "useStore", "store_action": member}}
}

func TestRebuildStoreFactoryAndReduxThunkVueReceipts(t *testing.T) {
	const vue = `<script setup lang="ts">
import { createAsyncThunk } from '@reduxjs/toolkit';
import { useStore } from './store';
export function hardReset() { useStore.getState().reset(); }
const outer = createAsyncThunk('outer', async (_, { dispatch }) => {
  dispatch(begin());
});
</script>
`
	const store = `import { create } from 'zustand';
import { createAsyncThunk } from '@reduxjs/toolkit';
export const useStore = create((set, get) => ({
  reset: () => {},
}));
export const begin = createAsyncThunk('begin', async () => {});
`
	extractor := languages.NewVueExtractor()
	parsed, err := extractor.Extract("Caller.vue", []byte(vue))
	require.NoError(t, err)
	t.Cleanup(parsed.ReleaseTree)
	parsedStore, err := languages.NewTypeScriptExtractor().Extract("store.ts", []byte(store))
	require.NoError(t, err)
	t.Cleanup(parsedStore.ReleaseTree)
	callerFacts := roundTripRebuildRNBridgeFacts(t,
		rebuildEventFacts("Caller.vue", extractor.Language(), parsed.Nodes, parsed.Edges...))
	storeFacts := roundTripRebuildRNBridgeFacts(t,
		rebuildEventFacts("store.ts", "typescript", parsedStore.Nodes, parsedStore.Edges...))
	for _, tc := range []struct{ synth, target string }{
		{SynthStoreFactory, "store.ts::useStore.reset@4"},
		{SynthReduxThunk, "store.ts::begin"},
	} {
		t.Run(tc.synth, func(t *testing.T) {
			var calls []*graph.Edge
			for _, edge := range callerFacts.Edges {
				if edge.Kind == graph.EdgeCalls && edge.Meta["via"] == tc.synth {
					calls = append(calls, edge)
				}
			}
			require.Len(t, calls, 1)
			require.Equal(t, "Caller.vue", calls[0].FilePath)
			old := graph.EdgeIdentityFor(calls[0])
			facts := []store_pebble.ParseFacts{callerFacts, storeFacts}
			moves := map[graph.EdgeIdentity]string{old: tc.target}
			if tc.synth == SynthStoreFactory {
				requireRebuildStoreFactoryOracle(t, facts, moves, 1, 1)
			} else {
				requireRebuildReduxThunkOracle(t, facts, moves, 1, 1)
			}

			b := openRebuildGRPCBuild(t, facts...)
			before := rebuildEventSavedRows(t, b)
			selection, err := NewFrameworkSynthesizerSelection([]string{tc.synth})
			require.NoError(t, err)
			frozen, err := FreezeRebuildFrameworkSelection(t.Context(), b.RebuildView(), selection)
			require.NoError(t, err)
			// Keep the declaration receipts exact. Only the Call receipt
			// loses its effective language.
			decls, bad := callerFacts, callerFacts
			decls.Edges = nil
			bad.Nodes, bad.EffectiveLanguage = nil, ""
			receipts := rebuildGRPCReceipts(decls, bad, storeFacts)
			if tc.synth == SynthStoreFactory {
				stats, proofErr := RunRebuildStoreFactory(t.Context(), b, receipts, frozen)
				require.Zero(t, stats)
				err = proofErr
			} else {
				stats, proofErr := RunRebuildReduxThunk(t.Context(), b, receipts, frozen)
				require.Zero(t, stats)
				err = proofErr
			}
			var refusal *store_pebble.RebuildUnknownProvenanceError
			require.ErrorAs(t, err, &refusal)
			old.To = tc.target
			require.Equal(t, &store_pebble.RebuildUnknownProvenanceError{Count: 1, First: old}, refusal)
			require.Equal(t, before, rebuildEventSavedRows(t, b))
		})
	}
}

func TestRebuildStoreFactoryAndReduxThunkCandidateProof(t *testing.T) {
	const callerFile, getterFile, peerFile = "caller.ts", "getter.ts", "peer.ts"
	for _, synth := range []string{SynthStoreFactory, SynthReduxThunk} {
		t.Run(synth, func(t *testing.T) {
			for _, mode := range []string{"two exact peers", "three exact peers", "duplicate receipt", "scope enters winning file",
				"entrant into ambiguous file", "empty getter file gains a candidate"} {
				t.Run(mode, func(t *testing.T) {
					var workspace string
					if mode == "scope enters winning file" || mode == "entrant into ambiguous file" {
						workspace = "local"
					}
					caller := rebuildEventNode(callerFile, "call", "typescript", workspace)
					winner := rebuildStoreFactoryAction(callerFile, caller.WorkspaceID)
					getter := rebuildEventNode(getterFile, "useStore", caller.Language, "")
					getter.Kind = graph.KindVariable
					call := rebuildStoreFactoryMarker(callerFile, caller.ID, "unresolved::reset", "reset")
					if synth == SynthReduxThunk {
						call = rebuildReduxThunkMarker(caller, "reset", 9)
						call.Meta["store_binding"] = "useStore"
					}
					file := getterFile
					if mode == "empty getter file gains a candidate" {
						file = peerFile
					}
					var peers []*graph.Node
					for _, suffix := range []string{"@1", "@2", "@3"} {
						n := rebuildStoreFactoryAction(file, caller.WorkspaceID)
						if synth == SynthReduxThunk {
							n = rebuildReduxThunkNode(file, "reset")
							n.WorkspaceID = caller.WorkspaceID
						}
						n.ID += suffix
						peers = append(peers, n)
					}
					want := winner.ID
					if mode == "scope enters winning file" {
						peers[1].WorkspaceID, peers[2].WorkspaceID = "foreign", "foreign"
						want = peers[0].ID
					}
					if mode == "entrant into ambiguous file" {
						peers[2].WorkspaceID = "foreign"
					}
					left := rebuildEventFacts(callerFile, caller.Language, []*graph.Node{caller, winner}, call)
					rightNodes := []*graph.Node{getter}
					if file == getterFile {
						rightNodes = append(rightNodes, peers...)
					}
					live := []store_pebble.ParseFacts{left, rebuildEventFacts(getterFile, caller.Language, rightNodes)}
					if file == peerFile {
						live = append(live, rebuildEventFacts(peerFile, caller.Language, peers))
					}
					b := openRebuildGRPCBuild(t, live...)
					before := rebuildEventSavedRows(t, b)
					selection, err := NewFrameworkSynthesizerSelection([]string{synth})
					require.NoError(t, err)
					frozen, err := FreezeRebuildFrameworkSelection(t.Context(), b.RebuildView(), selection)
					require.NoError(t, err)
					// Native falls through two getter-file peers. A duplicate
					// receipt cannot prove the second peer. A foreign peer
					// moved into the winning scope instead breaks a singleton.
					rightNodes = []*graph.Node{getter, peers[0], peers[1]}
					switch mode {
					case "three exact peers":
						rightNodes = append(rightNodes, peers[2])
					case "duplicate receipt":
						rightNodes[2] = peers[0]
					case "scope enters winning file":
						n := *peers[1]
						n.WorkspaceID = caller.WorkspaceID
						rightNodes[2] = &n
					case "entrant into ambiguous file":
						n := *peers[2]
						n.WorkspaceID = caller.WorkspaceID
						rightNodes = append(rightNodes, &n)
					case "empty getter file gains a candidate":
						n := *peers[0]
						n.FilePath = getterFile
						rightNodes = []*graph.Node{getter, &n}
					}
					recorded := []store_pebble.ParseFacts{left, rebuildEventFacts(getterFile, caller.Language, rightNodes)}
					if file == peerFile {
						// Keep the scoped pool ambiguous independently of the
						// entrant into its otherwise empty getter-file bucket.
						recorded = append(recorded, rebuildEventFacts(peerFile, caller.Language, peers[1:]))
					}
					receipts := rebuildGRPCReceipts(recorded...)
					var moves uint64
					confidence := 0.75
					if synth == SynthStoreFactory {
						stats, proofErr := RunRebuildStoreFactory(t.Context(), b, receipts, frozen)
						moves, err = stats.Moves, proofErr
					} else {
						stats, proofErr := RunRebuildReduxThunk(t.Context(), b, receipts, frozen)
						moves, err = stats.Moves, proofErr
						confidence = ConfidenceHeuristic
					}
					if mode == "two exact peers" || mode == "three exact peers" || mode == "entrant into ambiguous file" {
						require.NoError(t, err)
						require.Equal(t, uint64(1), moves)
						requireRebuildStoreCandidateMove(t, b, before, call, want, synth, confidence)
						return
					}
					first := graph.EdgeIdentityFor(call)
					first.To = want
					var refusal *store_pebble.RebuildUnknownProvenanceError
					require.ErrorAs(t, err, &refusal)
					require.Equal(t, &store_pebble.RebuildUnknownProvenanceError{Count: 1, First: first}, refusal)
					require.Zero(t, moves)
					require.Equal(t, before, rebuildEventSavedRows(t, b))
				})
			}
		})
	}
}

func requireRebuildStoreCandidateMove(t *testing.T, b *store_pebble.BuildStore, before rebuildEventRows,
	call *graph.Edge, target, synth string, confidence float64,
) {
	t.Helper()
	want := before
	want.edges = slices.Clone(before.edges)
	old := graph.EdgeIdentityFor(call)
	i := slices.IndexFunc(want.edges, func(e *graph.Edge) bool { return graph.EdgeIdentityFor(e) == old })
	require.NotEqual(t, -1, i)
	moved := *want.edges[i]
	moved.To, moved.Origin, moved.Confidence = target, graph.OriginASTInferred, confidence
	moved.ConfidenceLabel = graph.ConfidenceLabelFor(graph.EdgeCalls, confidence)
	moved.Meta = maps.Clone(moved.Meta)
	StampSynthesized(&moved, synth)
	want.edges[i] = &moved
	want.sort()
	require.Equal(t, want, rebuildEventSavedRows(t, b))
}

func TestRebuildStoreFactoryAndReduxThunkGetterFileProof(t *testing.T) {
	const callerFile, getterFile = "caller.ts", "getter.ts"
	for _, synth := range []string{SynthStoreFactory, SynthReduxThunk} {
		t.Run(synth, func(t *testing.T) {
			for _, tc := range []struct {
				name                 string
				ambiguous, extraFile bool
				unrelated, refuse    bool
			}{
				{name: "two exact files", ambiguous: true, extraFile: true},
				{name: "third getter moves while two exact files remain", ambiguous: true, extraFile: true},
				{name: "two declarations in one file", ambiguous: true, refuse: true},
				{name: "one exact declaration in the only file"},
				{name: "duplicate moved to a second file", extraFile: true, refuse: true},
				{name: "getter file without candidates needs no receipt", unrelated: true},
				{name: "otherwise irrelevant getter moves into candidate file", unrelated: true, extraFile: true, refuse: true},
				{name: "exact getter preserves fallback despite a moved duplicate", unrelated: true, extraFile: true},
			} {
				t.Run(tc.name, func(t *testing.T) {
					caller := rebuildEventNode(callerFile, "call", "typescript", "")
					candidate := func(file string) *graph.Node {
						if synth == SynthReduxThunk {
							n := rebuildReduxThunkNode(file, "reset")
							n.WorkspaceID = caller.WorkspaceID
							return n
						}
						return rebuildStoreFactoryAction(file, caller.WorkspaceID)
					}
					remoteFile := getterFile
					if tc.unrelated {
						remoteFile = "other.ts"
					}
					local, remote := candidate(callerFile), candidate(remoteFile)
					getter := func(file string) *graph.Node {
						n := rebuildEventNode(file, "useStore", caller.Language, "")
						n.Kind = graph.KindVariable
						return n
					}
					one, duplicate := getter(getterFile), getter(getterFile)
					duplicate.ID += "@2"
					other, third := getter("other.ts"), getter("third.ts")
					call := rebuildStoreFactoryMarker(callerFile, caller.ID, "unresolved::reset", "reset")
					if synth == SynthReduxThunk {
						call = rebuildReduxThunkMarker(caller, "reset", 9)
						call.Meta["store_binding"] = "useStore"
					}
					left := rebuildEventFacts(callerFile, caller.Language, []*graph.Node{caller, local}, call)
					rightNodes := []*graph.Node{one, duplicate}
					if !tc.unrelated {
						rightNodes = append(rightNodes, remote)
					}
					right := rebuildEventFacts(getterFile, caller.Language, rightNodes)
					otherFacts := rebuildEventFacts(other.FilePath, caller.Language, []*graph.Node{other})
					live := []store_pebble.ParseFacts{left, right}
					want := remote.ID
					if tc.unrelated {
						want = local.ID
						live = append(live, rebuildEventFacts(remoteFile, caller.Language, []*graph.Node{remote}))
					}
					if tc.ambiguous {
						want = local.ID
						live = append(live, otherFacts, rebuildEventFacts(third.FilePath, caller.Language, []*graph.Node{third}))
					}
					b := openRebuildGRPCBuild(t, live...)
					before := rebuildEventSavedRows(t, b)
					selection, err := NewFrameworkSynthesizerSelection([]string{synth})
					require.NoError(t, err)
					frozen, err := FreezeRebuildFrameworkSelection(t.Context(), b.RebuildView(), selection)
					require.NoError(t, err)
					if tc.unrelated {
						var nodes []*graph.Node
						if tc.extraFile && !tc.refuse {
							nodes = []*graph.Node{one}
						}
						right = rebuildEventFacts(getterFile, caller.Language, nodes)
					} else if !tc.ambiguous || tc.extraFile {
						right = rebuildEventFacts(getterFile, caller.Language, []*graph.Node{remote, one})
					}
					recorded := []store_pebble.ParseFacts{left, right}
					if tc.unrelated {
						recorded = append(recorded, live[2]) // the remote candidate, not a getter
					}
					if tc.extraFile {
						if !tc.ambiguous {
							moved := *duplicate
							moved.FilePath = other.FilePath
							otherFacts = rebuildEventFacts(moved.FilePath, caller.Language, []*graph.Node{&moved})
						}
						recorded = append(recorded, otherFacts)
					}
					if tc.name == "third getter moves while two exact files remain" {
						moved := *third
						moved.FilePath = "fourth.ts"
						recorded = append(recorded, rebuildEventFacts(moved.FilePath, caller.Language, []*graph.Node{&moved}))
					}
					var moves uint64
					confidence := 0.75
					receipts := rebuildGRPCReceipts(recorded...)
					if synth == SynthStoreFactory {
						stats, proofErr := RunRebuildStoreFactory(t.Context(), b, receipts, frozen)
						moves, err = stats.Moves, proofErr
					} else {
						stats, proofErr := RunRebuildReduxThunk(t.Context(), b, receipts, frozen)
						moves, err = stats.Moves, proofErr
						confidence = ConfidenceHeuristic
					}
					if !tc.refuse {
						require.NoError(t, err)
						require.Equal(t, uint64(1), moves)
						requireRebuildStoreCandidateMove(t, b, before, call, want, synth, confidence)
						return
					}
					var refusal *store_pebble.RebuildUnknownProvenanceError
					require.ErrorAs(t, err, &refusal)
					first := graph.EdgeIdentityFor(call)
					first.To = want
					require.Equal(t, &store_pebble.RebuildUnknownProvenanceError{Count: 1, First: first}, refusal)
					require.Zero(t, moves)
					require.Equal(t, before, rebuildEventSavedRows(t, b))
				})
			}
		})
	}
}

func TestRebuildStoreFactoryAndReduxThunkNodeRoleProof(t *testing.T) {
	const callerFile, storeFile, movedFile = "caller.ts", "store.ts", "moved.ts"
	for _, synth := range []string{SynthStoreFactory, SynthReduxThunk} {
		t.Run(synth, func(t *testing.T) {
			for _, tc := range []struct {
				name, nativeFile string
				proved           bool
			}{
				{"role fields only", storeFile, true},
				{"workspace singleton ignores file", storeFile, true},
				{"global singleton ignores scope", storeFile, true},
				{"caller file decides", storeFile, false},
				{"getter file action needs matching path", callerFile, false},
				{"action loses eligibility", "", false},
				{"moved receipt followed by exact receipt", callerFile, false},
				{"exact receipt followed by moved receipt", callerFile, false},
			} {
				t.Run(tc.name, func(t *testing.T) {
					var workspace string
					if tc.name == "workspace singleton ignores file" {
						workspace = "workspace"
					}
					caller := rebuildEventNode(callerFile, "call", "typescript", workspace)
					local := rebuildStoreFactoryAction(callerFile, caller.WorkspaceID)
					remote := rebuildStoreFactoryAction(storeFile, caller.WorkspaceID)
					getter := rebuildEventNode(storeFile, "useStore", caller.Language, "")
					getter.Kind = graph.KindVariable
					call := rebuildStoreFactoryMarker(callerFile, caller.ID, "unresolved::reset", "reset")
					if synth == SynthReduxThunk {
						call = rebuildReduxThunkMarker(caller, "reset", 9)
						call.Meta["store_binding"] = "useStore"
					}
					leftNodes := []*graph.Node{caller}
					rightNodes := []*graph.Node{remote}
					want := remote.ID
					switch tc.name {
					case "role fields only", "getter file action needs matching path", "moved receipt followed by exact receipt", "exact receipt followed by moved receipt":
						leftNodes = append(leftNodes, local)
						rightNodes = append(rightNodes, getter)
					case "caller file decides":
						leftNodes = append(leftNodes, local)
						want = local.ID
					case "global singleton ignores scope":
						leftNodes = nil
					}
					facts := []store_pebble.ParseFacts{
						rebuildEventFacts(callerFile, caller.Language, leftNodes, call),
						rebuildEventFacts(storeFile, caller.Language, rightNodes),
					}
					if tc.name == "workspace singleton ignores file" {
						foreign := rebuildStoreFactoryAction(callerFile, "foreign")
						facts[0] = rebuildEventFacts(callerFile, caller.Language, append(slices.Clone(leftNodes), foreign), call)
					}
					var tail []store_pebble.ParseFacts
					if synth == SynthReduxThunk {
						anchor := rebuildReduxThunkNode("anchor.ts", "unrelated")
						tail = append(tail, rebuildEventFacts(anchor.FilePath, anchor.Language, []*graph.Node{anchor}))
					}
					live := append(facts, tail...)
					requireNativeNodeRoleTarget(t, synth, want, live...)
					b := openRebuildGRPCBuild(t, live...)
					before := rebuildEventSavedRows(t, b)
					selection, err := NewFrameworkSynthesizerSelection([]string{synth})
					require.NoError(t, err)
					frozen, err := FreezeRebuildFrameworkSelection(t.Context(), b.RebuildView(), selection)
					require.NoError(t, err)
					changed := *remote
					conflictingReceipt := false
					switch tc.name {
					case "role fields only":
						c, g := *caller, *getter
						c.Kind, c.Language = graph.KindMethod, "javascript"
						g.Kind, g.Language = graph.KindConstant, "javascript"
						changed.Language = "javascript"
						if synth == SynthReduxThunk {
							changed.Kind = graph.KindMethod
							changed.Meta = map[string]any{"store_factory": "other"}
						} else {
							changed.Name = "someOtherName"
							changed.Meta = map[string]any{"store_factory": "useStore", "store_member": "reset"}
						}
						leftNodes = []*graph.Node{&c, local}
						rightNodes = []*graph.Node{&changed, &g}
					case "caller file decides":
						c := *caller
						c.FilePath = storeFile
						leftNodes = []*graph.Node{local}
						rightNodes = []*graph.Node{remote, &c}
					case "workspace singleton ignores file", "global singleton ignores scope", "getter file action needs matching path":
						changed.FilePath = movedFile
						if tc.name == "global singleton ignores scope" {
							changed.WorkspaceID = "other"
						}
						rightNodes = rightNodes[1:]
						tail = append(tail, rebuildEventFacts(movedFile, caller.Language, []*graph.Node{&changed}))
					case "moved receipt followed by exact receipt", "exact receipt followed by moved receipt":
						changed.FilePath = movedFile
						conflictingReceipt = true
					case "action loses eligibility":
						changed.Kind = graph.KindType
						rightNodes = []*graph.Node{&changed}
					}
					recorded := []store_pebble.ParseFacts{
						rebuildEventFacts(callerFile, caller.Language, leftNodes, call),
						rebuildEventFacts(storeFile, caller.Language, rightNodes),
					}
					// Duplicate receipts do not define a single native input. Check
					// native on the moved declaration, then prove both receipt orders.
					if conflictingReceipt {
						moved := rebuildEventFacts(movedFile, caller.Language, []*graph.Node{&changed})
						withoutExact := rebuildEventFacts(storeFile, caller.Language, rightNodes[1:])
						requireNativeNodeRoleTarget(t, synth, local.ID, append([]store_pebble.ParseFacts{recorded[0], withoutExact, moved}, tail...)...)
						if tc.name == "moved receipt followed by exact receipt" {
							recorded = slices.Insert(recorded, 1, moved)
						} else {
							recorded = append(recorded, moved)
						}
					} else {
						nativeTarget := call.To
						switch tc.nativeFile {
						case callerFile:
							nativeTarget = local.ID
						case storeFile:
							nativeTarget = remote.ID
						}
						requireNativeNodeRoleTarget(t, synth, nativeTarget, append(slices.Clone(recorded), tail...)...)
					}
					receipts := rebuildGRPCReceipts(append(recorded, tail...)...)
					var moves uint64
					confidence := 0.75
					if synth == SynthStoreFactory {
						stats, proofErr := RunRebuildStoreFactory(t.Context(), b, receipts, frozen)
						moves, err = stats.Moves, proofErr
					} else {
						stats, proofErr := RunRebuildReduxThunk(t.Context(), b, receipts, frozen)
						moves, err = stats.Moves, proofErr
						confidence = ConfidenceHeuristic
					}
					if tc.proved {
						require.NoError(t, err)
						require.Equal(t, uint64(1), moves)
						requireRebuildStoreCandidateMove(t, b, before, call, want, synth, confidence)
						return
					}
					var refusal *store_pebble.RebuildUnknownProvenanceError
					require.ErrorAs(t, err, &refusal)
					first := graph.EdgeIdentityFor(call)
					first.To = want
					require.Equal(t, &store_pebble.RebuildUnknownProvenanceError{Count: 1, First: first}, refusal)
					require.Zero(t, moves)
					require.Equal(t, before, rebuildEventSavedRows(t, b))
				})
			}
		})
	}
}

func requireNativeNodeRoleTarget(t *testing.T, synth, target string, facts ...store_pebble.ParseFacts) {
	t.Helper()
	native := openRebuildTemporalOracle(t, facts...)
	if synth == SynthStoreFactory {
		ResolveStoreFactoryCalls(native)
	} else {
		ResolveReduxThunkCalls(native)
	}
	rows := rebuildEventSQLiteRows(native)
	require.Len(t, rows.edges, 1)
	require.Equal(t, target, rows.edges[0].To)
}
