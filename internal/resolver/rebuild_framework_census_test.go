package resolver

import (
	"fmt"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/graph/store_pebble"
)

func TestRebuildFrameworkCensusOversizedEdge(t *testing.T) {
	const file, language = "gate.ts", "typescript"
	caller := rebuildEventNode(file, "caller", language, "")
	render := rebuildEventNode(file, "render", language, "")
	render.Kind = graph.KindMethod
	build := rebuildEventNode(file, "build", language, "")
	build.Kind = graph.KindMethod
	large := strings.Repeat("x", 16<<20)
	for _, tc := range []struct {
		name, stage, to, via string
		kind                 graph.EdgeKind
		allowed              bool
	}{
		{"ordinary Call", SynthGRPCStub, "unresolved::read", "", graph.EdgeCalls, false},
		{"unselected stage", SynthGRPCStub, "unresolved::read", rtkQueryVia, graph.EdgeCalls, false},
		{"gRPC", SynthGRPCStub, "unresolved::read", "grpc.stub", graph.EdgeCalls, true},
		{"Temporal Call", SynthTemporalStub, "unresolved::read", "temporal.await", graph.EdgeCalls, true},
		{"Temporal annotation", SynthTemporalStub, "annotation::java::WorkflowMethod", "", graph.EdgeAnnotated, true},
		{"other annotation", SynthTemporalStub, "annotation::java::Deprecated", "", graph.EdgeAnnotated, false},
		{"React setState", SynthReactSetState, "unresolved::setState", "", graph.EdgeCalls, true},
		{"Flutter setState", SynthFlutterSetState, "unresolved::setState", "", graph.EdgeCalls, true},
		{"Redux", SynthReduxThunk, "unresolved::read", reduxThunkVia, graph.EdgeCalls, true},
		{"Object Registry", SynthObjectRegistry, "unresolved::read", objectRegistryVia, graph.EdgeCalls, true},
		{"RTK Query", SynthRTKQuery, "unresolved::read", rtkQueryVia, graph.EdgeCalls, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			selection, err := NewFrameworkSynthesizerSelection([]string{tc.stage})
			require.NoError(t, err)
			b := openRebuildGRPCBuild(t, rebuildEventFacts(file, language, []*graph.Node{caller, render, build}))
			e := graph.Edge{From: caller.ID, To: tc.to, Kind: tc.kind, FilePath: file, Line: 2,
				Meta: map[string]any{"via": tc.via, "grpc_service": "Orders", "grpc_method": "Read", "registry_value": "Command"}}
			b.AddEdge(&e)
			frozen, err := FreezeRebuildFrameworkSelection(t.Context(), b.RebuildView(), selection)
			require.NoError(t, err)
			require.Equal(t, tc.allowed, frozen.allows(tc.stage), "small-row control")
			e.Context = large
			b.AddEdge(&e)
			frozen, err = FreezeRebuildFrameworkSelection(t.Context(), b.RebuildView(), selection)
			require.NoError(t, err)
			require.Equal(t, tc.allowed, frozen.allows(tc.stage), "oversized row must not change admission")
		})
	}
}

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
		_, err = RunRebuildEventChannel(ctx, b, receipts, frozen)
		require.NoError(t, err, "run %d event channel", run+1)
		_, err = RunRebuildReactSetState(ctx, b, receipts, frozen)
		require.NoError(t, err, "run %d React setState", run+1)
		_, err = RunRebuildFlutterSetState(ctx, b, receipts, frozen)
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
			_, err = RunRebuildClosureCollection(ctx, b, rebuildGRPCReceipts(proof...), frozen)
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

