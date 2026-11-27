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

func TestRebuildReduxThunkSQLiteOracle(t *testing.T) {
	// The JS and TS dispatches come from jsts_redux_thunk_test.go. The TS
	// fixture adds a begin thunk so the dispatch has a concrete destination.
	const js = `import { createSlice, createAsyncThunk } from '@reduxjs/toolkit';

const userSlice = createSlice({
  name: 'user',
  reducers: {
    setLoading(state) {},
    set(state, action) {},
  },
});

const fetchUser = createAsyncThunk('user/fetch', async (id, { dispatch }) => {
  dispatch(setLoading());
  dispatch(userSlice.actions.set(id));
});
`
	const ts = `import { createAsyncThunk } from '@reduxjs/toolkit';
const load = createAsyncThunk('load', async (_: number, { dispatch }) => {
  dispatch(begin());
});
const begin = createAsyncThunk('begin', async () => {});
`
	jsExtractor := languages.NewJavaScriptExtractor()
	jsParsed, err := jsExtractor.Extract("store.js", []byte(js))
	require.NoError(t, err)
	t.Cleanup(jsParsed.ReleaseTree)
	tsExtractor := languages.NewTypeScriptExtractor()
	tsParsed, err := tsExtractor.Extract("t.ts", []byte(ts))
	require.NoError(t, err)
	t.Cleanup(tsParsed.ReleaseTree)
	facts := []store_pebble.ParseFacts{
		rebuildEventFacts("store.js", jsExtractor.Language(), jsParsed.Nodes, jsParsed.Edges...),
		rebuildEventFacts("t.ts", tsExtractor.Language(), tsParsed.Nodes, tsParsed.Edges...),
	}
	var markers []*graph.Edge
	for i, name := range []string{"fetchUser", "load"} {
		f := facts[i]
		id := f.File.FilePath + "::" + name
		caller := slices.IndexFunc(f.Nodes, func(n *graph.Node) bool { return n.ID == id })
		require.NotEqual(t, -1, caller, "%s", id)
		require.Equal(t, name, f.Nodes[caller].Meta["redux_thunk"])
		for _, e := range f.Edges {
			if e.Meta["via"] == "redux-thunk" {
				markers = append(markers, e)
			}
		}
	}
	wantMarkers := []*graph.Edge{
		{From: "store.js::fetchUser", To: "unresolved::*.setLoading", Kind: graph.EdgeCalls, FilePath: "store.js", Line: 12,
			Meta: map[string]any{"via": "redux-thunk", "thunk_dispatch": "setLoading"}},
		{From: "store.js::fetchUser", To: "unresolved::*.set", Kind: graph.EdgeCalls, FilePath: "store.js", Line: 13,
			Meta: map[string]any{"via": "redux-thunk", "thunk_dispatch": "set"}},
		{From: "t.ts::load", To: "unresolved::*.begin", Kind: graph.EdgeCalls, FilePath: "t.ts", Line: 3,
			Meta: map[string]any{"via": "redux-thunk", "thunk_dispatch": "begin"}},
	}
	require.ElementsMatch(t, wantMarkers, markers)
	moves := map[graph.EdgeIdentity]string{
		graph.EdgeIdentityFor(wantMarkers[0]): "store.js::userSlice.setLoading@6",
		graph.EdgeIdentityFor(wantMarkers[1]): "store.js::userSlice.set@7",
		graph.EdgeIdentityFor(wantMarkers[2]): "t.ts::begin",
	}
	for i, f := range facts {
		facts[i] = roundTripRebuildRNBridgeFacts(t, f)
	}
	requireRebuildReduxThunkOracle(t, facts, moves, 3, 3)
}

