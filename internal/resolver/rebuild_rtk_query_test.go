package resolver

import (
	"fmt"
	"slices"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/graph/store_pebble"
)

func rtkQueryTestEndpoint(file, suffix, name string) *graph.Node {
	return &graph.Node{ID: file + "::" + suffix, Name: name, FilePath: file, Kind: graph.KindFunction, Language: "typescript",
		Meta: map[string]any{"rtk_endpoint": name}}
}

func rtkQueryTestCall(from, to, file, endpoint string, line int) *graph.Edge {
	return &graph.Edge{From: from, To: to, Kind: graph.EdgeCalls, FilePath: file, Line: line,
		Meta: map[string]any{"via": rtkQueryVia, "rtk_endpoint": endpoint}}
}

func TestRebuildRTKQuerySQLiteOracle(t *testing.T) {
	const file = "api.ts"
	high := rtkQueryTestEndpoint(file, "z.getUser", "getUser")
	low := rtkQueryTestEndpoint(file, "a.getUser", "getUser")
	hook := &graph.Node{ID: file + "::useGetUserQuery", Kind: graph.KindFunction, Name: "useGetUserQuery", FilePath: file, Language: "typescript",
		Meta: map[string]any{"rtk_generated_hook": true}}
	caller := &graph.Node{ID: file + "::render", Kind: graph.KindFunction, Name: "render", FilePath: file, Language: "typescript"}
	z := rtkQueryTestCall(hook.ID, "unresolved::*z", file, "getUser", 10)
	a := rtkQueryTestCall(hook.ID, "unresolved::*a", file, "getUser", 10)
	z.Meta["witness"] = "first Call receipt"
	a.Meta["witness"] = "first Call key"
	ordinary := &graph.Edge{From: caller.ID, To: "unresolved::useGetUserQuery", Kind: graph.EdgeCalls, FilePath: file, Line: 20}
	reset := rtkQueryTestCall(hook.ID, low.ID, file, "gone", 21)
	reset.Confidence = ConfidenceTyped
	StampSynthesizedTyped(reset, SynthRTKQuery)
	stationary := rtkQueryTestCall(hook.ID, high.ID, file, "getUser", 30)
	incumbentLoser := rtkQueryTestCall(hook.ID, "unresolved::loser", file, "getUser", 30)
	nodes := []*graph.Node{high, low, hook, caller} // deliberately not ID order
	facts := rebuildEventFacts(file, "typescript", nodes, z, a, ordinary, reset, stationary, incumbentLoser)
	native := openRebuildTemporalOracle(t, facts)
	private := openRebuildGRPCBuild(t, facts)
	before := rebuildEventSavedRows(t, private)
	require.Equal(t, before, rebuildEventSQLiteRows(native))
	var ordered []string
	for _, n := range native.NodesByKinds([]graph.NodeKind{graph.KindFunction}) {
		if n.ID == high.ID || n.ID == low.ID {
			ordered = append(ordered, n.ID)
		}
	}
	require.Equal(t, []string{low.ID, high.ID}, ordered)

	selection, err := NewFrameworkSynthesizerSelection([]string{SynthRTKQuery})
	require.NoError(t, err)
	frozen, err := FreezeRebuildFrameworkSelection(t.Context(), private.RebuildView(), selection)
	require.NoError(t, err)
	require.True(t, frozen.allows(SynthRTKQuery))
	disabled, err := NewFrameworkSynthesizerSelection(nil)
	require.NoError(t, err)
	stats, err := RunRebuildRTKQuery(t.Context(), private, nil, disabled)
	require.NoError(t, err)
	require.Zero(t, stats)
	require.Equal(t, before, rebuildEventSavedRows(t, private))

	report := RunFrameworkSynthesizersWithSelection(native, selection)
	require.NoError(t, report.Err)
	require.Equal(t, 5, report.Total)
	want := rebuildEventSQLiteRows(native)
	require.Contains(t, want.edges, stationary)
	for line, to := range map[int]string{10: high.ID, 20: hook.ID, 21: "unresolved::*.gone"} {
		i := slices.IndexFunc(want.edges, func(e *graph.Edge) bool { return e.Line == line && e.To == to })
		require.GreaterOrEqual(t, i, 0)
		saved := want.edges[i]
		if line == 21 {
			require.Zero(t, saved.Confidence)
			require.NotContains(t, saved.Meta, MetaSynthesizedBy)
			continue
		}
		require.Equal(t, ConfidenceTyped, saved.Confidence)
		require.Equal(t, SynthRTKQuery, saved.Meta[MetaSynthesizedBy])
		if line == 10 {
			require.Equal(t, "first Call receipt", saved.Meta["witness"])
		}
	}
	for _, wantStats := range []store_pebble.RebuildRTKQueryStats{{Resolved: 5, Moves: 5}, {Resolved: 2}} {
		stats, err := RunRebuildRTKQuery(t.Context(), private, rebuildGRPCReceipts(facts), frozen)
		require.NoError(t, err)
		require.Equal(t, wantStats, stats)
		require.Equal(t, want, rebuildEventSavedRows(t, private))
	}
}

