package resolver

import (
	"slices"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/graph/store_pebble"
)

const springRebuildFile = "Events.java"

func springRebuildNode(name, listens string) *graph.Node {
	return &graph.Node{ID: springRebuildFile + "::" + name, Name: name, Kind: graph.KindMethod, FilePath: springRebuildFile,
		Language: "java", Meta: map[string]any{"spring_listener_type": listens}}
}

func springRebuildCall(from, to, event string, line int) *graph.Edge {
	return &graph.Edge{From: from, To: to, FilePath: springRebuildFile, Kind: graph.EdgeCalls, Line: line,
		Origin: "test-origin", Meta: map[string]any{"via": springEventVia, "spring_event_type": event}}
}

func springRebuildArc(from, to string, kind graph.EdgeKind, line int) *graph.Edge {
	return &graph.Edge{From: springRebuildFile + "::" + from, To: springRebuildFile + "::" + to,
		FilePath: springRebuildFile, Kind: kind, Line: line}
}

func springRebuildSelection(t *testing.T, b *store_pebble.BuildStore) FrameworkSynthesizerSelection {
	t.Helper()
	selection, err := NewFrameworkSynthesizerSelection([]string{SynthSpringEvent})
	require.NoError(t, err)
	frozen, err := FreezeRebuildFrameworkSelection(t.Context(), b.RebuildView(), selection)
	require.NoError(t, err)
	return frozen
}

func TestRebuildSpringEventsSQLiteHierarchy(t *testing.T) {
	pub := springRebuildNode("publish", "")
	first := springRebuildNode("A", "Child")
	second := springRebuildNode("B", "demo.Base<Body>")
	third := springRebuildNode("C", "Contract")
	third.Kind = graph.KindFunction
	ext := springRebuildArc("Child", "Base", graph.EdgeExtends, 1)
	impl := springRebuildArc("Base", "Contract", graph.EdgeImplements, 2)
	cycle := springRebuildArc("Contract", "Child", graph.EdgeExtends, 3)
	duplicate := springRebuildArc("pkg.Child", "pkg.Base", graph.EdgeExtends, 4)
	bind := springRebuildCall(pub.ID, "unresolved::legacy", "Child", 10)
	stationary := springRebuildCall(pub.ID, first.ID, "Child", 11)
	miss := springRebuildCall(pub.ID, "unresolved::unseen", "Missing", 12)
	nodes := []*graph.Node{pub, first, second, third}
	for _, tc := range []struct {
		name     string
		arcs     []*graph.Edge
		resolved uint64
		moves    uint64
		adds     uint64
	}{
		{"inherited fanout", []*graph.Edge{ext, impl, cycle, duplicate}, 6, 2, 4},
		{"removed hierarchy", nil, 2, 2, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			stale := springRebuildCall(pub.ID, second.ID, "Missing", 13)
			stale.Confidence = ConfidenceTyped
			stale.ConfidenceLabel = graph.ConfidenceLabelFor(graph.EdgeCalls, ConfidenceTyped)
			StampSynthesizedTyped(stale, SynthSpringEvent)
			input := slices.Concat(tc.arcs, []*graph.Edge{bind, stationary, miss, stale})
			facts := rebuildEventFacts(springRebuildFile, "java", nodes, input...)
			b, native := openRebuildEventStores(t, facts)
			before := rebuildEventSQLiteRows(native)
			require.Contains(t, before.edges, stationary)
			require.Equal(t, int(tc.resolved), ResolveSpringEventCalls(native))
			want := rebuildEventSQLiteRows(native)
			receipt := facts
			// The qualified duplicate does not need proof: ext proves the same simple-name arc.
			receipt.Edges = slices.DeleteFunc(slices.Clone(input), func(e *graph.Edge) bool { return e == duplicate })
			frozen := springRebuildSelection(t, b)
			require.True(t, frozen.allows(SynthSpringEvent))
			stats, err := RunRebuildSpringEvents(t.Context(), b, rebuildGRPCReceipts(receipt), frozen)
			require.NoError(t, err)
			require.Equal(t, store_pebble.RebuildSpringEventStats{Stubs: 4, Resolved: tc.resolved, Moves: tc.moves, Adds: tc.adds}, stats)
			require.Equal(t, want, rebuildEventSavedRows(t, b))
			require.Contains(t, want.edges, stationary)
			require.Contains(t, want.edges, miss)
			for _, e := range want.edges {
				if e.Line == stale.Line {
					require.Equal(t, "unresolved::*.Missing", e.To)
					require.Equal(t, stale.Origin, e.Origin)
					require.Zero(t, e.Confidence)
					require.NotContains(t, e.Meta, MetaSynthesizedBy)
				}
			}
		})
	}
}