func TestRebuildFrameworkCensusContractOverlay(t *testing.T) {
	const file, language = "observer.go", "go"
	registrar := rebuildEventNode(file, "subscribe", language, "")
	dispatcher := rebuildEventNode(file, "notify", language, "")
	field := rebuildEventNode(file, "handlers", language, "")
	registrar.Kind, dispatcher.Kind, field.Kind = graph.KindMethod, graph.KindMethod, graph.KindField
	b := openRebuildGRPCBuild(t, rebuildEventFacts(file, language, []*graph.Node{registrar, dispatcher, field},
		rebuildObserverEdge(registrar, field, graph.EdgeAccessesField, 3),
		rebuildObserverEdge(dispatcher, field, graph.EdgeAccessesField, 7),
	))
	ctx := t.Context()
	selection, err := NewFrameworkSynthesizerSelection([]string{SynthObserverChannel})
	require.NoError(t, err)
	before, err := FreezeRebuildFrameworkSelection(ctx, b.RebuildView(), selection)
	require.NoError(t, err)
	require.True(t, before.allows(SynthObserverChannel))

	require.NoError(t, b.BeginRebuildContractOverlay(ctx, map[string]string{dispatcher.ID: "settings.rb"}))
	// The overlaid dispatcher no longer contributes an Observer role.
	during, err := FreezeRebuildFrameworkSelection(ctx, b.RebuildView(), selection)
	require.NoError(t, err)
	require.False(t, during.allows(SynthObserverChannel))
	require.NoError(t, b.EndRebuildContractOverlay(ctx))
}

func TestRebuildFrameworkCensusTemporalJavaAnnotation(t *testing.T) {
	selection, err := NewFrameworkSynthesizerSelection([]string{SynthTemporalStub})
	require.NoError(t, err)
	for _, tc := range []struct {
		annotation string
		want       bool
	}{
		{annotation: "WorkflowMethod", want: true},
		{annotation: "Deprecated", want: false},
	} {
		t.Run(tc.annotation, func(t *testing.T) {
			const file, language = "Workflow.java", "java"
			method := rebuildEventNode(file, "run", language, "")
			method.Kind = graph.KindMethod
			annotation := &graph.Node{
				ID: "annotation::java::" + tc.annotation, Name: tc.annotation,
				Kind: graph.KindType, FilePath: file, Language: language,
				Meta: map[string]any{"kind": "annotation", "synthetic": true},
			}
			edge := &graph.Edge{
				From:     method.ID,
				To:       annotation.ID,
				Kind:     graph.EdgeAnnotated,
				FilePath: file,
				Line:     2,
				Origin:   graph.OriginASTResolved,
			}
			b, native := openRebuildEventStores(t, rebuildEventFacts(file, language, []*graph.Node{method, annotation}, edge))
			saved := rebuildEventSavedRows(t, b)
			require.Len(t, saved.nodes, 2)
			require.Equal(t, []*graph.Edge{edge}, saved.edges, "annotation is the only edge; there is no Temporal call")

			frozen, err := FreezeRebuildFrameworkSelection(t.Context(), b.RebuildView(), selection)
			require.NoError(t, err)
			require.Equal(t, tc.want, frozen.allows(SynthTemporalStub), "rebuild census")
			summary := summarizeFrameworkCandidates(native, nil)
			require.Equal(t, tc.want, shouldRunFrameworkSynthesizer(synthFunc{name: SynthTemporalStub}, nil, summary), "native census")
		})
	}
}

func TestRebuildFrameworkCensusObserverRoleOverflow(t *testing.T) {
	selection := rebuildObserverSelection(t)
	for _, tc := range []struct {
		name  string
		roles int
		want  bool
	}{
		{name: "4096 roles", roles: 4096, want: false},
		{name: "4097 roles", roles: 4097, want: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			const file, language = "observer.go", "go"
			first := rebuildEventNode(file, "first", language, "")
			second := rebuildEventNode(file, "second", language, "")
			registrar := rebuildEventNode(file, "subscribe", language, "")
			dispatcher := rebuildEventNode(file, "notify", language, "")
			first.Kind, second.Kind = graph.KindField, graph.KindField
			registrar.Kind, dispatcher.Kind = graph.KindMethod, graph.KindMethod
			nodes := []*graph.Node{first, second, registrar, dispatcher}
			// Reuse the method name on distinct receivers so the limit counts IDs, not names.
			for i := range tc.roles - 2 {
				method := rebuildEventNode(file, fmt.Sprintf("Subscriber%d.subscribe", i), language, "")
				method.Name, method.Kind = "subscribe", graph.KindMethod
				nodes = append(nodes, method)
			}
			edges := []*graph.Edge{
				rebuildObserverEdge(registrar, first, graph.EdgeAccessesField, 3),
				rebuildObserverEdge(dispatcher, second, graph.EdgeAccessesField, 7),
			}
			b, native := openRebuildEventStores(t, rebuildEventFacts(file, language, nodes, edges...))
			saved := rebuildEventSavedRows(t, b)
			require.Len(t, saved.nodes, tc.roles+2)
			require.ElementsMatch(t, edges, saved.edges, "the two accesses must target different fields")

			frozen, err := FreezeRebuildFrameworkSelection(t.Context(), b.RebuildView(), selection)
			require.NoError(t, err)
			require.Equal(t, tc.want, frozen.allows(SynthObserverChannel), "rebuild census")
			summary := summarizeFrameworkCandidates(native, nil)
			require.Equal(t, tc.want, shouldRunFrameworkSynthesizer(synthFunc{name: SynthObserverChannel}, nil, summary), "native census")
		})
	}
}