func TestRebuildReduxThunkSQLiteElection(t *testing.T) {
	// Use already qualified facts so the workspace check sees the foreign
	// thunk. Extracted Redux thunk Calls do not carry store_binding.
	const callerFile, getterFile, lang = "caller.ts", "getter.ts", "typescript"
	caller := rebuildReduxThunkNode(callerFile, "outer")
	localThunk := rebuildReduxThunkNode(callerFile, "begin")
	localThunk.Kind = graph.KindMethod
	getterThunk := rebuildReduxThunkNode(getterFile, "begin")
	getterThunk.Kind = graph.KindConstant
	foreign := rebuildReduxThunkNode("foreign.ts", "begin")
	foreign.WorkspaceID = "other"
	firstFallbackThunk := rebuildReduxThunkNode("other.ts", "fallback")
	secondFallbackThunk := rebuildReduxThunkNode("peer.ts", "fallback")
	getter := rebuildEventNode(getterFile, "useStore", lang, "")
	getter.Kind = graph.KindConstant
	beginAction := rebuildEventNode(callerFile, "slice.begin", lang, "")
	beginAction.Meta = map[string]any{"store_factory": "slice", "store_member": "begin"}
	localAction := rebuildEventNode(callerFile, "fallback", lang, "")
	localAction.Meta = map[string]any{"store_factory": "slice"}
	getterAction := rebuildEventNode(getterFile, "fallback", lang, "")
	getterAction.Meta = map[string]any{"store_factory": "useStore"}

	local := rebuildReduxThunkMarker(caller, "begin", 10)
	bound := rebuildReduxThunkMarker(caller, "begin", 11)
	bound.Meta["store_binding"] = "useStore"
	bound.Meta[MetaProvenance] = "fixture-source"
	fallback := rebuildReduxThunkMarker(caller, "fallback", 12)
	boundFallback := rebuildReduxThunkMarker(caller, "fallback", 13)
	boundFallback.Meta["store_binding"] = "useStore"
	stale := rebuildReduxThunkMarker(caller, "removed", 14)
	stale.To = localAction.ID
	stale.Origin = graph.OriginASTInferred
	stale.Confidence = 0.6
	stale.ConfidenceLabel = graph.ConfidenceLabelFor(graph.EdgeCalls, 0.6)
	stale.Meta[MetaSynthesizedBy] = SynthReduxThunk
	stale.Meta[MetaProvenance] = ProvenanceHeuristic
	stale.Meta["fixture"] = "keep on restub"

	facts := []store_pebble.ParseFacts{
		rebuildEventFacts(callerFile, lang, []*graph.Node{caller, localThunk, beginAction, localAction},
			local, bound, fallback, boundFallback, stale),
		rebuildEventFacts(getterFile, lang, []*graph.Node{getterThunk, getter, getterAction}),
		rebuildEventFacts(firstFallbackThunk.FilePath, lang, []*graph.Node{firstFallbackThunk}),
		rebuildEventFacts(secondFallbackThunk.FilePath, lang, []*graph.Node{secondFallbackThunk}),
		rebuildEventFacts(foreign.FilePath, lang, []*graph.Node{foreign}),
	}
	requireRebuildReduxThunkOracle(t, facts, map[graph.EdgeIdentity]string{
		graph.EdgeIdentityFor(local):         localThunk.ID,  // Thunk beats a valid same-name action.
		graph.EdgeIdentityFor(bound):         getterThunk.ID, // Getter beats same-file thunk.
		graph.EdgeIdentityFor(fallback):      localAction.ID, // Neither thunk in the caller's file.
		graph.EdgeIdentityFor(boundFallback): getterAction.ID,
		graph.EdgeIdentityFor(stale):         "unresolved::*.removed",
	}, 5, 4)
}

func TestRebuildReduxThunkSQLiteNoOp(t *testing.T) {
	for _, tc := range []struct {
		name, file, language string
		thunk, via, dispatch bool
		allowed              bool
	}{
		{name: "no thunk skips receipts", file: "call.ts", language: "typescript", via: true, dispatch: true, allowed: true},
		{name: "web without via", file: "call.ts", language: "typescript", thunk: true, dispatch: true},
		{name: "via without web", file: "call.go", language: "go", thunk: true, via: true, dispatch: true},
		{name: "via only opens census but owns no Call", file: "call.ts", language: "typescript", thunk: true, via: true, allowed: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			caller := rebuildEventNode(tc.file, "call", tc.language, "")
			if tc.thunk {
				caller.Meta = map[string]any{"redux_thunk": "call"}
			}
			action := rebuildEventNode(tc.file, "move", tc.language, "")
			action.Meta = map[string]any{"store_factory": "useStore"}
			call := rebuildReduxThunkMarker(caller, "move", 9)
			if !tc.via {
				delete(call.Meta, "via")
			}
			if !tc.dispatch {
				delete(call.Meta, "thunk_dispatch")
			}
			facts := rebuildEventFacts(tc.file, tc.language, []*graph.Node{caller, action}, call)
			b, native := openRebuildEventStores(t, facts)
			before := rebuildEventSQLiteRows(native)
			require.Len(t, before.edges, 1)
			require.Equal(t, before, rebuildEventSavedRows(t, b))
			selection, err := NewFrameworkSynthesizerSelection([]string{SynthReduxThunk})
			require.NoError(t, err)
			frozen, err := FreezeRebuildFrameworkSelection(t.Context(), b.RebuildView(), selection)
			require.NoError(t, err)
			require.Equal(t, tc.allowed, frozen.allows(SynthReduxThunk))

			report := RunFrameworkSynthesizersWithSelection(native, selection)
			require.NoError(t, report.Err)
			require.Zero(t, report.Total)
			require.Equal(t, before, rebuildEventSQLiteRows(native))
			receipts := rebuildGRPCReceipts(facts)
			if !tc.allowed || !tc.thunk {
				receipts = func(_ func(store_pebble.ParseFacts, error) bool) {
					t.Fatal("excluded or thunk-free Redux Thunk stage must not consume receipts")
				}
			}
			stats, err := RunRebuildReduxThunk(t.Context(), b, receipts, frozen)
			require.NoError(t, err)
			require.Zero(t, stats)
			require.Equal(t, before, rebuildEventSavedRows(t, b))
		})
	}
}