func TestRebuildSpringEventsSQLiteFanoutOrder(t *testing.T) {
	pub := springRebuildNode("publish", "")
	first := springRebuildNode("A", "Event")
	second := springRebuildNode("B", "Event")
	z := springRebuildCall(pub.ID, "unresolved::z", "demo.Event", 10)
	a := springRebuildCall(pub.ID, "unresolved::a", "Event", 10)
	// This occupied fanout key moves to A; it loses to z there. B is now
	// free and must take z's payload rather than a's complete-key-first payload.
	moving := springRebuildCall(pub.ID, second.ID, "Event", 10)
	incumbent := &graph.Edge{From: pub.ID, To: second.ID, Kind: graph.EdgeCalls, FilePath: springRebuildFile, Line: 11, Origin: "incumbent"}
	blocked := springRebuildCall(pub.ID, first.ID, "Event", 11)
	nodes := []*graph.Node{pub, first, second}
	facts := rebuildEventFacts(springRebuildFile, "java", nodes, z, a, moving, blocked, incumbent)
	b, native := openRebuildEventStores(t, facts)
	require.Equal(t, 8, ResolveSpringEventCalls(native))
	want := rebuildEventSQLiteRows(native)
	require.Contains(t, want.edges, springEventFanout(z, "demo.Event", second.ID))
	require.Contains(t, want.edges, incumbent)
	receipt := facts
	receipt.Edges = []*graph.Edge{z, a, moving, incumbent} // blocked cannot change an output
	stats, err := RunRebuildSpringEvents(t.Context(), b, rebuildGRPCReceipts(receipt), springRebuildSelection(t, b))
	require.NoError(t, err)
	require.Equal(t, store_pebble.RebuildSpringEventStats{Stubs: 4, Resolved: 8, Moves: 3, Adds: 1}, stats)
	require.Equal(t, want, rebuildEventSavedRows(t, b))
}

func TestRebuildSpringEventsSQLiteWindow(t *testing.T) {
	pub := springRebuildNode("publish", "")
	first := springRebuildNode("A", "Event")
	second := springRebuildNode("B", "Event")
	z := springRebuildCall(pub.ID, "unresolved::z", "demo.Event", 1)
	calls := []*graph.Edge{z}
	for i := 1; i < graph.SQLiteReindexChunkSize; i++ {
		calls = append(calls, springRebuildCall(pub.ID, "unresolved::filler", "Event", i+1))
	}
	a := springRebuildCall(pub.ID, second.ID, "Event", 1)
	calls = append(calls, a)
	facts := rebuildEventFacts(springRebuildFile, "java", []*graph.Node{pub, first, second}, calls...)
	b, native := openRebuildEventStores(t, facts)
	require.Equal(t, len(calls)*2, ResolveSpringEventCalls(native))
	want := rebuildEventSQLiteRows(native)
	require.Contains(t, want.edges, springEventFanout(z, "demo.Event", second.ID))
	stats, err := RunRebuildSpringEvents(t.Context(), b, rebuildGRPCReceipts(facts), springRebuildSelection(t, b))
	require.NoError(t, err)
	require.Equal(t, uint64(len(calls)), stats.Moves)
	require.Equal(t, uint64(graph.SQLiteReindexChunkSize), stats.Adds)
	require.Equal(t, want, rebuildEventSavedRows(t, b))
}