func TestRebuildRTKQueryDecidingProof(t *testing.T) {
	const file = "api.ts"
	high := rtkQueryTestEndpoint(file, "z", "query")
	low := rtkQueryTestEndpoint(file, "a", "query")
	caller := &graph.Node{ID: file + "::hook", Name: "hook", FilePath: file, Kind: graph.KindFunction, Language: "typescript"}
	call := rtkQueryTestCall(caller.ID, "unresolved::*.query", file, "query", 10)
	facts := rebuildEventFacts(file, "typescript", []*graph.Node{high, low, caller}, call)
	selection, err := NewFrameworkSynthesizerSelection([]string{SynthRTKQuery})
	require.NoError(t, err)
	for _, omit := range []string{"winner", "loser", "Call"} {
		t.Run(omit, func(t *testing.T) {
			private := openRebuildGRPCBuild(t, facts)
			before := rebuildEventSavedRows(t, private)
			proof := facts
			switch omit {
			case "winner":
				proof.Nodes = []*graph.Node{caller, low}
			case "loser":
				proof.Nodes = []*graph.Node{caller, high}
			case "Call":
				proof.Edges = nil
			}
			stats, err := RunRebuildRTKQuery(t.Context(), private, rebuildGRPCReceipts(proof), selection)
			if omit == "loser" {
				require.NoError(t, err)
				require.Equal(t, store_pebble.RebuildRTKQueryStats{Resolved: 1, Moves: 1}, stats)
				native := openRebuildTemporalOracle(t, facts)
				require.Equal(t, 1, ResolveRTKQueryCalls(native))
				require.Equal(t, rebuildEventSQLiteRows(native), rebuildEventSavedRows(t, private))
				return
			}
			require.ErrorIs(t, err, store_pebble.ErrRebuildRTKQuery)
			var refused *store_pebble.RebuildUnknownProvenanceError
			require.ErrorAs(t, err, &refused)
			want := graph.EdgeIdentityFor(call)
			want.To = high.ID
			require.Equal(t, &store_pebble.RebuildUnknownProvenanceError{Count: 1, First: want}, refused)
			require.Zero(t, stats)
			require.Equal(t, before, rebuildEventSavedRows(t, private))
		})
	}
}

func TestRebuildRTKQueryFrozenSelection(t *testing.T) {
	const file = "api.ts"
	endpoint := rtkQueryTestEndpoint(file, "x", "query")
	hook := &graph.Node{ID: file + "::useQuery", Kind: graph.KindFunction, Name: "useQuery", FilePath: file, Language: "typescript",
		Meta: map[string]any{"rtk_generated_hook": true}}
	ordinary := &graph.Edge{From: file + "::render", To: "unresolved::useQuery", Kind: graph.EdgeCalls, FilePath: file, Line: 20}
	private := openRebuildGRPCBuild(t, rebuildEventFacts(file, "typescript", []*graph.Node{endpoint, hook}, ordinary))
	selection, err := NewFrameworkSynthesizerSelection([]string{SynthRTKQuery})
	require.NoError(t, err)
	frozen, err := FreezeRebuildFrameworkSelection(t.Context(), private.RebuildView(), selection)
	require.NoError(t, err)
	require.False(t, frozen.allows(SynthRTKQuery))
	before := rebuildEventSavedRows(t, private)
	private.AddEdge(rtkQueryTestCall(hook.ID, "unresolved::*.query", file, "query", 25))
	withLateTag := rebuildEventSavedRows(t, private)
	require.Len(t, withLateTag.edges, len(before.edges)+1)
	stats, err := RunRebuildRTKQuery(t.Context(), private, nil, frozen)
	require.NoError(t, err)
	require.Zero(t, stats)
	require.Equal(t, withLateTag, rebuildEventSavedRows(t, private))
}

