package indexer

import (
	"encoding/hex"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/zzet/gortex/internal/config"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/graph/store_pebble"
	"github.com/zzet/gortex/internal/resolver"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	"go.uber.org/zap/zaptest/observer"
)

func TestRebuildBuildFrameworkOrder(t *testing.T) {
	// The private tail must follow native registry order and report each pass's work.
	applied := []struct {
		pass, counter string
		count         int
	}{
		{"capabilities", "", 0},
		{"clone relations", "", 0},
		{"gRPC", "moves", 1},
		{"Temporal", "wrapper_stubs", 1},
		{"event channel", "pairs", 1},
		{"Swift/ObjC", "edges", 2},
		{"React Native bridge", "edges", 2},
		{"React Native native pairing", "edges", 2},
		{"Observer", "edges", 1},
		{"closure collection", "edges", 1},
		{"React setState", "edges", 1},
		{"Flutter setState", "edges", 1},
		{"KMP expect/actual", "edges", 2},
		{"Expo module bridge", "edges", 1},
		{"Fabric", "edges", 2},
		{"MyBatis", "moves", 1},
		{"SQL callsite", "moves", 1},
		{"Store Factory", "moves", 1},
	}
	for _, tc := range []struct {
		name  string
		exact bool
	}{
		{"exact_checkout", true},
		{"legacy_without_namespace", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := t.Context()
			root, src := rebuildInputFixture(t)
			lease := t.TempDir()
			cfg := config.Default().Index
			off := false
			cfg.Coverage.Modules.Enabled, cfg.Coverage.Clones.Enabled = &off, &off
			req := rebuildBuildRequest{
				Root:         root,
				ParseDir:     t.TempDir(),
				ParseLimit:   1 << 20,
				Incarnation:  rebuildTestIncarnation,
				BaseRecipe:   store_pebble.HeadDigest{1},
				ProvidersOff: true,
				Recipes: map[string]rebuildDigest{
					"go": {1}, "typescript": {1}, "swift": {1}, "objc": {1}, "java": {1}, "kotlin": {1}, "mybatis": {1},
					"sql": {1},
				},
				Policy:  freezeRebuildParsePolicy(root, cfg),
				Bounds:  rebuildTransformBounds{CaptureBytes: 1024, FileBytes: 1024},
				Witness: store_pebble.Witness{Stage: store_pebble.StageManifest, Profile: store_pebble.FullProfile},
			}
			idx := &Indexer{config: cfg}
			idx.installRebuildParsePolicy(req.Policy)
			store := rebuildParseStore{dir: req.ParseDir, limit: req.ParseLimit}
			var inputs []rebuildInput
			// Feed the same checked parse receipts to both checkout modes.
			// The files carry no framework facts if a cache lookup is missed.
			for _, facts := range rebuildFrameworkOrderReceipts() {
				path, lang := facts.File.FilePath, facts.EffectiveLanguage
				require.NoError(t, os.WriteFile(filepath.Join(root, path), []byte("// cached receipt fixture\n"), 0o600))
				input, _, err := readRebuildInput(ctx, src, path, lang, 1024)
				require.NoError(t, err)
				inputs = append(inputs, input)
				payload := encodeRebuildTestParseFacts(t, facts)
				key := input.parseKey(idx.rebuildExtractorRecipe(req.Recipes[lang], input))
				require.NoError(t, store.write(key, payload))
			}
			inputRoot, err := rebuildInputRoot(inputs)
			require.NoError(t, err)
			rootBytes, err := hex.DecodeString(inputRoot)
			require.NoError(t, err)
			req.Witness.Input = store_pebble.HeadDigest(rootBytes)
			if tc.exact {
				digest, err := writeRebuildInputSidecar(ctx, lease, func(emit func(rebuildSidecarRow) error) error {
					for _, in := range inputs {
						if err := emit(rebuildSidecarRow{Kind: rebuildNamespaceRegularFact, Path: []byte(in.Path)}); err != nil {
							return err
						}
					}
					return emit(rebuildSidecarRow{Kind: rebuildNamespaceCloseExactFact, Path: []byte(".")})
				})
				require.NoError(t, err)
				req.Witness.FrozenInputs = store_pebble.HeadDigest(digest)
			}
			req.Witness.Recipe = req.projectRecipe(cfg.Coverage.ClonesThreshold())
			require.NoError(t, writeRebuildBuildRequest(ctx, lease, req, cfg, inputs))
			core, logs := observer.New(zap.InfoLevel)
			var sent []store_pebble.Stage
			err = buildRebuildStages(ctx, lease, zap.New(core), store_pebble.OpenBuild, rebuildTestLinkOptions(t),
				func(tables store_pebble.StageTables) error {
					sent = append(sent, tables.Witness.Stage)
					return nil
				})
			require.ErrorIs(t, err, errRebuildBuildIncomplete)
			require.Equal(t, []store_pebble.Stage{store_pebble.StageManifest, store_pebble.StageFileGraph}, sent)

			const prefix, suffix = "rebuild ", " applied to private build"
			var entries []observer.LoggedEntry
			var got, want []string
			for _, entry := range logs.All() {
				if strings.HasPrefix(entry.Message, prefix) && strings.HasSuffix(entry.Message, suffix) {
					entries = append(entries, entry)
					got = append(got, entry.Message)
				}
			}
			expected := applied
			if !tc.exact {
				expected = applied[:1] // Legacy still runs capabilities, but no clone or framework turn.
			}
			for _, step := range expected {
				want = append(want, prefix+step.pass+suffix)
			}
			require.Equal(t, want, got)
			for i, step := range expected {
				if step.counter != "" {
					require.EqualValues(t, step.count, entries[i].ContextMap()[step.counter], "%s %s", step.pass, step.counter)
				}
			}
		})
	}
}