func TestRebuildSpringEventsRefusesUnprovedOutput(t *testing.T) {
	pub := springRebuildNode("publish", "")
	first := springRebuildNode("A", "Child")
	second := springRebuildNode("B", "Base")
	ext := springRebuildArc("Child", "Base", graph.EdgeExtends, 1)
	call := springRebuildCall(pub.ID, "unresolved::old", "Child", 10)
	nodes := []*graph.Node{pub, first, second}
	input := rebuildEventFacts(springRebuildFile, "java", nodes, ext, call)
	for _, tc := range []struct {
		name  string
		nodes []*graph.Node
		edges []*graph.Edge
		to    string
	}{
		{"listener", []*graph.Node{pub, first}, input.Edges, second.ID},
		{"hierarchy", nodes, []*graph.Edge{call}, second.ID},
		{"raw Call", nodes, []*graph.Edge{ext}, first.ID},
	} {
		t.Run(tc.name, func(t *testing.T) {
			b := openRebuildGRPCBuild(t, input)
			before := rebuildEventSavedRows(t, b)
			receipt := rebuildEventFacts(springRebuildFile, "java", tc.nodes, tc.edges...)
			stats, err := RunRebuildSpringEvents(t.Context(), b, rebuildGRPCReceipts(receipt), springRebuildSelection(t, b))
			require.ErrorIs(t, err, store_pebble.ErrRebuildSpringEvents)
			var refused *store_pebble.RebuildUnknownProvenanceError
			require.ErrorAs(t, err, &refused)
			want := graph.EdgeIdentityFor(call)
			want.To = tc.to
			require.Equal(t, want, refused.First)
			require.Equal(t, store_pebble.RebuildSpringEventStats{}, stats)
			require.Equal(t, before, rebuildEventSavedRows(t, b))
		})
	}
}

func TestRebuildSpringEventsFrozenSelection(t *testing.T) {
	pub := springRebuildNode("publish", "")
	listener := springRebuildNode("A", "Event")
	b := openRebuildGRPCBuild(t, rebuildEventFacts(springRebuildFile, "java", []*graph.Node{pub, listener}))
	frozen := springRebuildSelection(t, b)
	require.False(t, frozen.allows(SynthSpringEvent))
	call := springRebuildCall(pub.ID, "unresolved::old", "Event", 10)
	b.AddEdge(call)
	stats, err := RunRebuildSpringEvents(t.Context(), b, nil, frozen)
	require.NoError(t, err)
	require.Equal(t, store_pebble.RebuildSpringEventStats{}, stats)
	require.True(t, springRebuildSelection(t, b).allows(SynthSpringEvent))
}