func TestRebuildRTKQuerySQLiteWindow(t *testing.T) {
	const file = "api.ts"
	const from = file + "::hook"
	b, c := rtkQueryTestEndpoint(file, "b", "b"), rtkQueryTestEndpoint(file, "c", "c")
	hook := &graph.Node{ID: from, Name: "hook", FilePath: file, Kind: graph.KindFunction, Language: "typescript"}
	first := rtkQueryTestCall(from, "unresolved::a", file, "b", 1)
	last := rtkQueryTestCall(from, b.ID, file, "c", 1)
	input := []*graph.Edge{first}
	for i := range graph.SQLiteReindexChunkSize - 1 {
		input = append(input, rtkQueryTestCall(from, fmt.Sprintf("unresolved::fill%d", i), file, "c", i+2))
	}
	input = append(input, last)
	facts := rebuildEventFacts(file, "typescript", []*graph.Node{hook, b, c}, input...)
	native := openRebuildTemporalOracle(t, facts)
	private := openRebuildGRPCBuild(t, facts)
	require.Equal(t, graph.SQLiteReindexChunkSize+1, ResolveRTKQueryCalls(native))
	want := rebuildEventSQLiteRows(native)
	require.False(t, slices.ContainsFunc(want.edges, func(e *graph.Edge) bool { return e.To == b.ID }))
	selection, err := NewFrameworkSynthesizerSelection([]string{SynthRTKQuery})
	require.NoError(t, err)
	stats, err := RunRebuildRTKQuery(t.Context(), private, rebuildGRPCReceipts(facts), selection)
	require.NoError(t, err)
	require.Equal(t, store_pebble.RebuildRTKQueryStats{Resolved: graph.SQLiteReindexChunkSize + 1, Moves: graph.SQLiteReindexChunkSize + 1}, stats)
	require.Equal(t, want, rebuildEventSavedRows(t, private))
}

func TestRebuildRTKQueryCallerFileProof(t *testing.T) {
	const file, other = "app.ts", "peer.ts"
	caller := &graph.Node{ID: file + "::render", Name: "render", Kind: graph.KindFunction, FilePath: file, Language: "typescript"}
	local := rtkQueryTestHook(file, "local")
	remote := rtkQueryTestHook(other, "remote")
	endpoint := rtkQueryTestEndpoint(file, "query", "query")
	call := &graph.Edge{From: caller.ID, To: "unresolved::useQuery", Kind: graph.EdgeCalls, FilePath: file, Line: 5}
	facts := []store_pebble.ParseFacts{
		rebuildEventFacts(file, "typescript", []*graph.Node{caller, local, endpoint}, call),
		rebuildEventFacts(other, "typescript", []*graph.Node{remote}),
	}
	selection, err := NewFrameworkSynthesizerSelection([]string{SynthRTKQuery})
	require.NoError(t, err)
	for _, mismatch := range []string{"none", "file", "workspace"} {
		t.Run(mismatch, func(t *testing.T) {
			b := openRebuildGRPCBuild(t, facts...)
			before := rebuildEventSavedRows(t, b)
			raw := *caller
			if mismatch == "file" {
				raw.FilePath = other
			}
			if mismatch == "workspace" {
				raw.WorkspaceID = "other"
			}
			proof := []store_pebble.ParseFacts{
				rebuildEventFacts(file, "typescript", []*graph.Node{local, endpoint}, call),
				rebuildEventFacts(other, "typescript", []*graph.Node{remote}),
				rebuildEventFacts(raw.FilePath, "typescript", []*graph.Node{&raw}),
			}
			stats, err := RunRebuildRTKQuery(t.Context(), b, rebuildGRPCReceipts(proof...), selection)
			if mismatch == "none" {
				require.NoError(t, err)
				require.Equal(t, store_pebble.RebuildRTKQueryStats{Resolved: 1, Moves: 1}, stats)
				native := openRebuildTemporalOracle(t, facts...)
				require.Equal(t, 1, ResolveRTKQueryCalls(native))
				require.Equal(t, rebuildEventSQLiteRows(native), rebuildEventSavedRows(t, b))
				return
			}
			requireRTKQueryRefusal(t, err, stats, call, local.ID)
			require.Equal(t, before, rebuildEventSavedRows(t, b))
		})
	}
}

