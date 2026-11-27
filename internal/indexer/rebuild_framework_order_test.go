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
	"go.uber.org/zap"
	"go.uber.org/zap/zaptest/observer"
)

func TestRebuildBuildFrameworkOrder(t *testing.T) {
	// The private tail must follow native registry order. Work counters
	// distinguish applied passes from logs caused only by scratch spill.
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
					"go": {1}, "typescript": {1}, "swift": {1}, "objc": {1}, "java": {1},
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
	dispatch := node(swift.FilePath, "swift", "dispatchQueue", graph.KindMethod, 7)
	dispatch.Meta = map[string]any{"cc_dispatch_field": "queuedHandlers"}
	appendHandler := node(swift.FilePath, "swift", "appendHandler", graph.KindMethod, 9)
	appendHandler.Meta = map[string]any{"cc_append_field": "queuedHandlers"}
	java := node("Calendar.java", "java", method, graph.KindMethod, 5)
	java.Meta = map[string]any{"rn_module": module, "rn_method": method}

	const app, ts = "app.ts", "typescript"
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
	// Observer must accept the registrar call proved by event channel;
	// the original receipt has no call to subscribe.
	appFacts := receipt([]*graph.Node{setup, subscribe, notify, callback, field, topic, show},
		edge(setup, topic.ID, graph.EdgeEmits, 3, event),
		edge(subscribe, topic.ID, graph.EdgeListensOn, 7, event),
		edge(subscribe, field.ID, graph.EdgeAccessesField, 8, nil),
		edge(notify, field.ID, graph.EdgeAccessesField, 10, nil),
		edge(setup, callback.ID, graph.EdgeReferences, 3, nil),
		edge(show, "unresolved::rn::"+module+"::"+method, graph.EdgeCalls, 21,
			map[string]any{"via": "rn.native", "rn_module": module, "rn_method": method}),
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

	return []store_pebble.ParseFacts{
		receipt([]*graph.Node{objc}),
		receipt([]*graph.Node{swift, dispatch, appendHandler}),
		receipt([]*graph.Node{java}),
		appFacts, grpcFacts, widgetFacts, temporalFacts,
	}
}
