package resolver

import (
	"fmt"
	"slices"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/graph/store_pebble"
)

func objectRegistryTestCall(from, to, file, class, method string, line int) *graph.Edge {
	return &graph.Edge{From: from, To: to, Kind: graph.EdgeCalls, FilePath: file, Line: line,
		Meta: map[string]any{"via": objectRegistryVia, "registry_value": class, "registry_method": method}}
}

func objectRegistryTestMethod(file, id, name, class string) (*graph.Node, *graph.Edge) {
	n := &graph.Node{ID: id, Name: name, Kind: graph.KindMethod, FilePath: file, Language: "javascript"}
	member := &graph.Edge{From: id, To: file + "::" + class, Kind: graph.EdgeMemberOf, FilePath: file, Line: 2}
	return n, member
}

func TestRebuildObjectRegistrySQLiteOracle(t *testing.T) {
	const file, dispatch = "registry.js", "registry.js::dispatch"
	target, member := objectRegistryTestMethod(file, file+"::Cmd.run", "run", "Cmd")
	other := *member
	other.To = file + "::ZOther"
	caller := &graph.Node{ID: dispatch, Name: "dispatch", Kind: graph.KindFunction, FilePath: file, Language: "javascript"}
	z := objectRegistryTestCall(dispatch, "unresolved::z", file, "Cmd", "missing", 10)
	a := objectRegistryTestCall(dispatch, "unresolved::a", file, "Cmd", "missing", 10)
	z.Meta["witness"] = "first receipt"
	a.Meta["witness"] = "first complete key"
	stationary := objectRegistryTestCall(dispatch, target.ID, file, "Cmd", "run", 11)
	stationary.Meta["witness"] = "keep stationary payload"
	incumbent := &graph.Edge{From: dispatch, To: target.ID, Kind: graph.EdgeCalls, FilePath: file, Line: 14,
		Meta: map[string]any{"witness": "non-registry incumbent"}}
	incumbentLoser := objectRegistryTestCall(dispatch, "unresolved::incumbent", file, "Cmd", "run", 14)
	reset := objectRegistryTestCall(dispatch, "stale::Gone", file, "Gone", "run", 12)
	reset.Confidence = 0.9
	reset.ConfidenceLabel = graph.ConfidenceLabelFor(graph.EdgeCalls, reset.Confidence)
	StampSynthesized(reset, SynthObjectRegistry)
	dynamic := func(line int, preclaimed bool) *graph.Edge {
		e := &graph.Edge{From: dispatch, To: "unresolved::dynamic", Kind: graph.EdgeCalls, FilePath: file, Line: line,
			Meta: map[string]any{"dyn_shape": "computed_member", "dyn_key": "run"}}
		if preclaimed {
			e.Meta["registry_claimed"] = true
		}
		return e
	}
	moveSibling, settledSibling, preclaimed := dynamic(10, false), dynamic(11, false), dynamic(13, true)
	markedMover := objectRegistryTestCall(dispatch, "unresolved::*.Cmd", file, "Cmd", "run", 13)
	markedMover.Meta["registry_claimed"] = true
	nodes := []*graph.Node{caller, target}
	edges := []*graph.Edge{&other, member, z, a, stationary, incumbent, incumbentLoser, reset, moveSibling, settledSibling, preclaimed, markedMover}
	facts := rebuildEventFacts(file, "javascript", nodes, edges...)
	native := openRebuildTemporalOracle(t, facts)
	private := openRebuildGRPCBuild(t, facts)
	before := rebuildEventSavedRows(t, private)
	require.Equal(t, before, rebuildEventSQLiteRows(native))

	selection, err := NewFrameworkSynthesizerSelection([]string{SynthObjectRegistry})
	require.NoError(t, err)
	disabled, err := NewFrameworkSynthesizerSelection(nil)
	require.NoError(t, err)
	stats, err := RunRebuildObjectRegistry(t.Context(), private, nil, disabled)
	require.NoError(t, err)
	require.Zero(t, stats)
	require.Equal(t, before, rebuildEventSavedRows(t, private))

	report := RunFrameworkSynthesizersWithSelection(native, selection)
	require.NoError(t, report.Err)
	require.Equal(t, 5, report.Total)
	want := rebuildEventSQLiteRows(native)
	require.Contains(t, want.edges, moveSibling)
	require.Contains(t, want.edges, settledSibling)
	require.Contains(t, want.edges, preclaimed)
	require.Contains(t, want.edges, stationary)
	require.Contains(t, want.edges, incumbent)
	require.NotContains(t, want.edges, incumbentLoser)
	winner := slices.IndexFunc(want.edges, func(e *graph.Edge) bool { return e.Line == 10 && e.To == target.ID })
	require.GreaterOrEqual(t, winner, 0)
	require.Equal(t, "first receipt", want.edges[winner].Meta["witness"])
	require.Equal(t, SynthObjectRegistry, want.edges[winner].Meta[MetaSynthesizedBy])
	for _, e := range want.edges {
		switch e.Line {
		case 12:
			require.Equal(t, "unresolved::*.Gone", e.To)
			require.Zero(t, e.Confidence)
			require.NotContains(t, e.Meta, MetaSynthesizedBy)
		case 13:
			require.Equal(t, true, e.Meta["registry_claimed"])
		}
	}
	for _, moves := range []uint64{5, 0} {
		stats, err := RunRebuildObjectRegistry(t.Context(), private, rebuildGRPCReceipts(facts), selection)
		require.NoError(t, err)
		require.Equal(t, moves, stats.Moves)
		require.Equal(t, want, rebuildEventSavedRows(t, private))
	}

	// The same-kind scan on Graph exposes its saved pointers. The claim
	// on a settled caller and the claim on a mover both survive there.
	g := graph.New()
	g.AddBatch(nodes, edges)
	require.Equal(t, 5, ResolveObjectRegistryCalls(g))
	var claimed []int
	for e := range g.EdgesByKind(graph.EdgeCalls) {
		if e.To == "unresolved::dynamic" && e.Meta["registry_claimed"] == true {
			claimed = append(claimed, e.Line)
		}
	}
	require.ElementsMatch(t, []int{10, 11, 13}, claimed)
}