func TestRebuildBuildFrameworkCensusStaysFrozenAfterEvent(t *testing.T) {
	const (
		file         = "widget.ts"
		lang         = "typescript"
		owner        = file + "::Widget"
		caller       = owner + ".refresh"
		listener     = owner + ".setState"
		topic        = "event::pubsub::eventemitter::change"
		transport    = "eventemitter"
		emitLine     = 11
		cloneMessage = "rebuild clone relations applied to private build"
		eventMessage = "rebuild event channel applied to private build"
	)
	nodes := []*graph.Node{
		{ID: owner, Name: "Widget", Kind: graph.KindType, FilePath: file, Language: lang, StartLine: 1, EndLine: 50},
		{ID: topic, Name: "change", Kind: graph.KindEvent, FilePath: file, Language: lang,
			Meta: map[string]any{"transport": transport, "event_kind": "pubsub"}},
	}
	eventMeta := map[string]any{"transport": transport}
	edges := []*graph.Edge{
		{From: caller, To: topic, Kind: graph.EdgeEmits, FilePath: file, Line: emitLine, Meta: eventMeta},
		{From: listener, To: topic, Kind: graph.EdgeListensOn, FilePath: file, Line: 21, Meta: eventMeta},
	}
	for i, name := range []string{"refresh", "setState", "render", "build"} {
		id, line := owner+"."+name, (i+1)*10
		nodes = append(nodes, &graph.Node{ID: id, Name: name, Kind: graph.KindMethod, FilePath: file, Language: lang, StartLine: line, EndLine: line + 2})
		edges = append(edges, &graph.Edge{From: id, To: owner, Kind: graph.EdgeMemberOf, FilePath: file, Line: line})
	}
	facts := store_pebble.ParseFacts{
		EffectiveLanguage: lang, File: graph.FileMetaRow{FilePath: file, NodeCount: len(nodes)}, Nodes: nodes, Edges: edges,
	}

	ctx := t.Context()
	root, src := rebuildInputFixture(t)
	lease := t.TempDir()
	cfg := config.Default().Index
	off := false
	cfg.Coverage.Modules.Enabled, cfg.Coverage.Clones.Enabled = &off, &off
	passes := []string{resolver.SynthEventChannel, resolver.SynthReactSetState, resolver.SynthFlutterSetState}
	cfg.FrameworkSynthesizers = &passes
	wantLate, err := resolver.NewFrameworkSynthesizerSelection(passes)
	require.NoError(t, err)
	req := rebuildBuildRequest{
		Root: root, ParseDir: t.TempDir(), ParseLimit: 1 << 20, Incarnation: rebuildTestIncarnation,
		BaseRecipe: store_pebble.HeadDigest{1}, ProvidersOff: true, Recipes: map[string]rebuildDigest{lang: {1}},
		Policy: freezeRebuildParsePolicy(root, cfg), Bounds: rebuildTransformBounds{CaptureBytes: 1024, FileBytes: 1024},
		Witness: store_pebble.Witness{Stage: store_pebble.StageManifest, Profile: store_pebble.FullProfile},
	}
	// Only the checked cached receipt carries the framework facts.
	require.NoError(t, os.WriteFile(filepath.Join(root, file), []byte("// cached receipt fixture\n"), 0o600))
	input, _, err := readRebuildInput(ctx, src, file, lang, 1024)
	require.NoError(t, err)
	idx := &Indexer{config: cfg}
	idx.installRebuildParsePolicy(req.Policy)
	store := rebuildParseStore{dir: req.ParseDir, limit: req.ParseLimit}
	key := input.parseKey(idx.rebuildExtractorRecipe(req.Recipes[lang], input))
	require.NoError(t, store.write(key, encodeRebuildTestParseFacts(t, facts)))
	inputs := []rebuildInput{input}
	inputRoot, err := rebuildInputRoot(inputs)
	require.NoError(t, err)
	rootBytes, err := hex.DecodeString(inputRoot)
	require.NoError(t, err)
	req.Witness.Input = store_pebble.HeadDigest(rootBytes)
	digest, err := writeRebuildInputSidecar(ctx, lease, func(emit func(rebuildSidecarRow) error) error {
		if err := emit(rebuildSidecarRow{Kind: rebuildNamespaceRegularFact, Path: []byte(file)}); err != nil {
			return err
		}
		return emit(rebuildSidecarRow{Kind: rebuildNamespaceCloseExactFact, Path: []byte(".")})
	})
	require.NoError(t, err)
	req.Witness.FrozenInputs = store_pebble.HeadDigest(digest)
	req.Witness.Recipe = req.projectRecipe(cfg.Coverage.ClonesThreshold())
	require.NoError(t, writeRebuildBuildRequest(ctx, lease, req, cfg, inputs))

	var private *store_pebble.BuildStore
	openBuild := func(path string, profile store_pebble.Profile) (*store_pebble.BuildStore, error) {
		var err error
		private, err = store_pebble.OpenBuild(path, profile)
		return private, err
	}
	core, logs := observer.New(zap.InfoLevel)
	logger := zap.New(core, zap.Hooks(func(entry zapcore.Entry) error {
		if entry.Message != cloneMessage && entry.Message != eventMessage {
			return nil
		}
		view := private.RebuildView()
		var calls []*graph.Edge
		require.NoError(t, view.VisitEdges(ctx, func(e *graph.Edge) error {
			if e.Kind == graph.EdgeCalls {
				calls = append(calls, e)
			}
			return nil
		}))
		if entry.Message == cloneMessage {
			require.Empty(t, calls, "before the worker freezes the census")
			return nil
		}
		require.Equal(t, []*graph.Edge{{
			From:            caller,
			To:              listener,
			Kind:            graph.EdgeCalls,
			FilePath:        file,
			Line:            emitLine,
			Confidence:      0.5,
			ConfidenceLabel: "INFERRED",
			Origin:          graph.OriginASTInferred,
			Meta: map[string]any{
				"via":             "event.channel",
				"event_topic":     "change",
				"event_transport": transport,
				"synthesized_by":  resolver.SynthEventChannel,
				"provenance":      resolver.ProvenanceHeuristic,
			},
		}}, calls)
		// A fresh census here would admit both lifecycle passes. This read
		// does not change the selection the worker already froze.
		late, err := resolver.FreezeRebuildFrameworkSelection(ctx, view, wantLate)
		require.NoError(t, err)
		require.Equal(t, wantLate, late, "the Event Call must make both lifecycle passes eligible for a late census")
		return nil
	}))
	var sent []store_pebble.Stage
	err = buildRebuildStages(ctx, lease, logger, openBuild, rebuildTestLinkOptions(t), func(tables store_pebble.StageTables) error {
		sent = append(sent, tables.Witness.Stage)
		return nil
	})
	require.ErrorIs(t, err, errRebuildBuildIncomplete)
	require.Equal(t, []store_pebble.Stage{store_pebble.StageManifest, store_pebble.StageFileGraph}, sent)
	require.Equal(t, 1, logs.FilterMessage(cloneMessage).Len())
	event := logs.FilterMessage(eventMessage).All()
	require.Len(t, event, 1)
	require.EqualValues(t, 1, event[0].ContextMap()["pairs"])
	require.Empty(t, logs.FilterMessage("rebuild React setState applied to private build").All())
	require.Empty(t, logs.FilterMessage("rebuild Flutter setState applied to private build").All())
}

