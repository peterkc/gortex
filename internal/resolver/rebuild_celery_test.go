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

func TestRebuildCeleryProofGuards(t *testing.T) {
	const file, registered = "tasks.py", "email.send"
	caller := celeryTestNode(file, "dispatch", "", "")
	caller.WorkspaceID = "local"
	anchor := celeryTestNode(file, "anchor", "mail", "")
	winner := celeryTestNode(file, "winner", "", registered)
	winner.WorkspaceID = caller.WorkspaceID
	entrant := celeryTestNode(file, "entrant", "", "")
	entrant.WorkspaceID = "foreign"
	nodes := []*graph.Node{caller, anchor, winner, entrant}
	for i := range celeryFanoutCap - 1 {
		n := celeryTestNode(file, fmt.Sprintf("peer%d", i), "", registered)
		n.WorkspaceID = entrant.WorkspaceID
		nodes = append(nodes, n)
	}
	call := celeryTestCall(caller.ID, "unresolved::*.mail", file, "mail", registered, 4)
	facts := rebuildEventFacts(file, "python", nodes, call)
	selection, err := NewFrameworkSynthesizerSelection([]string{SynthCelery})
	require.NoError(t, err)
	for _, change := range []string{"task-name anchor", "extra registration", "Call registration", "Call owner", "Call language"} {
		t.Run(change, func(t *testing.T) {
			b := openRebuildGRPCBuild(t, facts)
			before := rebuildEventSavedRows(t, b)
			proof := rebuildEventFacts(file, "python", slices.Clone(nodes))
			callProof := rebuildEventFacts(file, "python", nil, call)
			switch change {
			case "task-name anchor":
				proof.Nodes[1] = celeryTestNode(file, "anchor", "", "different.registration")
			case "extra registration":
				raw := celeryTestNode(file, "entrant", "", registered)
				raw.WorkspaceID = entrant.WorkspaceID
				proof.Nodes[3] = raw
			case "Call registration":
				callProof.Edges[0] = celeryTestCall(caller.ID, call.To, file, "mail", "different.registration", call.Line)
			case "Call owner":
				callProof.File.FilePath = "different.py"
			case "Call language":
				callProof.EffectiveLanguage = ""
			}
			stats, err := RunRebuildCelery(t.Context(), b, rebuildGRPCReceipts(proof, callProof), selection)
			require.ErrorIs(t, err, store_pebble.ErrRebuildCelery)
			require.ErrorIs(t, err, store_pebble.ErrRebuildUnknownProvenance)
			require.Zero(t, stats)
			require.Equal(t, before, rebuildEventSavedRows(t, b))
		})
	}
}

func TestRebuildCeleryCandidateNodeCap(t *testing.T) {
	const file, registration = "tasks.py", "email.send"
	selection, err := NewFrameworkSynthesizerSelection([]string{SynthCelery})
	require.NoError(t, err)
	for _, tc := range []struct {
		name, task, registered, callRegistration string
		calls, refuse                            bool
	}{
		{name: "not a candidate", calls: true},
		{name: "no Call", task: "mail", registered: registration},
		{name: "unrelated name and registration", task: "other", registered: registration, calls: true},
		{name: "wanted task", task: "mail", calls: true, refuse: true},
		{name: "wanted registration", registered: registration, callRegistration: registration, calls: true, refuse: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			caller := celeryTestNode(file, "dispatch", "", "")
			anchor := celeryTestNode(file, "anchor", "mail", "")
			large := celeryTestNode(file, "large", tc.task, tc.registered)
			large.Meta["payload"] = strings.Repeat("x", 16<<20)
			facts := rebuildEventFacts(file, "python", []*graph.Node{caller, anchor, large})
			if tc.calls {
				facts.Edges = []*graph.Edge{celeryTestCall(caller.ID, "unresolved::*.mail", file, "mail", tc.callRegistration, 4)}
			}
			b := openRebuildGRPCBuild(t, facts)
			before := rebuildEventSavedRows(t, b)
			stats, err := RunRebuildCelery(t.Context(), b, func(yield func(store_pebble.ParseFacts, error) bool) {
				require.True(t, tc.calls && !tc.refuse, "only a moving, in-budget Call needs receipts")
				yield(facts, nil)
			}, selection)
			if tc.refuse {
				require.ErrorIs(t, err, store_pebble.ErrRebuildCelery)
				require.ErrorIs(t, err, store_pebble.ErrRebuildLinkInputSize)
				require.ErrorContains(t, err, "Celery node row")
				require.Zero(t, stats)
				require.Equal(t, before, rebuildEventSavedRows(t, b))
				return
			}
			require.NoError(t, err)
			if !tc.calls {
				require.Zero(t, stats)
				require.Equal(t, before, rebuildEventSavedRows(t, b))
				return
			}
			native := openRebuildTemporalOracle(t, facts)
			require.Equal(t, 1, ResolveCeleryCalls(native))
			require.Equal(t, rebuildEventSQLiteRows(native), rebuildEventSavedRows(t, b))
		})
	}
}

func TestRebuildCeleryFrozenSelection(t *testing.T) {
	const file = "tasks.py"
	caller := celeryTestNode(file, "dispatch", "", "")
	large := &graph.Edge{From: caller.ID, To: "unresolved::x", FilePath: file, Kind: graph.EdgeCalls, Line: 2,
		Meta: map[string]any{"payload": strings.Repeat("x", 16<<20)}}
	task := celeryTestNode(file, "mail", "mail", "")
	b := openRebuildGRPCBuild(t, rebuildEventFacts(file, "python", []*graph.Node{caller, task}, large))
	selection, err := NewFrameworkSynthesizerSelection([]string{SynthCelery})
	require.NoError(t, err)
	frozen, err := FreezeRebuildFrameworkSelection(t.Context(), b.RebuildView(), selection)
	require.NoError(t, err)
	require.False(t, frozen.allows(SynthCelery))
	before := rebuildEventSavedRows(t, b)
	stats, err := RunRebuildCelery(t.Context(), b, func(func(store_pebble.ParseFacts, error) bool) {
		t.Fatal("an unrelated Call must not require receipts")
	}, selection)
	require.NoError(t, err)
	require.Zero(t, stats)
	require.Equal(t, before, rebuildEventSavedRows(t, b))
	late := celeryTestCall(caller.ID, "unresolved::*.mail", file, "mail", "", 3)
	b.AddEdge(late)
	bounded, err := FreezeRebuildFrameworkSelection(t.Context(), b.RebuildView(), selection)
	require.NoError(t, err)
	require.True(t, bounded.allows(SynthCelery))

	late.Meta["payload"] = large.Meta["payload"]
	b.AddEdge(late)
	stats, err = RunRebuildCelery(t.Context(), b, nil, frozen)
	require.NoError(t, err)
	require.Zero(t, stats)
	refrozen, err := FreezeRebuildFrameworkSelection(t.Context(), b.RebuildView(), selection)
	require.NoError(t, err)
	require.True(t, refrozen.allows(SynthCelery), "oversized row must not change admission")
	stats, err = RunRebuildCelery(t.Context(), b, func(func(store_pebble.ParseFacts, error) bool) {
		t.Fatal("oversized Call must be refused before receipt replay")
	}, refrozen)
	require.ErrorIs(t, err, store_pebble.ErrRebuildCelery)
	require.ErrorIs(t, err, store_pebble.ErrRebuildLinkInputSize)
	require.ErrorContains(t, err, "Celery edge row")
	require.Zero(t, stats)
}
