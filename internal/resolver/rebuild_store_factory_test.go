package resolver

import (
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
	// Surgical facts: two distinct old keys for one caller and source site
	// converge on the same action. The first receipt supplies the winner.
	const file = "order.ts"
	caller := rebuildEventNode(file, "call", "typescript", "")
	action := rebuildStoreFactoryAction(file, "")
	bare := rebuildStoreFactoryMarker(file, caller.ID, "unresolved::reset", "reset")
	wildcard := rebuildStoreFactoryMarker(file, caller.ID, "unresolved::*.reset", "reset")
	bare.Meta["fixture"] = "first payload"
	wildcard.Meta["fixture"] = "second payload"
	facts := roundTripRebuildRNBridgeFacts(t, rebuildEventFacts(file, "typescript", []*graph.Node{caller, action}, bare, wildcard))
	b, native := openRebuildEventStores(t, facts)
	require.Equal(t, rebuildEventSQLiteRows(native), rebuildEventSavedRows(t, b))
	selection, err := NewFrameworkSynthesizerSelection([]string{SynthStoreFactory})
	require.NoError(t, err)
	frozen, err := FreezeRebuildFrameworkSelection(t.Context(), b.RebuildView(), selection)
	require.NoError(t, err)
	require.True(t, frozen.allows(SynthStoreFactory))

	stats, err := RunRebuildStoreFactory(t.Context(), b, rebuildGRPCReceipts(facts), frozen)
	require.NoError(t, err)
	require.EqualValues(t, 2, stats.Moves)
	require.Equal(t, 2, ResolveStoreFactoryCalls(native))
	want := rebuildEventSQLiteRows(native)
	require.Len(t, want.edges, 1)
	require.Equal(t, action.ID, want.edges[0].To)
	require.Equal(t, "first payload", want.edges[0].Meta["fixture"])
	require.Equal(t, want, rebuildEventSavedRows(t, b))
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
	for _, input := range []string{"caller workspace", "loser workspace", "getter eligibility", "raw marker"} {
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
			case "raw marker":
				e := *call
				e.Meta = maps.Clone(e.Meta)
				e.Meta["store_action"] = "other"
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
	const callerFile, getterFile = "caller.ts", "getter.ts"
	for _, synth := range []string{SynthStoreFactory, SynthReduxThunk} {
		t.Run(synth, func(t *testing.T) {
			for _, mode := range []string{"two exact peers", "duplicate receipt", "scope enters winning file"} {
				t.Run(mode, func(t *testing.T) {
					caller := rebuildEventNode(callerFile, "call", "typescript", "local")
					winner := rebuildStoreFactoryAction(callerFile, caller.WorkspaceID)
					getter := rebuildEventNode(getterFile, "useStore", caller.Language, "")
					getter.Kind = graph.KindVariable
					call := rebuildStoreFactoryMarker(callerFile, caller.ID, "unresolved::reset", "reset")
					if synth == SynthReduxThunk {
						call = rebuildReduxThunkMarker(caller, "reset", 9)
						call.Meta["store_binding"] = "useStore"
					}
					var peers []*graph.Node
					for _, suffix := range []string{"@1", "@2", "@3"} {
						n := rebuildStoreFactoryAction(getterFile, caller.WorkspaceID)
						if synth == SynthReduxThunk {
							n = rebuildReduxThunkNode(getterFile, "reset")
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
					left := rebuildEventFacts(callerFile, caller.Language, []*graph.Node{caller, winner}, call)
					right := rebuildEventFacts(getterFile, caller.Language, append([]*graph.Node{getter}, peers...))
					b := openRebuildGRPCBuild(t, left, right)
					before := rebuildEventSavedRows(t, b)
					selection, err := NewFrameworkSynthesizerSelection([]string{synth})
					require.NoError(t, err)
					frozen, err := FreezeRebuildFrameworkSelection(t.Context(), b.RebuildView(), selection)
					require.NoError(t, err)
					// Native falls through two getter-file peers. A duplicate
					// receipt cannot prove the second peer. A foreign peer
					// moved into the winning scope instead breaks a singleton.
					peer := peers[1]
					switch mode {
					case "duplicate receipt":
						peer = peers[0]
					case "scope enters winning file":
						n := *peer
						n.WorkspaceID = caller.WorkspaceID
						peer = &n
					}
					right.Nodes = []*graph.Node{getter, peers[0], peer}
					receipts := rebuildGRPCReceipts(left, right)
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
					if mode == "two exact peers" {
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
				refuse               bool
			}{
				{name: "two exact files", ambiguous: true, extraFile: true},
				{name: "two declarations in one file", ambiguous: true, refuse: true},
				{name: "one exact declaration in the only file"},
				{name: "duplicate moved to a second file", extraFile: true, refuse: true},
			} {
				t.Run(tc.name, func(t *testing.T) {
					caller := rebuildEventNode(callerFile, "call", "typescript", "local")
					candidate := func(file string) *graph.Node {
						if synth == SynthReduxThunk {
							n := rebuildReduxThunkNode(file, "reset")
							n.WorkspaceID = caller.WorkspaceID
							return n
						}
						return rebuildStoreFactoryAction(file, caller.WorkspaceID)
					}
					local, remote := candidate(callerFile), candidate(getterFile)
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
					right := rebuildEventFacts(getterFile, caller.Language, []*graph.Node{remote, one, duplicate})
					otherFacts := rebuildEventFacts(other.FilePath, caller.Language, []*graph.Node{other})
					live := []store_pebble.ParseFacts{left, right}
					want := remote.ID
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
					if !tc.ambiguous || tc.extraFile {
						right.Nodes = []*graph.Node{remote, one}
					}
					recorded := []store_pebble.ParseFacts{left, right}
					if tc.extraFile {
						if !tc.ambiguous {
							moved := *duplicate
							moved.FilePath = other.FilePath
							otherFacts.Nodes = []*graph.Node{&moved}
						}
						recorded = append(recorded, otherFacts)
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