func TestRebuildRTKQueryGetterEntrant(t *testing.T) {
	const file, other, binding = "app.ts", "store.ts", "useStore"
	caller := &graph.Node{ID: file + "::render", Name: "render", Kind: graph.KindFunction, FilePath: file, Language: "typescript"}
	local, remote := rtkQueryTestHook(file, "local"), rtkQueryTestHook(other, "remote")
	endpoint := rtkQueryTestEndpoint(other, "query", "query")
	getter := &graph.Node{ID: other + "::getter", Name: binding, Kind: graph.KindVariable, FilePath: other, Language: "typescript"}
	entrant := &graph.Node{ID: file + "::entrant", Name: binding, Kind: graph.KindVariable, FilePath: file, Language: "typescript",
		Meta: map[string]any{"store_factory": binding}}
	call := &graph.Edge{From: caller.ID, To: "unresolved::useQuery", Kind: graph.EdgeCalls, FilePath: file, Line: 5,
		Meta: map[string]any{"store_binding": binding}}
	left := rebuildEventFacts(file, "typescript", []*graph.Node{caller, local, entrant}, call)
	right := rebuildEventFacts(other, "typescript", []*graph.Node{remote, endpoint, getter})
	b := openRebuildGRPCBuild(t, left, right)
	before := rebuildEventSavedRows(t, b)
	selection, err := NewFrameworkSynthesizerSelection([]string{SynthRTKQuery})
	require.NoError(t, err)
	raw := *entrant
	raw.Meta = nil
	proof := rebuildEventFacts(file, "typescript", []*graph.Node{caller, local, &raw}, call)
	stats, err := RunRebuildRTKQuery(t.Context(), b, rebuildGRPCReceipts(proof, right), selection)
	requireRTKQueryRefusal(t, err, stats, call, remote.ID)
	require.Equal(t, before, rebuildEventSavedRows(t, b))
	native := openRebuildTemporalOracle(t, proof, right)
	require.Equal(t, 1, ResolveRTKQueryCalls(native))
	rows := rebuildEventSQLiteRows(native)
	require.Len(t, rows.edges, 1)
	require.Equal(t, local.ID, rows.edges[0].To)
}

func rtkQueryTestHook(file, suffix string) *graph.Node {
	return &graph.Node{ID: file + "::" + suffix, Name: "useQuery", Kind: graph.KindFunction, FilePath: file, Language: "typescript",
		Meta: map[string]any{"rtk_generated_hook": true}}
}

func requireRTKQueryRefusal(t *testing.T, err error, stats store_pebble.RebuildRTKQueryStats, call *graph.Edge, target string) {
	t.Helper()
	require.ErrorIs(t, err, store_pebble.ErrRebuildRTKQuery)
	var refused *store_pebble.RebuildUnknownProvenanceError
	require.ErrorAs(t, err, &refused)
	want := graph.EdgeIdentityFor(call)
	want.To = target
	require.Equal(t, &store_pebble.RebuildUnknownProvenanceError{Count: 1, First: want}, refused)
	require.Zero(t, stats)
}

func TestRebuildRTKQueryEndpointReceipt(t *testing.T) {
	const file = "api.ts"
	caller := rtkQueryTestHook(file, "hook")
	winner := rtkQueryTestEndpoint(file, "a", "query")
	rival := rtkQueryTestEndpoint(file, "z", "other")
	call := rtkQueryTestCall(caller.ID, "unresolved::*.query", file, "query", 5)
	facts := rebuildEventFacts(file, "typescript", []*graph.Node{caller, winner, rival}, call)
	b := openRebuildGRPCBuild(t, facts)
	before := rebuildEventSavedRows(t, b)
	selection, err := NewFrameworkSynthesizerSelection([]string{SynthRTKQuery})
	require.NoError(t, err)
	raw := *rival
	raw.Meta = map[string]any{"rtk_endpoint": "query"}
	proof := rebuildEventFacts(file, "typescript", []*graph.Node{caller, winner, &raw}, call)
	stats, err := RunRebuildRTKQuery(t.Context(), b, rebuildGRPCReceipts(proof), selection)
	requireRTKQueryRefusal(t, err, stats, call, winner.ID)
	require.Equal(t, before, rebuildEventSavedRows(t, b))
	native := openRebuildTemporalOracle(t, proof)
	require.Equal(t, 1, ResolveRTKQueryCalls(native))
	rows := rebuildEventSQLiteRows(native)
	require.Len(t, rows.edges, 1)
	require.Equal(t, rival.ID, rows.edges[0].To)
}