func TestRebuildObjectRegistryNoTaggedCalls(t *testing.T) {
	const file = "registry.js"
	node, member := objectRegistryTestMethod(file, file+"::Cmd.run", "run", "Cmd")
	private := openRebuildGRPCBuild(t, rebuildEventFacts(file, "javascript", []*graph.Node{node}, member))
	before := rebuildEventSavedRows(t, private)
	selection, err := NewFrameworkSynthesizerSelection([]string{SynthObjectRegistry})
	require.NoError(t, err)
	stats, err := RunRebuildObjectRegistry(t.Context(), private, func(func(store_pebble.ParseFacts, error) bool) {
		t.Fatal("replayed receipts without a tagged Call")
	}, selection)
	require.NoError(t, err)
	require.Zero(t, stats)
	require.Equal(t, before, rebuildEventSavedRows(t, private))
}

func TestRebuildObjectRegistryCandidateProof(t *testing.T) {
	const file, outside = "registry.js", "remote.js"
	const dispatch = file + "::dispatch"
	caller := &graph.Node{ID: dispatch, Name: "dispatch", Kind: graph.KindFunction, FilePath: file, Language: "javascript"}
	local, member := objectRegistryTestMethod(file, file+"::Cmd.run", "run", "Cmd")
	rival, other := objectRegistryTestMethod(outside, outside+"::Cmd.run", "run", "Cmd")
	call := objectRegistryTestCall(dispatch, "unresolved::*.Cmd", file, "Cmd", "run", 10)
	facts := rebuildEventFacts(file, "javascript", []*graph.Node{caller, local}, member, call)
	remote := rebuildEventFacts(outside, "javascript", []*graph.Node{rival}, other)
	selection, err := NewFrameworkSynthesizerSelection([]string{SynthObjectRegistry})
	require.NoError(t, err)
	for _, bad := range []string{"none", "winner membership", "call"} {
		t.Run(bad, func(t *testing.T) {
			private := openRebuildGRPCBuild(t, facts, remote)
			before := rebuildEventSavedRows(t, private)
			proof := facts
			// The remote method contributes live ambiguity but cannot change
			// the unique caller-file winner. Its receipt is not required.
			switch bad {
			case "winner membership":
				proof.Edges = []*graph.Edge{call}
			case "call":
				proof.Edges = []*graph.Edge{member}
			}
			stats, err := RunRebuildObjectRegistry(t.Context(), private, rebuildGRPCReceipts(proof), selection)
			if bad == "none" {
				require.NoError(t, err)
				require.Equal(t, uint64(1), stats.Moves)
				native := openRebuildTemporalOracle(t, facts, remote)
				require.Equal(t, 1, ResolveObjectRegistryCalls(native))
				require.Equal(t, rebuildEventSQLiteRows(native), rebuildEventSavedRows(t, private))
				return
			}
			require.ErrorIs(t, err, store_pebble.ErrRebuildObjectRegistry)
			var refused *store_pebble.RebuildUnknownProvenanceError
			require.ErrorAs(t, err, &refused)
			want := graph.EdgeIdentityFor(call)
			want.To = local.ID
			require.Equal(t, &store_pebble.RebuildUnknownProvenanceError{Count: 1, First: want}, refused)
			require.Zero(t, stats)
			require.Equal(t, before, rebuildEventSavedRows(t, private))
		})
	}
}

