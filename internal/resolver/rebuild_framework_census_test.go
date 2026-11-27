package resolver

import (
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/graph/store_pebble"
)

func TestRebuildFrameworkCensusSetStateUsesPreLoopCalls(t *testing.T) {
	const (
		file      = "widget.ts"
		language  = "typescript"
		topic     = "event::pubsub::eventemitter::change"
		transport = "eventemitter"
		emitLine  = 11
	)
	owner := rebuildEventNode(file, "Widget", language, "")
	owner.Kind = graph.KindType
	caller := rebuildEventNode(file, "Widget.refresh", language, "")
	listener := rebuildEventNode(file, "Widget.setState", language, "")
	render := rebuildEventNode(file, "Widget.render", language, "")
	build := rebuildEventNode(file, "Widget.build", language, "")
	channel := rebuildEventTopic(file, language, topic, "change", transport)
	nodes := []*graph.Node{owner, channel}
	edges := []*graph.Edge{
		rebuildEventSite(caller, topic, graph.EdgeEmits, emitLine, transport),
		rebuildEventSite(listener, topic, graph.EdgeListensOn, 21, transport),
	}
	caller.Name, listener.Name, render.Name, build.Name = "refresh", "setState", "render", "build"
	for i, method := range []*graph.Node{caller, listener, render, build} {
		method.Kind = graph.KindMethod
		method.StartLine = (i + 1) * 10
		nodes = append(nodes, method)
		edges = append(edges, rebuildObserverEdge(method, owner, graph.EdgeMemberOf, method.StartLine))
	}
	b, native, proof := openRebuildReactSetStateStores(t, rebuildEventFacts(file, language, nodes, edges...))
	before := rebuildEventSavedRows(t, b)
	for _, e := range before.edges {
		require.NotEqual(t, graph.EdgeCalls, e.Kind)
	}
	selection, err := NewFrameworkSynthesizerSelection([]string{
		SynthEventChannel, SynthReactSetState, SynthFlutterSetState,
	})
	require.NoError(t, err)
	ctx := t.Context()
	receipts := rebuildGRPCReceipts(proof...)

	// The first census cannot see the event Call. The next census can see
	// that same Call, without changing the parse receipts or seeding a Call.
	for run := range 2 {
		frozen, err := FreezeRebuildFrameworkSelection(ctx, b.RebuildView(), selection)
		require.NoError(t, err, "run %d census", run+1)
		_, err = RunRebuildEventChannel(ctx, b, receipts, frozen, 1)
		require.NoError(t, err, "run %d event channel", run+1)
		_, err = RunRebuildReactSetState(ctx, b, receipts, frozen, 1)
		require.NoError(t, err, "run %d React setState", run+1)
		_, err = RunRebuildFlutterSetState(ctx, b, receipts, frozen, 1)
		require.NoError(t, err, "run %d Flutter setState", run+1)

		require.NoError(t, RunFrameworkSynthesizersWithSelection(native, selection).Err)
		want := rebuildEventSQLiteRows(native)
		event := synthEventEdge(native, caller.ID, listener.ID)
		require.NotNil(t, event, "run %d must contain the event-generated setState Call", run+1)
		require.Equal(t, emitLine, event.Line)
		for _, e := range before.edges {
			require.Contains(t, want.edges, e)
		}
		if run == 0 {
			// Every input row and the event Call are present; no lifecycle hop fits.
			require.Len(t, want.edges, len(before.edges)+1)
		} else {
			require.Len(t, want.edges, len(before.edges)+3)
			renderHop := rebuildReactSetStateOutput(caller.ID, render.ID, owner.ID, file, caller.StartLine)
			buildHop := rebuildFlutterSetStateOutput(caller.ID, build.ID, owner.ID, file, caller.StartLine)
			require.Contains(t, want.edges, renderHop)
			require.Contains(t, want.edges, buildHop)
		}
		require.Equal(t, want, rebuildEventSavedRows(t, b), "run %d complete graph", run+1)
	}
}

func TestRebuildFrameworkCensusClosureCollectionAppleFamily(t *testing.T) {
	for _, tc := range []struct {
		name  string
		apple bool
	}{
		{name: "no apple declaration"},
		{name: "unrelated Swift declaration admits Go roles", apple: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			const file, language, field = "plugin.go", "go", "handlers"
			dispatch := rebuildClosureCollectionNode(file, "dispatch", language, rebuildCCDispatch, field)
			register := rebuildClosureCollectionNode(file, "register", language, rebuildCCAppend, field)
			facts := []store_pebble.ParseFacts{
				rebuildEventFacts(file, language, []*graph.Node{dispatch, register}),
			}
			if tc.apple {
				swift := rebuildEventNode("support.swift", "support", "swift", "")
				appleFacts := rebuildEventFacts(swift.FilePath, swift.Language, []*graph.Node{swift})
				facts = append(facts, appleFacts)
			}
			proof, b, native := openRebuildObserverStores(t, nil, facts...)
			selection := rebuildClosureCollectionSelection(t)
			ctx := t.Context()
			frozen, err := FreezeRebuildFrameworkSelection(ctx, b.RebuildView(), selection)
			require.NoError(t, err)
			_, err = RunRebuildClosureCollection(ctx, b, rebuildGRPCReceipts(proof...), frozen, 1)
			require.NoError(t, err)

			require.NoError(t, RunFrameworkSynthesizersWithSelection(native, selection).Err)
			want := rebuildEventSQLiteRows(native)
			if tc.apple {
				call := closureCollectionEdge(dispatch, register, field)
				require.Equal(t, []*graph.Edge{call}, want.edges)
			} else {
				require.Empty(t, want.edges)
			}
			require.Equal(t, want, rebuildEventSavedRows(t, b))
		})
	}
}

func TestRebuildFrameworkCensusObserverRolesShareTarget(t *testing.T) {
	const file, language = "observer.go", "go"
	registrar := rebuildEventNode(file, "subscribe", language, "")
	dispatcher := rebuildEventNode(file, "notify", language, "")
	first := rebuildEventNode(file, "first", language, "")
	second := rebuildEventNode(file, "second", language, "")
	registrar.Kind, dispatcher.Kind = graph.KindMethod, graph.KindMethod
	first.Kind, second.Kind = graph.KindField, graph.KindField
	b := openRebuildGRPCBuild(t, rebuildEventFacts(file, language, []*graph.Node{registrar, dispatcher, first, second},
		rebuildObserverEdge(registrar, first, graph.EdgeAccessesField, 3),
		rebuildObserverEdge(dispatcher, second, graph.EdgeAccessesField, 7),
		rebuildObserverEdge(dispatcher, first, graph.EdgeReads, 8),
	))
	selection, err := NewFrameworkSynthesizerSelection([]string{SynthObserverChannel})
	require.NoError(t, err)
	frozen, err := FreezeRebuildFrameworkSelection(t.Context(), b.RebuildView(), selection)
	require.NoError(t, err)
	require.False(t, frozen.allows(SynthObserverChannel))

	b.AddEdge(rebuildObserverEdge(dispatcher, first, graph.EdgeAccessesField, 9))
	require.False(t, frozen.allows(SynthObserverChannel), "new accesses must not change a completed census")
	next, err := FreezeRebuildFrameworkSelection(t.Context(), b.RebuildView(), selection)
	require.NoError(t, err)
	require.True(t, next.allows(SynthObserverChannel))
}