func TestRebuildSQLCallsiteFrozenGate(t *testing.T) {
	configured, err := resolver.NewFrameworkSynthesizerSelection([]string{resolver.SynthSQLCallsite})
	require.NoError(t, err)
	disabled, err := resolver.NewFrameworkSynthesizerSelection([]string{})
	require.NoError(t, err)
	for _, tc := range []struct {
		name     string
		late     string
		sqlKind  graph.NodeKind
		selected bool
		wantMove bool
	}{
		{name: "python alone satisfies the caller group", sqlKind: graph.KindFunction, selected: true, wantMove: true},
		{name: "SQL absent at freeze", late: "sql", sqlKind: graph.KindFunction, selected: true},
		{name: "caller group absent at freeze", late: "python", sqlKind: graph.KindFunction, selected: true},
		{name: "disabled", sqlKind: graph.KindFunction},
		{name: "SQL language but no function", sqlKind: graph.KindType, selected: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := t.Context()
			b, err := store_pebble.OpenBuild(t.TempDir(), store_pebble.FullProfile)
			require.NoError(t, err)
			t.Cleanup(func() { require.NoError(t, b.Close()) })
			const name = "load_calendar"
			sql := &graph.Node{ID: "calendar.sql::" + name, FilePath: "calendar.sql", Language: "sql", Kind: tc.sqlKind, Name: name}
			caller := &graph.Node{ID: "calendar.py::load", FilePath: "calendar.py", Language: "python", Kind: graph.KindFunction, Name: "load"}
			call := &graph.Edge{From: caller.ID, To: "unresolved::sqlfn::" + name, FilePath: caller.FilePath, Kind: graph.EdgeCalls, Line: 2,
				Meta: map[string]any{"via": "sql.callsite", "sql_function": name}}
			facts := []store_pebble.ParseFacts{
				{EffectiveLanguage: "sql", File: graph.FileMetaRow{FilePath: sql.FilePath, NodeCount: 1}, Nodes: []*graph.Node{sql}},
				{EffectiveLanguage: "python", File: graph.FileMetaRow{FilePath: caller.FilePath, NodeCount: 1}, Nodes: []*graph.Node{caller}, Edges: []*graph.Edge{call}},
			}
			for _, f := range facts {
				if f.EffectiveLanguage != tc.late {
					require.NoError(t, b.AddParsedFile("repo", f))
				}
			}
			selection := disabled
			if tc.selected {
				selection = configured
			}
			frozen, err := resolver.FreezeRebuildFrameworkSelection(ctx, b.RebuildView(), selection)
			require.NoError(t, err)
			if !tc.selected || tc.late != "" {
				require.Equal(t, disabled, frozen)
			} else {
				require.Equal(t, configured, frozen)
			}
			if tc.late != "" {
				for _, f := range facts {
					if f.EffectiveLanguage == tc.late {
						require.NoError(t, b.AddParsedFile("repo", f))
					}
				}
				late, err := resolver.FreezeRebuildFrameworkSelection(ctx, b.RebuildView(), selection)
				require.NoError(t, err)
				require.Equal(t, configured, late, "a live recheck would admit SQL callsites")
			}
			readCalls := func() []*graph.Edge {
				var calls []*graph.Edge
				require.NoError(t, b.RebuildView().VisitEdges(ctx, func(e *graph.Edge) error {
					calls = append(calls, e)
					return nil
				}))
				return calls
			}
			before := readCalls()
			require.Len(t, before, 1)
			require.Equal(t, call.To, before[0].To)
			stats, err := resolver.RunRebuildSQLCallsites(ctx, b, func(yield func(store_pebble.ParseFacts, error) bool) {
				require.True(t, tc.wantMove, "unchanged SQL callsites must not replay receipts")
				for _, f := range facts {
					if !yield(f, nil) {
						return
					}
				}
			}, frozen)
			require.NoError(t, err)
			after := readCalls()
			if tc.wantMove {
				require.Equal(t, store_pebble.RebuildSQLCallsiteStats{Stubs: 1, Resolved: 1, Moves: 1}, stats)
				require.Len(t, after, 1)
				require.Equal(t, sql.ID, after[0].To)
				require.Equal(t, resolver.SynthSQLCallsite, after[0].Meta[resolver.MetaSynthesizedBy])
			} else {
				require.Zero(t, stats)
				require.Equal(t, before, after)
			}
		})
	}
}