func TestRebuildReduxThunkSQLiteCollision(t *testing.T) {
	for _, incumbent := range []bool{false, true} {
		name := "first mover"
		if incumbent {
			name = "stationary incumbent"
		}
		t.Run(name, func(t *testing.T) {
			const file = "collision.ts"
			caller := rebuildReduxThunkNode(file, "outer")
			target := rebuildReduxThunkNode(file, "inner")
			first := rebuildReduxThunkMarker(caller, "inner", 8)
			first.To = "unresolved::zzz.inner"
			first.Meta["payload"] = "first"
			second := rebuildReduxThunkMarker(caller, "inner", 8)
			second.Meta["payload"] = "second"
			calls := []*graph.Edge{first, second}
			wantPayload := "first"
			if incumbent {
				fixed := rebuildReduxThunkMarker(caller, "inner", 8)
				fixed.To = target.ID
				fixed.Meta["payload"] = "stationary"
				calls = append(calls, fixed)
				wantPayload = "stationary"
			}
			facts := rebuildEventFacts(file, caller.Language, []*graph.Node{caller, target}, calls...)
			b, native := openRebuildEventStores(t, facts)
			before := rebuildEventSQLiteRows(native)
			require.Len(t, before.edges, len(calls))
			require.Equal(t, before, rebuildEventSavedRows(t, b))
			selection, err := NewFrameworkSynthesizerSelection([]string{SynthReduxThunk})
			require.NoError(t, err)
			frozen, err := FreezeRebuildFrameworkSelection(t.Context(), b.RebuildView(), selection)
			require.NoError(t, err)
			require.True(t, frozen.allows(SynthReduxThunk))

			report := RunFrameworkSynthesizersWithSelection(native, selection)
			require.NoError(t, report.Err)
			require.Equal(t, len(calls), report.Total)
			want := rebuildEventSQLiteRows(native)
			require.Len(t, want.edges, 1)
			require.Equal(t, target.ID, want.edges[0].To)
			require.Equal(t, wantPayload, want.edges[0].Meta["payload"])
			if incumbent {
				require.Nil(t, want.edges[0].Meta[MetaSynthesizedBy])
			}
			stats, err := RunRebuildReduxThunk(t.Context(), b, rebuildGRPCReceipts(facts), frozen)
			require.NoError(t, err)
			require.Equal(t, store_pebble.RebuildReduxThunkStats{
				Stubs: uint64(len(calls)), Resolved: uint64(len(calls)), Moves: 2,
			}, stats)
			require.Equal(t, want, rebuildEventSavedRows(t, b))
		})
	}
}

