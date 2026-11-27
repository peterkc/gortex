package resolver

import (
	"slices"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/graph/store_pebble"
)

func rebuildVuexTestNode(file, id, namespace, name, kind string, nodeKind graph.NodeKind) *graph.Node {
	return &graph.Node{ID: file + "::" + id, Kind: nodeKind, Name: id, FilePath: file, Language: "typescript",
		Meta: map[string]any{"vuex_namespace": namespace, "vuex_action": name, "vuex_kind": kind}}
}

func rebuildVuexTestCall(from, to, file, key, kind string, line int) *graph.Edge {
	return &graph.Edge{From: from, To: to, FilePath: file, Kind: graph.EdgeCalls, Line: line,
		Meta: map[string]any{"via": vuexDispatchVia, "vuex_key": key, "vuex_kind": kind}}
}

func TestRebuildVuexDispatchSQLiteOracle(t *testing.T) {
	const file, from = "store.ts", "store.ts::submit"
	caller := &graph.Node{ID: from, Name: "submit", Kind: graph.KindFunction, FilePath: file, Language: "typescript"}
	root := rebuildVuexTestNode(file, "rootLogin", "", "login", "action", graph.KindFunction)
	nested := rebuildVuexTestNode(file, "nestedLogin", "app/user", "login", "action", graph.KindMethod)
	mutation := rebuildVuexTestNode(file, "mutateLogin", "app/user", "login", "mutation", graph.KindFunction)
	nodes := []*graph.Node{caller, root, nested, mutation}
	z := rebuildVuexTestCall(from, "unresolved::z", file, "app/user/login", "action", 10)
	a := rebuildVuexTestCall(from, "unresolved::a", file, "app/user/login", "action", 10)
	z.Meta["witness"], a.Meta["witness"] = "first receipt", "first complete key"
	stationary := rebuildVuexTestCall(from, nested.ID, file, "app/user/login", "action", 11)
	stationary.Meta["witness"] = "stationary stays unstamped"
	rootCall := rebuildVuexTestCall(from, "unresolved::*.login", file, "login", "action", 12)
	rootCall.Meta["registry_claimed"] = true
	commit := rebuildVuexTestCall(from, "unresolved::*.login", file, "app/user/login", "mutation", 13)
	reset := rebuildVuexTestCall(from, root.ID, file, "missing/login", "action", 14)
	StampSynthesized(reset, SynthVuexDispatch)
	reset.Confidence = ConfidenceHeuristic
	incumbent := &graph.Edge{From: from, To: nested.ID, FilePath: file, Kind: graph.EdgeCalls, Line: 15,
		Meta: map[string]any{"witness": "incumbent"}}
	loser := rebuildVuexTestCall(from, "unresolved::loser", file, "app/user/login", "action", 15)
	dynamic := func(line int, marked bool) *graph.Edge {
		e := &graph.Edge{From: from, To: "unresolved::dynamic", FilePath: file, Kind: graph.EdgeCalls, Line: line,
			Meta: map[string]any{"dyn_shape": "computed_member", "dyn_key": "login"}}
		if marked {
			e.Meta["registry_claimed"] = true
		}
		return e
	}
	moveSibling, stationarySibling, markedSibling := dynamic(10, false), dynamic(11, false), dynamic(12, true)
	edges := []*graph.Edge{z, a, stationary, rootCall, commit, reset, incumbent, loser, moveSibling, stationarySibling, markedSibling}
	facts := rebuildEventFacts(file, "typescript", nodes, edges...)
	native := openRebuildTemporalOracle(t, facts)
	b := openRebuildGRPCBuild(t, facts)
	require.Equal(t, rebuildEventSQLiteRows(native), rebuildEventSavedRows(t, b))
	selection, err := NewFrameworkSynthesizerSelection([]string{SynthVuexDispatch})
	require.NoError(t, err)
	frozen, err := FreezeRebuildFrameworkSelection(t.Context(), b.RebuildView(), selection)
	require.NoError(t, err)
	require.True(t, frozen.allows(SynthVuexDispatch))

	report := RunFrameworkSynthesizersWithSelection(native, selection)
	require.NoError(t, report.Err)
	require.Equal(t, 6, report.Total)
	want := rebuildEventSQLiteRows(native)
	for _, keep := range []*graph.Edge{moveSibling, stationarySibling, markedSibling, incumbent, stationary} {
		require.Contains(t, want.edges, keep)
	}
	for line, target := range map[int]string{10: nested.ID, 12: root.ID, 13: mutation.ID, 14: "unresolved::*.login"} {
		i := slices.IndexFunc(want.edges, func(e *graph.Edge) bool { return e.Line == line && e.To == target })
		require.GreaterOrEqual(t, i, 0)
		row := want.edges[i]
		if line == 14 {
			require.Zero(t, row.Confidence)
			require.NotContains(t, row.Meta, MetaSynthesizedBy)
			continue
		}
		require.Equal(t, ConfidenceHeuristic, row.Confidence)
		require.Equal(t, SynthVuexDispatch, row.Meta[MetaSynthesizedBy])
		if line == 10 {
			require.Equal(t, "first receipt", row.Meta["witness"])
		}
		if line == 12 {
			require.Equal(t, true, row.Meta["registry_claimed"])
		}
	}
	for _, moves := range []uint64{6, 0} {
		stats, err := RunRebuildVuexDispatch(t.Context(), b, rebuildGRPCReceipts(facts), frozen)
		require.NoError(t, err)
		require.Equal(t, moves, stats.Moves)
		require.Equal(t, want, rebuildEventSavedRows(t, b))
	}

	// Graph exposes saved pointers to both scans, unlike direct SQLite.
	g := graph.New()
	g.AddBatch(nodes, edges)
	require.Equal(t, 6, ResolveVuexDispatchCalls(g))
	var claimed []int
	for e := range g.EdgesByKind(graph.EdgeCalls) {
		if e.To == "unresolved::dynamic" && e.Meta["registry_claimed"] == true {
			claimed = append(claimed, e.Line)
		}
	}
	require.ElementsMatch(t, []int{10, 11, 12}, claimed)
}