func TestRebuildObjectRegistryRoleProof(t *testing.T) {
	const file, otherFile, recordedFile = "registry.js", "other.js", "recorded.js"
	caller := &graph.Node{ID: file + "::dispatch", Name: "dispatch", Kind: graph.KindFunction, FilePath: file, Language: "javascript"}
	local, member := objectRegistryTestMethod(file, file+"::Cmd.run", "run", "Cmd")
	rival, other := objectRegistryTestMethod(otherFile, otherFile+"::Cmd.run", "run", "Cmd")
	call := objectRegistryTestCall(caller.ID, "unresolved::*.Cmd", file, "Cmd", "run", 10)
	facts := rebuildEventFacts(file, "javascript", []*graph.Node{caller, local}, member, call)
	recordedCaller := *caller
	recordedCaller.Kind, recordedCaller.Language = graph.KindMethod, "typescript"
	recordedCandidate := *local
	recordedCandidate.Kind, recordedCandidate.FilePath = graph.KindFunction, recordedFile
	proof := []store_pebble.ParseFacts{
		rebuildEventFacts(file, "javascript", []*graph.Node{&recordedCaller}, member, call),
		rebuildEventFacts(recordedFile, "javascript", []*graph.Node{&recordedCandidate}),
	}
	selection, err := NewFrameworkSynthesizerSelection([]string{SynthObjectRegistry})
	require.NoError(t, err)
	for _, withRival := range []bool{false, true} {
		t.Run(fmt.Sprint(withRival), func(t *testing.T) {
			input, receipts := []store_pebble.ParseFacts{facts}, slices.Clone(proof)
			if withRival {
				remote := rebuildEventFacts(otherFile, "javascript", []*graph.Node{rival}, other)
				input, receipts = append(input, remote), append(receipts, remote)
			}
			private := openRebuildGRPCBuild(t, input...)
			before := rebuildEventSavedRows(t, private)
			stats, err := RunRebuildObjectRegistry(t.Context(), private, rebuildGRPCReceipts(receipts...), selection)
			if withRival {
				// Ambiguity makes native read the caller-file candidate pool.
				require.ErrorIs(t, err, store_pebble.ErrRebuildUnknownProvenance)
				require.Zero(t, stats)
				require.Equal(t, before, rebuildEventSavedRows(t, private))
				return
			}
			require.NoError(t, err)
			require.Equal(t, uint64(1), stats.Moves)
			native := openRebuildTemporalOracle(t, facts)
			require.Equal(t, 1, ResolveObjectRegistryCalls(native))
			require.Equal(t, rebuildEventSQLiteRows(native), rebuildEventSavedRows(t, private))
		})
	}
}

