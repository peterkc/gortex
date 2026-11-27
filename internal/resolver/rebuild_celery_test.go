package resolver

import (
	"fmt"
	"slices"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/graph/store_pebble"
)

func celeryTestNode(file, id, task, registered string) *graph.Node {
	return &graph.Node{ID: file + "::" + id, Name: id, Kind: graph.KindFunction, FilePath: file, Language: "python",
		Meta: map[string]any{"celery_task": task, "celery_registered_name": registered}}
}

func celeryTestCall(from, to, file, task, registered string, line int) *graph.Edge {
	return &graph.Edge{From: from, To: to, FilePath: file, Kind: graph.EdgeCalls, Line: line,
		Meta: map[string]any{"via": celeryVia, "celery_task": task, "celery_registered_name": registered}}
}

func TestRebuildCelerySQLiteOracle(t *testing.T) {
	const file = "tasks.py"
	task := celeryTestNode(file, "mail", "mail", "")
	registered := celeryTestNode(file, "send", "", "email.send")
	registered.Kind = graph.KindMethod
	caller := celeryTestNode(file, "dispatch", "", "")
	z := celeryTestCall(caller.ID, "unresolved::z", file, "mail", "email.send", 10)
	a := celeryTestCall(caller.ID, "unresolved::a", file, "mail", "email.send", 10)
	z.Meta["witness"], a.Meta["witness"] = "first receipt", "first key"
	byName := celeryTestCall(caller.ID, "unresolved::*.mail", file, "mail", "", 11)
	miss := celeryTestCall(caller.ID, task.ID, file, "mail", "email.missing", 12)
	StampSynthesizedTyped(miss, SynthCelery)
	miss.Confidence = ConfidenceTyped
	stationary := celeryTestCall(caller.ID, registered.ID, file, "mail", "email.send", 13)
	facts := rebuildEventFacts(file, "python", []*graph.Node{task, registered, caller}, z, a, byName, miss, stationary)
	native := openRebuildTemporalOracle(t, facts)
	b := openRebuildGRPCBuild(t, facts)
	selection, err := NewFrameworkSynthesizerSelection([]string{SynthCelery})
	require.NoError(t, err)
	frozen, err := FreezeRebuildFrameworkSelection(t.Context(), b.RebuildView(), selection)
	require.NoError(t, err)
	require.True(t, frozen.allows(SynthCelery))
	nativeReport := RunFrameworkSynthesizersWithSelection(native, selection)
	require.NoError(t, nativeReport.Err)
	require.Equal(t, 4, nativeReport.Total)
	want := rebuildEventSQLiteRows(native)
	require.Contains(t, want.edges, stationary)
	for line, target := range map[int]string{10: registered.ID, 11: task.ID, 12: "unresolved::*.mail"} {
		i := slices.IndexFunc(want.edges, func(e *graph.Edge) bool { return e.Line == line && e.To == target })
		require.GreaterOrEqual(t, i, 0)
		row := want.edges[i]
		if line == 12 {
			require.Zero(t, row.Confidence)
			require.NotContains(t, row.Meta, MetaSynthesizedBy)
		} else {
			require.Equal(t, ConfidenceTyped, row.Confidence)
			require.Equal(t, SynthCelery, row.Meta[MetaSynthesizedBy])
		}
		if line == 10 {
			require.Equal(t, "first receipt", row.Meta["witness"])
		}
	}
	for _, moves := range []uint64{4, 0} {
		stats, err := RunRebuildCelery(t.Context(), b, rebuildGRPCReceipts(facts), frozen)
		require.NoError(t, err)
		require.Equal(t, moves, stats.Moves)
		require.Equal(t, want, rebuildEventSavedRows(t, b))
	}
}