func TestRebuildVuexDispatchGetterAndProof(t *testing.T) {
	const file, other, from = "store.ts", "remote.ts", "store.ts::submit"
	caller := &graph.Node{ID: from, Name: "submit", Kind: graph.KindFunction, FilePath: file, Language: "typescript"}
	local := rebuildVuexTestNode(file, "local", "user", "login", "action", graph.KindFunction)
	remote := rebuildVuexTestNode(other, "remote", "user", "login", "action", graph.KindFunction)
	getter := &graph.Node{ID: other + "::useUserStore", Name: "useUserStore", Kind: graph.KindVariable, FilePath: other, Language: "typescript"}
	call := rebuildVuexTestCall(from, "unresolved::*.login", file, "user/login", "action", 10)
	call.Meta["store_binding"] = getter.Name
	facts := rebuildEventFacts(file, "typescript", []*graph.Node{caller, local}, call)
	definition := rebuildEventFacts(other, "typescript", []*graph.Node{remote, getter})
	selection, err := NewFrameworkSynthesizerSelection([]string{SynthVuexDispatch})
	require.NoError(t, err)
	for _, omit := range []string{"none", "winner", "getter", "Call"} {
		t.Run(omit, func(t *testing.T) {
			b := openRebuildGRPCBuild(t, facts, definition)
			proof, remoteProof := facts, definition
			switch omit {
			case "winner":
				remoteProof.Nodes = []*graph.Node{getter}
			case "getter":
				remoteProof.Nodes = []*graph.Node{remote}
			case "Call":
				proof.Edges = nil
			}
			before := rebuildEventSavedRows(t, b)
			stats, err := RunRebuildVuexDispatch(t.Context(), b, rebuildGRPCReceipts(proof, remoteProof), selection)
			if omit == "none" {
				require.NoError(t, err)
				require.Equal(t, uint64(1), stats.Moves)
				native := openRebuildTemporalOracle(t, facts, definition)
				require.Equal(t, 1, ResolveVuexDispatchCalls(native))
				want := rebuildEventSQLiteRows(native)
				require.Len(t, want.edges, 1)
				require.Equal(t, remote.ID, want.edges[0].To)
				require.Equal(t, want, rebuildEventSavedRows(t, b))
				return
			}
			require.ErrorIs(t, err, store_pebble.ErrRebuildVuexDispatch)
			var refused *store_pebble.RebuildUnknownProvenanceError
			require.ErrorAs(t, err, &refused)
			want := graph.EdgeIdentityFor(call)
			want.To = remote.ID
			require.Equal(t, &store_pebble.RebuildUnknownProvenanceError{Count: 1, First: want}, refused)
			require.Zero(t, stats)
			require.Equal(t, before, rebuildEventSavedRows(t, b))
		})
	}
}

func TestRebuildVuexDispatchFrozenSelection(t *testing.T) {
	const file, from = "store.ts", "store.ts::submit"
	caller := &graph.Node{ID: from, Name: "submit", Kind: graph.KindFunction, FilePath: file, Language: "typescript"}
	unrelated := &graph.Edge{From: from, To: "unresolved::x", Kind: graph.EdgeCalls, FilePath: file, Line: 2,
		Meta: map[string]any{"payload": strings.Repeat("x", 16<<20)}}
	b := openRebuildGRPCBuild(t, rebuildEventFacts(file, "typescript", []*graph.Node{caller}, unrelated))
	selection, err := NewFrameworkSynthesizerSelection([]string{SynthVuexDispatch})
	require.NoError(t, err)
	frozen, err := FreezeRebuildFrameworkSelection(t.Context(), b.RebuildView(), selection)
	require.NoError(t, err)
	require.False(t, frozen.allows(SynthVuexDispatch))
	late := rebuildVuexTestCall(from, "unresolved::*.login", file, "user/login", "action", 3)
	late.Meta["payload"] = unrelated.Meta["payload"]
	b.AddEdge(late)
	stats, err := RunRebuildVuexDispatch(t.Context(), b, nil, frozen)
	require.NoError(t, err)
	require.Zero(t, stats)
	_, err = FreezeRebuildFrameworkSelection(t.Context(), b.RebuildView(), selection)
	require.ErrorIs(t, err, store_pebble.ErrRebuildLinkInputSize)
}