func TestRebuildSpringEventsUsesSettledProducerProof(t *testing.T) {
	pub := springRebuildNode("publish", "")
	listener := springRebuildNode("A", "Contract")
	raw := springRebuildCall(pub.ID, "unresolved::old", "Child", 10)
	facts := rebuildEventFacts(springRebuildFile, "java", []*graph.Node{pub, listener}, raw)
	const ts = "types.ts"
	owner := &graph.Node{ID: ts + "::Child", Name: "Child", FilePath: ts, Kind: graph.KindType, Language: "typescript", StartLine: 2}
	iface := &graph.Node{ID: ts + "::Contract", Name: "Contract", FilePath: ts, Kind: graph.KindInterface,
		Language: "typescript", Meta: map[string]any{"methods": []string{"run"}}}
	method := &graph.Node{ID: owner.ID + ".run", Name: "run", FilePath: ts, Kind: graph.KindMethod, Language: "typescript"}
	member := &graph.Edge{From: method.ID, To: owner.ID, FilePath: ts, Kind: graph.EdgeMemberOf, Line: 3}
	types := rebuildEventFacts(ts, "typescript", []*graph.Node{owner, iface, method}, member)
	settled := *raw
	settled.To = pub.ID
	for _, proved := range []bool{true, false} {
		b, native := openRebuildEventStores(t, facts, types)
		require.NoError(t, b.ApplyLinkReindexes(t.Context(), []graph.EdgeReindex{{Edge: &settled, OldTo: raw.To}}))
		native.ReindexEdges([]graph.EdgeReindex{{Edge: &settled, OldTo: raw.To}})
		require.Equal(t, 1, New(native).InferImplements())
		receipts := rebuildGRPCReceipts(facts, types)
		if proved {
			require.NoError(t, RunRebuildImplements(t.Context(), b, receipts))
		} else {
			b.AddEdge(methodSetImplementationEdge(owner, iface.ID))
		}
		before := rebuildEventSavedRows(t, b)
		require.Equal(t, rebuildEventSQLiteRows(native), before)
		require.Equal(t, 1, ResolveSpringEventCalls(native))
		stats, err := RunRebuildSpringEvents(t.Context(), b, receipts, springRebuildSelection(t, b))
		if !proved {
			var refused *store_pebble.RebuildUnknownProvenanceError
			require.ErrorAs(t, err, &refused)
			require.Equal(t, listener.ID, refused.First.To)
			require.Equal(t, before, rebuildEventSavedRows(t, b))
			continue
		}
		require.NoError(t, err)
		require.Equal(t, store_pebble.RebuildSpringEventStats{Stubs: 1, Resolved: 1, Moves: 1}, stats)
		require.Equal(t, rebuildEventSQLiteRows(native), rebuildEventSavedRows(t, b))
	}
}

func TestRebuildSpringEventsReceiptListenerEntrants(t *testing.T) {
	pub := springRebuildNode("publish", "")
	first := springRebuildNode("B", "Event")
	call := springRebuildCall(pub.ID, "unresolved::old", "Event", 10)
	for _, tc := range []struct {
		name, entrant, liveType, rawType string
		occupy                           bool
		refuse                           bool
	}{
		{name: "new first", entrant: "A", rawType: "Event", refuse: true},
		{name: "new secondary", entrant: "C", liveType: "Other", rawType: "Event", refuse: true},
		{name: "unreachable", entrant: "A", rawType: "Other"},
		{name: "occupied secondary", entrant: "C", rawType: "Event", occupy: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			live := springRebuildNode(tc.entrant, tc.liveType)
			raw := springRebuildNode(tc.entrant, tc.rawType)
			edges := []*graph.Edge{call}
			if tc.occupy {
				edges = append(edges, &graph.Edge{From: pub.ID, To: live.ID, Kind: graph.EdgeCalls, FilePath: springRebuildFile, Line: call.Line})
			}
			b := openRebuildGRPCBuild(t, rebuildEventFacts(springRebuildFile, "java", []*graph.Node{pub, first, live}, edges...))
			before := rebuildEventSavedRows(t, b)
			receipt := rebuildEventFacts(springRebuildFile, "java", []*graph.Node{pub, first, raw}, edges...)
			stats, err := RunRebuildSpringEvents(t.Context(), b, rebuildGRPCReceipts(receipt), springRebuildSelection(t, b))
			if tc.refuse {
				var refused *store_pebble.RebuildUnknownProvenanceError
				require.ErrorAs(t, err, &refused)
				require.Equal(t, raw.ID, refused.First.To)
				require.Zero(t, stats)
				require.Equal(t, before, rebuildEventSavedRows(t, b))
				return
			}
			require.NoError(t, err)
			require.Equal(t, store_pebble.RebuildSpringEventStats{Stubs: 1, Resolved: 1, Moves: 1}, stats)
		})
	}
}