func TestRebuildObjectRegistryFrozenSelection(t *testing.T) {
	const file = "registry.js"
	method, member := objectRegistryTestMethod(file, file+"::Cmd.run", "run", "Cmd")
	malformed := objectRegistryTestCall(file+"::dispatch", "unresolved::*.Cmd", file, "", "run", 3)
	b := openRebuildGRPCBuild(t, rebuildEventFacts(file, "javascript", []*graph.Node{method}, member, malformed))
	selection, err := NewFrameworkSynthesizerSelection([]string{SynthObjectRegistry})
	require.NoError(t, err)
	frozen, err := FreezeRebuildFrameworkSelection(t.Context(), b.RebuildView(), selection)
	require.NoError(t, err)
	require.False(t, frozen.allows(SynthObjectRegistry))
	b.AddEdge(objectRegistryTestCall(malformed.From, malformed.To, file, "Cmd", "run", 4))
	withLateTag := rebuildEventSavedRows(t, b)
	stats, err := RunRebuildObjectRegistry(t.Context(), b, nil, frozen)
	require.NoError(t, err)
	require.Zero(t, stats)
	require.Equal(t, withLateTag, rebuildEventSavedRows(t, b))
}

func TestRebuildObjectRegistrySQLiteMoveWindow(t *testing.T) {
	const file, dispatch = "registry.js", "registry.js::dispatch"
	b, bMember := objectRegistryTestMethod(file, file+"::B.run", "run", "B")
	c, cMember := objectRegistryTestMethod(file, file+"::C.run", "run", "C")
	caller := &graph.Node{ID: dispatch, Name: "dispatch", Kind: graph.KindFunction, FilePath: file, Language: "javascript"}
	nodes := []*graph.Node{caller, b, c}
	aMove := objectRegistryTestCall(dispatch, "unresolved::a", file, "B", "run", 1)
	bMove := objectRegistryTestCall(dispatch, b.ID, file, "C", "run", 1)
	edges := []*graph.Edge{bMember, cMember, aMove}
	for i := range graph.SQLiteReindexChunkSize - 1 {
		edges = append(edges, objectRegistryTestCall(dispatch, fmt.Sprintf("unresolved::fill%d", i), file, "C", "run", i+2))
	}
	edges = append(edges, bMove)
	selection, err := NewFrameworkSynthesizerSelection([]string{SynthObjectRegistry})
	require.NoError(t, err)
	for _, sameWindow := range []bool{false, true} {
		t.Run(fmt.Sprintf("same_window_%t", sameWindow), func(t *testing.T) {
			input := slices.Clone(edges)
			if sameWindow {
				last := len(input) - 1
				input[last-1], input[last] = input[last], input[last-1]
			}
			facts := rebuildEventFacts(file, "javascript", nodes, input...)
			native := openRebuildTemporalOracle(t, facts)
			private := openRebuildGRPCBuild(t, facts)
			require.Equal(t, graph.SQLiteReindexChunkSize+1, ResolveObjectRegistryCalls(native))
			want := rebuildEventSQLiteRows(native)
			hasB := slices.ContainsFunc(want.edges, func(e *graph.Edge) bool { return e.From == dispatch && e.To == b.ID && e.Line == 1 })
			require.Equal(t, sameWindow, hasB)
			stats, err := RunRebuildObjectRegistry(t.Context(), private, rebuildGRPCReceipts(facts), selection)
			require.NoError(t, err)
			require.Equal(t, uint64(graph.SQLiteReindexChunkSize+1), stats.Moves)
			require.Equal(t, want, rebuildEventSavedRows(t, private))
		})
	}
}