func TestRebuildFrameworkCensusMacroOnlySelection(t *testing.T) {
	const file = "source.c"
	large := strings.Repeat("x", 16<<20)
	selection, err := NewFrameworkSynthesizerSelection([]string{SynthMacroExpansion})
	require.NoError(t, err)
	for _, tc := range []struct {
		name           string
		sourceLanguage string
		largeKind      graph.NodeKind
		includeMacro   bool
		admitted       bool
	}{
		{"large nonmacro", "c", graph.KindFunction, true, true},
		{"no macro", "c", graph.KindFunction, false, false},
		{"no C family", "go", graph.KindMacro, false, false},
		{"unused large macro", "c", graph.KindMacro, false, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			wide := rebuildEventNode(file, "wide", tc.sourceLanguage, "")
			wide.Kind = tc.largeKind
			wide.Meta = map[string]any{"payload": large, "macro_kind": macroFunctionKindMeta}
			nodes := []*graph.Node{wide}
			if tc.includeMacro {
				macro := rebuildEventNode(file, "small", tc.sourceLanguage, "")
				macro.Kind = graph.KindMacro
				macro.Meta = map[string]any{"macro_kind": macroFunctionKindMeta}
				nodes = append(nodes, macro)
			}
			facts := rebuildEventFacts(file, tc.sourceLanguage, nodes)
			b := openRebuildGRPCBuild(t, facts)
			frozen, err := FreezeRebuildFrameworkSelection(t.Context(), b.RebuildView(), selection)
			require.NoError(t, err)
			require.Equal(t, tc.admitted, frozen.allows(SynthMacroExpansion))
			receipts := rebuildGRPCReceipts(facts)
			require.NoError(t, FreezeRebuildMacroExpansionInputs(t.Context(), b, receipts, frozen))
			stats, err := RunRebuildMacroExpansion(t.Context(), b, receipts, frozen)
			require.NoError(t, err)
			require.Zero(t, stats)
		})
	}
}

func TestRebuildFrameworkCensusAllowsUnrelatedOversizedNode(t *testing.T) {
	const file, lang = "bus.ts", "typescript"
	const topic, transport = "event::pubsub::eventemitter::tick", "eventemitter"
	emitter := rebuildEventNode(file, "emit", lang, "")
	listener := rebuildEventNode(file, "listen", lang, "")
	unrelated := rebuildEventNode(file, "unrelated", lang, "")
	unrelated.Meta = map[string]any{"padding": strings.Repeat("x", 16<<20)}
	facts := rebuildEventFacts(file, lang, []*graph.Node{
		emitter, listener, rebuildEventTopic(file, lang, topic, "tick", transport),
	}, rebuildEventSite(emitter, topic, graph.EdgeEmits, 4, transport),
		rebuildEventSite(listener, topic, graph.EdgeListensOn, 8, transport))
	build, native := openRebuildEventStores(t, facts)
	build.AddNode(unrelated)
	selected, err := NewFrameworkSynthesizerSelection([]string{SynthEventChannel})
	require.NoError(t, err)
	frozen, err := FreezeRebuildFrameworkSelection(t.Context(), build.RebuildView(), selected)
	require.NoError(t, err)
	require.True(t, frozen.allows(SynthEventChannel))
	stats, err := RunRebuildEventChannel(t.Context(), build, rebuildGRPCReceipts(facts), frozen)
	require.NoError(t, err)
	require.EqualValues(t, 1, stats.Pairs)
	require.Equal(t, 1, ResolveEventChannelCalls(native))
	require.Equal(t, rebuildEventSQLiteRows(native).edges, rebuildEventSavedRows(t, build).edges)
}