func TestRebuildCeleryCapBeforeWorkspace(t *testing.T) {
	const file = "tasks.py"
	caller := celeryTestNode(file, "dispatch", "", "")
	caller.WorkspaceID = "caller"
	anchor := celeryTestNode(file, "anchor", "mail", "")
	winner := celeryTestNode(file, "winner", "", "email.send")
	winner.WorkspaceID = caller.WorkspaceID
	all := []*graph.Node{caller, anchor, winner}
	for i := range celeryFanoutCap {
		n := celeryTestNode(file, fmt.Sprintf("remote%d", i), "", "email.send")
		n.WorkspaceID = "other"
		all = append(all, n)
	}
	call := celeryTestCall(caller.ID, anchor.ID, file, "mail", "email.send", 10)
	selection, err := NewFrameworkSynthesizerSelection([]string{SynthCelery})
	require.NoError(t, err)
	for _, count := range []int{celeryFanoutCap, celeryFanoutCap + 1} {
		t.Run(fmt.Sprint(count), func(t *testing.T) {
			facts := rebuildEventFacts(file, "python", all[:count+2], call)
			native := openRebuildTemporalOracle(t, facts)
			b := openRebuildGRPCBuild(t, facts)
			target, resolved := winner.ID, 1
			proof := facts
			if count > celeryFanoutCap {
				target, resolved = "unresolved::*.mail", 0
				before := rebuildEventSavedRows(t, b)
				missing := facts
				missing.Nodes = missing.Nodes[:len(missing.Nodes)-1]
				stats, err := RunRebuildCelery(t.Context(), b, rebuildGRPCReceipts(missing), selection)
				require.ErrorIs(t, err, store_pebble.ErrRebuildCelery)
				var refused *store_pebble.RebuildUnknownProvenanceError
				require.ErrorAs(t, err, &refused)
				id := graph.EdgeIdentityFor(call)
				id.To = target
				require.Equal(t, &store_pebble.RebuildUnknownProvenanceError{Count: 1, First: id}, refused)
				require.Zero(t, stats)
				require.Equal(t, before, rebuildEventSavedRows(t, b))
				// The native cap branch never needs a caller proof.
				proof.Nodes = proof.Nodes[1:]
			} else {
				// A remote loser does not need a scoped proof below the cap.
				proof.Nodes = proof.Nodes[:3]
			}
			require.Equal(t, resolved, ResolveCeleryCalls(native))
			want := rebuildEventSQLiteRows(native)
			require.Len(t, want.edges, 1)
			require.Equal(t, target, want.edges[0].To)
			stats, err := RunRebuildCelery(t.Context(), b, rebuildGRPCReceipts(proof), selection)
			require.NoError(t, err)
			require.Equal(t, uint64(1), stats.Moves)
			require.Equal(t, want, rebuildEventSavedRows(t, b))
		})
	}
}

func TestRebuildCeleryNeedsTaskNameAnchor(t *testing.T) {
	const file = "tasks.py"
	registered := celeryTestNode(file, "send", "", "email.send")
	call := celeryTestCall(file+"::dispatch", "unresolved::*.mail", file, "mail", "email.send", 10)
	facts := rebuildEventFacts(file, "python", []*graph.Node{registered}, call)
	b := openRebuildGRPCBuild(t, facts)
	native := openRebuildTemporalOracle(t, facts)
	require.Zero(t, ResolveCeleryCalls(native))
	selection, err := NewFrameworkSynthesizerSelection([]string{SynthCelery})
	require.NoError(t, err)
	stats, err := RunRebuildCelery(t.Context(), b, func(func(store_pebble.ParseFacts, error) bool) {
		t.Fatal("replayed without a task-name anchor")
	}, selection)
	require.NoError(t, err)
	require.Zero(t, stats)
	require.Equal(t, rebuildEventSQLiteRows(native), rebuildEventSavedRows(t, b))
}

func TestRebuildCeleryFrozenSelection(t *testing.T) {
	const file = "tasks.py"
	caller := celeryTestNode(file, "dispatch", "", "")
	large := &graph.Edge{From: caller.ID, To: "unresolved::x", FilePath: file, Kind: graph.EdgeCalls, Line: 2,
		Meta: map[string]any{"payload": strings.Repeat("x", 16<<20)}}
	b := openRebuildGRPCBuild(t, rebuildEventFacts(file, "python", []*graph.Node{caller}, large))
	selection, err := NewFrameworkSynthesizerSelection([]string{SynthCelery})
	require.NoError(t, err)
	frozen, err := FreezeRebuildFrameworkSelection(t.Context(), b.RebuildView(), selection)
	require.NoError(t, err)
	require.False(t, frozen.allows(SynthCelery))
	late := celeryTestCall(caller.ID, "unresolved::*.mail", file, "mail", "", 3)
	late.Meta["payload"] = large.Meta["payload"]
	b.AddEdge(late)
	stats, err := RunRebuildCelery(t.Context(), b, nil, frozen)
	require.NoError(t, err)
	require.Zero(t, stats)
	_, err = FreezeRebuildFrameworkSelection(t.Context(), b.RebuildView(), selection)
	require.ErrorIs(t, err, store_pebble.ErrRebuildLinkInputSize)
}