func TestRebuildObjectRegistryUnpositionedCandidateReceipt(t *testing.T) {
	const file, otherFile = "registry.js", "other.js"
	local, member := objectRegistryTestMethod(file, file+"::Cmd.run", "run", "Cmd")
	rival, other := objectRegistryTestMethod(otherFile, otherFile+"::Cmd.run", "run", "Cmd")
	caller := &graph.Node{ID: file + "::dispatch", Name: "dispatch", Kind: graph.KindFunction, FilePath: file, Language: "javascript"}
	call := objectRegistryTestCall(caller.ID, "unresolved::*.Cmd", file, "Cmd", "run", 3)
	facts := rebuildEventFacts(file, "javascript", []*graph.Node{caller, local}, member, call)
	selection, err := NewFrameworkSynthesizerSelection([]string{SynthObjectRegistry})
	require.NoError(t, err)
	for _, change := range []string{"file", "name", "workspace"} {
		t.Run(change, func(t *testing.T) {
			live := *rival
			raw := live
			switch change {
			case "file":
				raw.FilePath = file
			case "name":
				live.Name = "other"
			case "workspace":
				live.WorkspaceID = "foreign"
			}
			remote := rebuildEventFacts(live.FilePath, "javascript", []*graph.Node{&live})
			otherMember := *other
			otherMember.FilePath = live.FilePath
			remote.Edges = []*graph.Edge{&otherMember}
			private := openRebuildGRPCBuild(t, facts, remote)
			before := rebuildEventSavedRows(t, private)
			// The rival has a declaration receipt, but no membership position.
			proof := rebuildEventFacts(raw.FilePath, "javascript", []*graph.Node{&raw})
			stats, err := RunRebuildObjectRegistry(t.Context(), private, rebuildGRPCReceipts(facts, proof), selection)
			require.ErrorIs(t, err, store_pebble.ErrRebuildUnknownProvenance)
			require.Zero(t, stats)
			require.Equal(t, before, rebuildEventSavedRows(t, private))
		})
	}
}

func TestRebuildObjectRegistryStationaryMembershipOrder(t *testing.T) {
	const file = "registry.js"
	method, member := objectRegistryTestMethod(file, file+"::Cmd.run", "run", "Cmd")
	caller := &graph.Node{ID: file + "::dispatch", Kind: graph.KindFunction, FilePath: file, Language: "javascript"}
	call := objectRegistryTestCall(caller.ID, method.ID, file, "Cmd", "run", 3)
	selection, err := NewFrameworkSynthesizerSelection([]string{SynthObjectRegistry})
	require.NoError(t, err)
	for _, class := range []string{"Cmd", "Other"} {
		t.Run(class, func(t *testing.T) {
			other := *member
			other.To = "other.js::" + class
			facts := rebuildEventFacts(file, "javascript", []*graph.Node{method, caller}, member, &other, call)
			private := openRebuildGRPCBuild(t, facts)
			before := rebuildEventSavedRows(t, private)
			proof := facts
			proof.Edges = []*graph.Edge{member, call}
			stats, err := RunRebuildObjectRegistry(t.Context(), private, rebuildGRPCReceipts(proof), selection)
			if class == "Other" {
				require.ErrorIs(t, err, store_pebble.ErrRebuildUnknownProvenance)
				require.Zero(t, stats)
				native := openRebuildTemporalOracle(t, facts)
				require.Zero(t, ResolveObjectRegistryCalls(native))
				want := *call
				want.To, want.Origin = "unresolved::*.Cmd", graph.OriginASTInferred
				require.Contains(t, rebuildEventSQLiteRows(native).edges, &want)
			} else {
				require.NoError(t, err)
				require.Equal(t, store_pebble.RebuildObjectRegistryStats{Stubs: 1, Resolved: 1}, stats)
			}
			require.Equal(t, before, rebuildEventSavedRows(t, private))
		})
	}
}