func TestRebuildSpringEventsProofTerms(t *testing.T) {
	pub := springRebuildNode("publish", "")
	first := springRebuildNode("A", "Child")
	second := springRebuildNode("B", "Base")
	arc := springRebuildArc("Child", "Base", graph.EdgeExtends, 1)
	move := springRebuildCall(pub.ID, "unresolved::old", "Child", 10)
	reset := springRebuildCall(pub.ID, pub.ID, "Miss", 11)
	stationary := springRebuildCall(pub.ID, first.ID, "Child", 12)
	for _, tc := range []struct {
		name       string
		nodes      []*graph.Node
		rawNodes   []*graph.Node
		input      []*graph.Edge
		rawEdges   []*graph.Edge
		misfileArc bool
		want       *graph.Edge
		to         string
	}{
		{name: "primary without listener proof", nodes: []*graph.Node{pub, first}, rawNodes: []*graph.Node{pub}, input: []*graph.Edge{move}, rawEdges: []*graph.Edge{move}, want: move, to: first.ID},
		{name: "reset without global witness", nodes: []*graph.Node{pub, first}, rawNodes: []*graph.Node{pub}, input: []*graph.Edge{reset}, rawEdges: []*graph.Edge{reset}, want: reset, to: "unresolved::*.Miss"},
		{name: "add without earlier listener proof", nodes: []*graph.Node{pub, first, second}, rawNodes: []*graph.Node{pub, second}, input: []*graph.Edge{arc, stationary}, rawEdges: []*graph.Edge{arc, stationary}, want: stationary, to: second.ID},
		{name: "arc in wrong receipt file", nodes: []*graph.Node{pub, second}, rawNodes: []*graph.Node{pub, second}, input: []*graph.Edge{arc, move}, rawEdges: []*graph.Edge{move}, misfileArc: true, want: move, to: second.ID},
	} {
		t.Run(tc.name, func(t *testing.T) {
			b := openRebuildGRPCBuild(t, rebuildEventFacts(springRebuildFile, "java", tc.nodes, tc.input...))
			before := rebuildEventSavedRows(t, b)
			receipts := []store_pebble.ParseFacts{rebuildEventFacts(springRebuildFile, "java", tc.rawNodes, tc.rawEdges...)}
			if tc.misfileArc {
				receipts = append(receipts, rebuildEventFacts("Other.java", "java", nil, arc))
			}
			stats, err := RunRebuildSpringEvents(t.Context(), b, rebuildGRPCReceipts(receipts...), springRebuildSelection(t, b))
			var refused *store_pebble.RebuildUnknownProvenanceError
			require.ErrorAs(t, err, &refused)
			want := graph.EdgeIdentityFor(tc.want)
			want.To = tc.to
			require.Equal(t, &store_pebble.RebuildUnknownProvenanceError{Count: 1, First: want}, refused)
			require.Zero(t, stats)
			require.Equal(t, before, rebuildEventSavedRows(t, b))
		})
	}
}

func TestRebuildSpringEventsListenerReceiptFields(t *testing.T) {
	pub := springRebuildNode("publish", "")
	first := springRebuildNode("A", "Child")
	call := springRebuildCall(pub.ID, "unresolved::old", "Child", 10)
	input := rebuildEventFacts(springRebuildFile, "java", []*graph.Node{pub, first}, call)
	for _, tc := range []struct {
		name   string
		kind   graph.NodeKind
		listen string
		refuse bool
	}{
		{"language and receipt file differ", graph.KindMethod, "Child", false},
		{"eligible kind differs", graph.KindFunction, "Child", false},
		{"qualified marker differs", graph.KindMethod, "pkg.Child", false},
		{"ineligible kind", graph.KindType, "Child", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			b := openRebuildGRPCBuild(t, input)
			before := rebuildEventSavedRows(t, b)
			raw := *first
			raw.Kind, raw.FilePath, raw.Language = tc.kind, "Other.kts", "kotlin"
			raw.Meta = map[string]any{"spring_listener_type": tc.listen}
			receipts := rebuildGRPCReceipts(rebuildEventFacts(springRebuildFile, "java", []*graph.Node{pub}, call),
				rebuildEventFacts(raw.FilePath, "kotlin-script", []*graph.Node{&raw}))
			stats, err := RunRebuildSpringEvents(t.Context(), b, receipts, springRebuildSelection(t, b))
			if tc.refuse {
				require.ErrorIs(t, err, store_pebble.ErrRebuildUnknownProvenance)
				require.Zero(t, stats)
				require.Equal(t, before, rebuildEventSavedRows(t, b))
				return
			}
			require.NoError(t, err)
			require.Equal(t, store_pebble.RebuildSpringEventStats{Stubs: 1, Resolved: 1, Moves: 1}, stats)
		})
	}
}