func TestRebuildRTKQueryHookAndAnchorReceipts(t *testing.T) {
	const file = "app.ts"
	caller := &graph.Node{ID: file + "::render", Name: "render", Kind: graph.KindFunction, FilePath: file, Language: "typescript"}
	hook, rival := rtkQueryTestHook(file, "hook"), rtkQueryTestHook(file, "rival")
	rival.Name = "useOtherQuery"
	entrant := &graph.Node{ID: file + "::entrant", Name: hook.Name, Kind: graph.KindFunction, FilePath: file, Language: "typescript"}
	endpoint := rtkQueryTestEndpoint(file, "endpoint", "query")
	call := &graph.Edge{From: caller.ID, To: "unresolved::useQuery", Kind: graph.EdgeCalls, FilePath: file, Line: 5}
	facts := rebuildEventFacts(file, "typescript", []*graph.Node{caller, hook, rival, entrant, endpoint}, call)
	selection, err := NewFrameworkSynthesizerSelection([]string{SynthRTKQuery})
	require.NoError(t, err)
	for _, mismatch := range []string{"anchor missing", "anchor conflicting", "hook renamed", "rival renamed", "new hook"} {
		t.Run(mismatch, func(t *testing.T) {
			b := openRebuildGRPCBuild(t, facts)
			before := rebuildEventSavedRows(t, b)
			proof := facts
			proof.Nodes = slices.Clone(facts.Nodes)
			switch mismatch {
			case "anchor missing":
				proof.Nodes = proof.Nodes[:4]
			case "anchor conflicting":
				raw := *endpoint
				raw.Meta = nil
				proof.Nodes = append(proof.Nodes, &raw)
			case "hook renamed":
				raw := *hook
				raw.Name = "useDifferentQuery"
				proof.Nodes[1] = &raw
			case "rival renamed":
				raw := *rival
				raw.Name = hook.Name
				proof.Nodes[2] = &raw
			case "new hook":
				raw := *entrant
				raw.Meta = hook.Meta
				proof.Nodes[3] = &raw
			}
			proof.File.NodeCount = len(proof.Nodes)
			stats, err := RunRebuildRTKQuery(t.Context(), b, rebuildGRPCReceipts(proof), selection)
			requireRTKQueryRefusal(t, err, stats, call, hook.ID)
			require.Equal(t, before, rebuildEventSavedRows(t, b))
		})
	}
}