func rebuildFrameworkOrderReceipts() []store_pebble.ParseFacts {
	node := func(file, lang, name string, kind graph.NodeKind, line int) *graph.Node {
		return &graph.Node{
			ID:        file + "::" + name,
			Name:      name,
			Kind:      kind,
			FilePath:  file,
			Language:  lang,
			StartLine: line,
			EndLine:   line,
		}
	}
	edge := func(from *graph.Node, to string, kind graph.EdgeKind, line int, meta map[string]any) *graph.Edge {
		return &graph.Edge{From: from.ID, To: to, Kind: kind, FilePath: from.FilePath, Line: line, Meta: meta}
	}
	receipt := func(nodes []*graph.Node, edges ...*graph.Edge) store_pebble.ParseFacts {
		return store_pebble.ParseFacts{
			EffectiveLanguage: nodes[0].Language,
			File:              graph.FileMetaRow{FilePath: nodes[0].FilePath, NodeCount: len(nodes)},
			Nodes:             nodes,
			Edges:             edges,
		}
	}

	const module, method = "Calendar", "open"
	objc := node("Bridge.m", "objc", "deliverTo:", graph.KindMethod, 3)
	objc.Meta = map[string]any{"rn_module": module, "rn_method": method}
	swift := node("Bridge.swift", "swift", "custom", graph.KindMethod, 3)
	swift.Meta = map[string]any{"objc_selector": "deliverTo:"}
	expo := node(swift.FilePath, swift.Language, "openClock", graph.KindMethod, 5)
	expo.Meta = map[string]any{"expo_module": module, "expo_method": method}
	dispatch := node(swift.FilePath, "swift", "dispatchQueue", graph.KindMethod, 7)
	dispatch.Meta = map[string]any{"cc_dispatch_field": "queuedHandlers"}
	appendHandler := node(swift.FilePath, "swift", "appendHandler", graph.KindMethod, 9)
	appendHandler.Meta = map[string]any{"cc_append_field": "queuedHandlers"}
	java := node("Calendar.java", "java", method, graph.KindMethod, 5)
	java.Meta = map[string]any{"rn_module": module, "rn_method": method, "receiver": module}
	mapper := node("Calendar.xml", "mybatis", method, graph.KindMethod, 2)
	const namespace = "demo.Calendar"
	mapper.ID = namespace + "::" + method
	mapperFacts := receipt([]*graph.Node{mapper},
		edge(mapper, "unresolved::mybatis::"+mapper.ID, graph.EdgeCalls, 2,
			map[string]any{"via": "mybatis.mapper", "mybatis_namespace": namespace, "mybatis_statement": method}),
	)
	manager := node(java.FilePath, java.Language, "RCTClockView", graph.KindType, 8)
	manager.Meta = map[string]any{"fabric_component": "RCTClockView", "fabric_native": "java"}

	const app, ts = "app.ts", "typescript"
	const sqlFunction = "load_calendar"
	sql := node("calendar.sql", "sql", sqlFunction, graph.KindFunction, 1)
	const storeBinding, storeAction = "useCalendarStore", "reloadCalendar"
	action := node("calendarStore.ts", ts, "calendarDispatch", graph.KindFunction, 1)
	action.Meta = map[string]any{"store_factory": storeBinding, "store_member": storeAction}
	setup := node(app, ts, "setup", graph.KindFunction, 2)
	subscribe := node(app, ts, "subscribe", graph.KindMethod, 6)
	notify := node(app, ts, "notify", graph.KindMethod, 9)
	callback := node(app, ts, "callback", graph.KindFunction, 13)
	field := node(app, ts, "handlers", graph.KindField, 16)
	topic := node(app, ts, "orders", graph.KindEvent, 17)
	topic.ID = "event::pubsub::eventemitter::orders"
	topic.Meta = map[string]any{"transport": "eventemitter", "event_kind": "pubsub"}
	event := map[string]any{"transport": "eventemitter"}
	show := node(app, ts, "show", graph.KindFunction, 20)
	spec := node(app, ts, "ClockView", graph.KindType, 23)
	spec.Meta = map[string]any{"fabric_component": "ClockView"}
	// Observer must accept the registrar call proved by event channel;
	// the original receipt has no call to subscribe.
	appFacts := receipt([]*graph.Node{setup, subscribe, notify, callback, field, topic, show, spec},
		edge(setup, topic.ID, graph.EdgeEmits, 3, event),
		edge(subscribe, topic.ID, graph.EdgeListensOn, 7, event),
		edge(subscribe, field.ID, graph.EdgeAccessesField, 8, nil),
		edge(notify, field.ID, graph.EdgeAccessesField, 10, nil),
		edge(setup, callback.ID, graph.EdgeReferences, 3, nil),
		edge(show, "unresolved::rn::"+module+"::"+method, graph.EdgeCalls, 21,
			map[string]any{"via": "rn.native", "rn_module": module, "rn_method": method}),
		edge(show, "unresolved::sqlfn::"+sqlFunction, graph.EdgeCalls, 22,
			map[string]any{"via": "sql.callsite", "sql_function": sqlFunction}),
		edge(show, "unresolved::*."+storeAction, graph.EdgeCalls, 25,
			map[string]any{"via": "store-factory", "store_binding": storeBinding, "store_action": storeAction}),
	)

	const rpc = "rpc.go"
	client := node(rpc, "go", "callGreeter", graph.KindFunction, 2)
	register := node(rpc, "go", "registerGreeter", graph.KindFunction, 5)
	registration := node(rpc, "go", "RegisterGreeterServer", graph.KindFunction, 9)
	server := node(rpc, "go", "greeter", graph.KindType, 11)
	hello := node(rpc, "go", "Hello", graph.KindMethod, 13)
	hello.ID = server.ID + ".Hello"
	grpcFacts := receipt([]*graph.Node{client, register, registration, server, hello},
		edge(client, "unresolved::grpc::Greeter::Hello", graph.EdgeCalls, 3,
			map[string]any{"via": "grpc.stub", "grpc_service": "Greeter", "grpc_method": "Hello"}),
		edge(register, registration.ID, graph.EdgeCalls, 6,
			map[string]any{"grpc_register_service": "Greeter", "grpc_register_impl": server.Name}),
		edge(hello, server.ID, graph.EdgeMemberOf, 13, nil),
	)

	const widgetFile = "widget.go"
	widget := node(widgetFile, "go", "Widget", graph.KindType, 2)
	render := node(widgetFile, "go", "render", graph.KindMethod, 4)
	build := node(widgetFile, "go", "build", graph.KindMethod, 6)
	setState := node(widgetFile, "go", "setState", graph.KindMethod, 8)
	refresh := node(widgetFile, "go", "refresh", graph.KindMethod, 10)
	for _, m := range []*graph.Node{render, build, setState, refresh} {
		m.ID = widget.ID + "." + m.Name
	}
	widgetFacts := receipt([]*graph.Node{widget, render, build, setState, refresh},
		edge(render, widget.ID, graph.EdgeMemberOf, 4, nil),
		edge(build, widget.ID, graph.EdgeMemberOf, 6, nil),
		edge(setState, widget.ID, graph.EdgeMemberOf, 8, nil),
		edge(refresh, widget.ID, graph.EdgeMemberOf, 10, nil),
		edge(refresh, "unresolved::setState", graph.EdgeCalls, 11, nil),
	)

	const workflow = "workflow.go"
	wrapper := node(workflow, "go", "RunActivity", graph.KindFunction, 2)
	param := node(workflow, "go", "activityName", graph.KindParam, 2)
	param.ID = wrapper.ID + "#param:" + param.Name
	param.Meta = map[string]any{"position": 0}
	start := node(workflow, "go", "runWorkflow", graph.KindFunction, 5)
	// Main moves the unique bare call. Temporal proves the wrapper trigger
	// from its original receipt identity, not the installed target.
	temporalFacts := receipt([]*graph.Node{wrapper, param, start},
		edge(wrapper, "unresolved::temporal::activity::activityName", graph.EdgeCalls, 3, map[string]any{
			"via":                 "temporal.stub",
			"temporal_kind":       "activity",
			"temporal_name":       param.Name,
			"temporal_name_param": param.Name,
		}),
		edge(start, "unresolved::RunActivity", graph.EdgeCalls, 6, map[string]any{"arg_names": []string{"NotifyActivity"}}),
	)

	expect := node("common.kt", "kotlin", "platformClock", graph.KindFunction, 2)
	expect.Meta = map[string]any{"kmp_role": "expect"}
	actual := node("platform.kt", "kotlin", expect.Name, graph.KindFunction, 2)
	actual.Meta = map[string]any{"kmp_role": "actual"}

	return []store_pebble.ParseFacts{
		receipt([]*graph.Node{objc}),
		receipt([]*graph.Node{swift, dispatch, appendHandler, expo}),
		receipt([]*graph.Node{java, manager}),
		mapperFacts,
		appFacts, grpcFacts, widgetFacts, temporalFacts,
		receipt([]*graph.Node{expect}),
		receipt([]*graph.Node{actual}),
		receipt([]*graph.Node{sql}),
		receipt([]*graph.Node{action}),
	}
}