func TestRebuildSpringEventsStationarySoleListener(t *testing.T) {
	pub := springRebuildNode("publish", "")
	first := springRebuildNode("A", "Event")
	other := springRebuildNode("B", "Other")
	call := springRebuildCall(pub.ID, first.ID, "Event", 10)
	input := rebuildEventFacts(springRebuildFile, "java", []*graph.Node{pub, first, other}, call)
	for _, listenerProved := range []bool{false, true} {
		b := openRebuildGRPCBuild(t, input)
		before := rebuildEventSavedRows(t, b)
		nodes := []*graph.Node{pub, other}
		if listenerProved {
			nodes = append(nodes, first)
		}
		// With the listener proved, neither the presence nor the absence of
		// this Call can cause a write, so the Call receipt is unnecessary.
		receipt := rebuildEventFacts(springRebuildFile, "java", nodes)
		stats, err := RunRebuildSpringEvents(t.Context(), b, rebuildGRPCReceipts(receipt), springRebuildSelection(t, b))
		if listenerProved {
			require.NoError(t, err)
			require.Equal(t, store_pebble.RebuildSpringEventStats{Stubs: 1, Resolved: 1}, stats)
		} else {
			var refused *store_pebble.RebuildUnknownProvenanceError
			require.ErrorAs(t, err, &refused)
			want := graph.EdgeIdentityFor(call)
			want.To = "unresolved::*.Event"
			require.Equal(t, &store_pebble.RebuildUnknownProvenanceError{Count: 1, First: want}, refused)
			require.Zero(t, stats)
		}
		require.Equal(t, before, rebuildEventSavedRows(t, b))
	}
}

func TestRebuildSpringEventsStationaryPrimaryWithOccupiedFanout(t *testing.T) {
	pub := springRebuildNode("publish", "")
	first, second := springRebuildNode("A", "Event"), springRebuildNode("B", "Event")
	call := springRebuildCall(pub.ID, first.ID, "Event", 10)
	incumbent := *call
	incumbent.To, incumbent.Meta = second.ID, nil
	b := openRebuildGRPCBuild(t, rebuildEventFacts(springRebuildFile, "java", []*graph.Node{pub, first, second}, call, &incumbent))
	before := rebuildEventSavedRows(t, b)
	receipt := rebuildEventFacts(springRebuildFile, "java", []*graph.Node{pub, second}, call, &incumbent)
	_, native := openRebuildEventStores(t, receipt)
	require.Equal(t, 1, ResolveSpringEventCalls(native))
	require.NotContains(t, rebuildEventSQLiteRows(native).edges, call)

	stats, err := RunRebuildSpringEvents(t.Context(), b, rebuildGRPCReceipts(receipt), springRebuildSelection(t, b))
	var refusal *store_pebble.RebuildUnknownProvenanceError
	require.ErrorAs(t, err, &refusal)
	require.Equal(t, &store_pebble.RebuildUnknownProvenanceError{Count: 1, First: graph.EdgeIdentityFor(&incumbent)}, refusal)
	require.Zero(t, stats)
	require.Equal(t, before, rebuildEventSavedRows(t, b))
}