func TestRebuildRTKQueryGetterProof(t *testing.T) {
	const file, other, binding = "app.ts", "store.ts", "useStore"
	selection, err := NewFrameworkSynthesizerSelection([]string{SynthRTKQuery})
	require.NoError(t, err)
	for _, tc := range []struct {
		name, getterFile               string
		singleton, omit, changeBinding bool
		refuse                         bool
	}{
		{name: "getter beats caller file", getterFile: other},
		{name: "deciding getter missing", getterFile: other, omit: true, refuse: true},
		{name: "deciding binding changed", getterFile: other, changeBinding: true, refuse: true},
		{name: "getter in caller file can be omitted", getterFile: file, omit: true},
		{name: "getter in empty candidate file can be omitted", getterFile: "third.ts", omit: true},
		{name: "singleton ignores binding and getter", getterFile: other, singleton: true, omit: true, changeBinding: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			caller := &graph.Node{ID: file + "::render", Name: "render", Kind: graph.KindFunction, FilePath: file, Language: "typescript"}
			local, remote := rtkQueryTestHook(file, "local"), rtkQueryTestHook(other, "remote")
			endpoint := rtkQueryTestEndpoint(other, "query", "query")
			getter := &graph.Node{ID: tc.getterFile + "::getter", Name: binding, Kind: graph.KindVariable, FilePath: tc.getterFile, Language: "typescript"}
			call := &graph.Edge{From: caller.ID, To: "unresolved::useQuery", Kind: graph.EdgeCalls, FilePath: file, Line: 5,
				Meta: map[string]any{"store_binding": binding}}
			left := rebuildEventFacts(file, "typescript", []*graph.Node{caller, local}, call)
			right := rebuildEventFacts(other, "typescript", []*graph.Node{endpoint})
			if !tc.singleton {
				right.Nodes = append(right.Nodes, remote)
				right.File.NodeCount++
			}
			facts := []store_pebble.ParseFacts{left, right}
			switch tc.getterFile {
			case file:
				facts[0].Nodes = append(facts[0].Nodes, getter)
				facts[0].File.NodeCount++
			case other:
				facts[1].Nodes = append(facts[1].Nodes, getter)
				facts[1].File.NodeCount++
			default:
				facts = append(facts, rebuildEventFacts(tc.getterFile, "typescript", []*graph.Node{getter}))
			}
			b := openRebuildGRPCBuild(t, facts...)
			before := rebuildEventSavedRows(t, b)
			proof := slices.Clone(facts)
			if tc.omit {
				for i := range proof {
					proof[i].Nodes = slices.DeleteFunc(slices.Clone(proof[i].Nodes), func(n *graph.Node) bool { return n.ID == getter.ID })
					proof[i].File.NodeCount = len(proof[i].Nodes)
				}
			}
			if tc.changeBinding {
				raw := *call
				raw.Meta = map[string]any{"store_binding": "different"}
				proof[0].Edges = []*graph.Edge{&raw}
			}
			stats, err := RunRebuildRTKQuery(t.Context(), b, rebuildGRPCReceipts(proof...), selection)
			if tc.refuse {
				requireRTKQueryRefusal(t, err, stats, call, remote.ID)
				require.Equal(t, before, rebuildEventSavedRows(t, b))
				return
			}
			require.NoError(t, err)
			require.Equal(t, store_pebble.RebuildRTKQueryStats{Resolved: 1, Moves: 1}, stats)
			native := openRebuildTemporalOracle(t, facts...)
			require.Equal(t, 1, ResolveRTKQueryCalls(native))
			require.Equal(t, rebuildEventSQLiteRows(native), rebuildEventSavedRows(t, b))
		})
	}
}

func TestRebuildRTKQueryEndpointGateAndReset(t *testing.T) {
	const file = "api.ts"
	caller := rtkQueryTestHook(file, "hook")
	old := rtkQueryTestEndpoint(file, "former", "other")
	call := rtkQueryTestCall(caller.ID, old.ID, file, "removed", 5)
	call.Confidence = ConfidenceTyped
	StampSynthesizedTyped(call, SynthRTKQuery)
	selection, err := NewFrameworkSynthesizerSelection([]string{SynthRTKQuery})
	require.NoError(t, err)
	for _, withEndpoint := range []bool{false, true} {
		nodes := []*graph.Node{caller}
		if withEndpoint {
			nodes = append(nodes, old)
		}
		facts := rebuildEventFacts(file, "typescript", nodes, call)
		b := openRebuildGRPCBuild(t, facts)
		before := rebuildEventSavedRows(t, b)
		stats, err := RunRebuildRTKQuery(t.Context(), b, func(yield func(store_pebble.ParseFacts, error) bool) {
			require.True(t, withEndpoint, "no endpoint must skip receipt replay")
			yield(facts, nil)
		}, selection)
		require.NoError(t, err)
		if !withEndpoint {
			require.Zero(t, stats)
			require.Equal(t, before, rebuildEventSavedRows(t, b))
			continue
		}
		require.Equal(t, store_pebble.RebuildRTKQueryStats{Moves: 1}, stats)
		native := openRebuildTemporalOracle(t, facts)
		require.Zero(t, ResolveRTKQueryCalls(native))
		want := rebuildEventSQLiteRows(native)
		require.Len(t, want.edges, 1)
		require.Equal(t, "unresolved::*.removed", want.edges[0].To)
		require.Zero(t, want.edges[0].Confidence)
		require.NotContains(t, want.edges[0].Meta, MetaSynthesizedBy)
		require.Equal(t, want, rebuildEventSavedRows(t, b))
	}
}