func TestRebuildReduxThunkRefusesUnprovedMove(t *testing.T) {
	const callerFile, getterFile = "caller.ts", "getter.ts"
	caller := rebuildEventNode(callerFile, "outer", "typescript", "workspace")
	local := rebuildReduxThunkNode(callerFile, "inner")
	remote := rebuildReduxThunkNode(getterFile, "inner")
	local.WorkspaceID, remote.WorkspaceID = caller.WorkspaceID, caller.WorkspaceID
	getter := rebuildEventNode(getterFile, "useStore", caller.Language, "")
	getter.Kind = graph.KindVariable
	call := rebuildReduxThunkMarker(caller, "inner", 8)
	call.Meta["store_binding"] = "useStore"
	left := rebuildEventFacts(callerFile, caller.Language, []*graph.Node{caller, local}, call)
	right := rebuildEventFacts(getterFile, caller.Language, []*graph.Node{remote, getter})
	for _, mismatch := range []string{"raw binding", "losing thunk scope", "getter eligibility"} {
		t.Run(mismatch, func(t *testing.T) {
			b := openRebuildGRPCBuild(t, left, right)
			before := rebuildEventSavedRows(t, b)
			selection, err := NewFrameworkSynthesizerSelection([]string{SynthReduxThunk})
			require.NoError(t, err)
			frozen, err := FreezeRebuildFrameworkSelection(t.Context(), b.RebuildView(), selection)
			require.NoError(t, err)
			l, r := left, right
			switch mismatch {
			case "raw binding":
				e := *call
				e.Meta = maps.Clone(e.Meta)
				delete(e.Meta, "store_binding")
				l.Edges = []*graph.Edge{&e}
			case "losing thunk scope":
				n := *local
				n.WorkspaceID = "other"
				l.Nodes = []*graph.Node{caller, &n}
			case "getter eligibility":
				n := *getter
				n.Meta = map[string]any{"store_factory": false}
				r.Nodes = []*graph.Node{remote, &n}
			}
			stats, err := RunRebuildReduxThunk(t.Context(), b, rebuildGRPCReceipts(l, r), frozen)
			if mismatch == "losing thunk scope" {
				require.NoError(t, err)
				require.Equal(t, store_pebble.RebuildReduxThunkStats{Stubs: 1, Resolved: 1, Moves: 1}, stats)
				requireRebuildStoreCandidateMove(t, b, before, call, remote.ID, SynthReduxThunk, ConfidenceHeuristic)
				return
			}
			first := graph.EdgeIdentityFor(call)
			first.To = remote.ID
			var refusal *store_pebble.RebuildUnknownProvenanceError
			require.ErrorAs(t, err, &refusal)
			require.Equal(t, &store_pebble.RebuildUnknownProvenanceError{Count: 1, First: first}, refusal)
			require.Zero(t, stats)
			require.Equal(t, before, rebuildEventSavedRows(t, b))
		})
	}
}

func requireRebuildReduxThunkOracle(t *testing.T, facts []store_pebble.ParseFacts,
	moves map[graph.EdgeIdentity]string, stubs, resolved uint64,
) {
	t.Helper()
	b, native := openRebuildEventStores(t, facts...)
	before := rebuildEventSQLiteRows(native)
	require.Equal(t, before, rebuildEventSavedRows(t, b))
	selection, err := NewFrameworkSynthesizerSelection([]string{SynthReduxThunk})
	require.NoError(t, err)
	frozen, err := FreezeRebuildFrameworkSelection(t.Context(), b.RebuildView(), selection)
	require.NoError(t, err)
	require.True(t, frozen.allows(SynthReduxThunk))

	want := before
	want.edges = slices.Clone(before.edges)
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
		if strings.HasPrefix(to, "unresolved::") {
			moved.Confidence, moved.ConfidenceLabel = 0, ""
			delete(moved.Meta, MetaSynthesizedBy)
			delete(moved.Meta, MetaProvenance)
		} else {
			moved.Confidence = 0.6
			moved.ConfidenceLabel = graph.ConfidenceLabelFor(graph.EdgeCalls, 0.6)
			moved.Meta[MetaSynthesizedBy] = SynthReduxThunk
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
		stats, err := RunRebuildReduxThunk(t.Context(), b, receipts, frozen)
		require.NoError(t, err)
		require.Equal(t, store_pebble.RebuildReduxThunkStats{
			Stubs: stubs, Resolved: resolved, Moves: wantMoves,
		}, stats, "private pass %d", pass+1)
		require.Equal(t, want, rebuildEventSavedRows(t, b), "private pass %d", pass+1)
	}
}

func rebuildReduxThunkNode(file, name string) *graph.Node {
	n := rebuildEventNode(file, name, "typescript", "")
	n.Kind = graph.KindVariable
	n.Meta = map[string]any{"redux_thunk": name}
	return n
}

func rebuildReduxThunkMarker(caller *graph.Node, callee string, line int) *graph.Edge {
	return &graph.Edge{
		From: caller.ID, To: "unresolved::*." + callee, Kind: graph.EdgeCalls, FilePath: caller.FilePath, Line: line,
		Meta: map[string]any{"via": "redux-thunk", "thunk_dispatch": callee},
	}
}