func TestRebuildSpringEventsReceiptArcEntrants(t *testing.T) {
	pub := springRebuildNode("publish", "")
	first := springRebuildNode("B", "Child")
	call := springRebuildCall(pub.ID, first.ID, "Child", 10)
	for _, tc := range []struct {
		name     string
		listener string
		from     string
		kind     graph.EdgeKind
		refuse   bool
	}{
		{"earlier via Extends", "A", "Child", graph.EdgeExtends, true},
		{"fanout via Implements", "C", "Child", graph.EdgeImplements, true},
		{"unreachable arc", "A", "Other", graph.EdgeExtends, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			entrant := springRebuildNode(tc.listener, "Base")
			nodes := []*graph.Node{pub, first, entrant}
			b := openRebuildGRPCBuild(t, rebuildEventFacts(springRebuildFile, "java", nodes, call))
			before := rebuildEventSavedRows(t, b)
			// Reach the live listener only by crossing both receipt-only arcs.
			link := springRebuildArc(tc.from, "Middle", tc.kind, 2)
			parent := springRebuildArc("Middle", "Base", graph.EdgeExtends, 3)
			receipt := rebuildEventFacts(springRebuildFile, "java", nodes, call, link, parent)
			stats, err := RunRebuildSpringEvents(t.Context(), b, rebuildGRPCReceipts(receipt), springRebuildSelection(t, b))
			if tc.refuse {
				var refusal *store_pebble.RebuildUnknownProvenanceError
				require.ErrorAs(t, err, &refusal)
				want := graph.EdgeIdentityFor(call)
				want.To = entrant.ID
				require.Equal(t, &store_pebble.RebuildUnknownProvenanceError{Count: 1, First: want}, refusal)
				require.Zero(t, stats)
			} else {
				require.NoError(t, err)
				require.Equal(t, store_pebble.RebuildSpringEventStats{Stubs: 1, Resolved: 1}, stats)
			}
			require.Equal(t, before, rebuildEventSavedRows(t, b))
		})
	}
}

func TestRebuildSpringEventsReceiptOnlyActivation(t *testing.T) {
	pub := springRebuildNode("publish", "")
	for _, tc := range []struct {
		name, liveType, rawType, oldTo, refusalTo string
	}{
		{"no listeners on either side", "", "", pub.ID, ""},
		{"unrelated entrant activates reset", "", "Other", pub.ID, "unresolved::*.Event"},
		{"unrelated entrant cannot reset unresolved Call", "", "Other", "unresolved::old", ""},
		{"entrant binds even with no live listeners", "", "Event", "unresolved::old", springRebuildFile + "::A"},
		{"unproved sole listener cannot reset stationary Call", "Event", "", springRebuildFile + "::A", ""},
		{"unrelated entrant makes sole-listener loss matter", "Event", "Other", springRebuildFile + "::A", "unresolved::*.Event"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			live, raw := springRebuildNode("A", tc.liveType), springRebuildNode("A", tc.rawType)
			call := springRebuildCall(pub.ID, tc.oldTo, "Event", 10)
			b := openRebuildGRPCBuild(t, rebuildEventFacts(springRebuildFile, "java", []*graph.Node{pub, live}, call))
			before := rebuildEventSavedRows(t, b)
			receipt := rebuildEventFacts(springRebuildFile, "java", []*graph.Node{pub, raw}, call)
			stats, err := RunRebuildSpringEvents(t.Context(), b, rebuildGRPCReceipts(receipt), springRebuildSelection(t, b))
			switch {
			case tc.refusalTo != "":
				var refusal *store_pebble.RebuildUnknownProvenanceError
				require.ErrorAs(t, err, &refusal)
				want := graph.EdgeIdentityFor(call)
				want.To = tc.refusalTo
				require.Equal(t, &store_pebble.RebuildUnknownProvenanceError{Count: 1, First: want}, refusal)
				require.Zero(t, stats)
			case tc.liveType == "":
				require.NoError(t, err)
				require.Zero(t, stats)
			default:
				require.NoError(t, err)
				require.Equal(t, store_pebble.RebuildSpringEventStats{Stubs: 1, Resolved: 1}, stats)
			}
			require.Equal(t, before, rebuildEventSavedRows(t, b))
		})
	}
}
