package resolver

import (
	"cmp"
	"maps"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/graph/store_pebble"
	"github.com/zzet/gortex/internal/graph/store_sqlite"
)

func TestRebuildEventChannelSQLiteOracle(t *testing.T) {
	const (
		file       = "a.ts"
		otherFile  = "z.ts"
		nativeFile = "native.swift"
		stream     = "event::pubsub::eventemitter::orders"
		bridge     = "event::pubsub::rn_native_event::battery"
	)
	publish := rebuildEventNode(file, "publish", "typescript", "work")
	handle := rebuildEventNode(file, "handle", "typescript", "work")
	stationary := rebuildEventNode(file, "stationary", "typescript", "work")
	foreign := rebuildEventNode(file, "foreign", "typescript", "elsewhere")
	send := rebuildEventNode(nativeFile, "send", "swift", "work")
	lower := rebuildEventSite(publish, stream, graph.EdgeEmits, 4, "eventemitter")
	late := rebuildEventSite(publish, stream, graph.EdgeEmits, 11, "eventemitter")
	tied := *lower
	tied.FilePath = otherFile
	existing := &graph.Edge{
		From: publish.ID, To: stationary.ID, Kind: graph.EdgeCalls, FilePath: file, Line: 4,
		Confidence: 0.9, Origin: graph.OriginASTInferred, Meta: map[string]any{"fixture": "stationary"},
	}
	facts := []store_pebble.ParseFacts{
		rebuildEventFacts(file, "typescript", []*graph.Node{publish, handle, stationary, foreign,
			rebuildEventTopic(file, "typescript", stream, "orders", "eventemitter"),
		}, late, lower, existing,
			rebuildEventSite(handle, stream, graph.EdgeListensOn, 15, "eventemitter"),
			rebuildEventSite(stationary, stream, graph.EdgeListensOn, 16, "eventemitter"),
			rebuildEventSite(publish, stream, graph.EdgeListensOn, 17, "eventemitter"),
			rebuildEventSite(foreign, stream, graph.EdgeListensOn, 18, "eventemitter"),
			rebuildEventSite(handle, bridge, graph.EdgeListensOn, 19, "rn_native_event")),
		rebuildEventFacts(nativeFile, "swift", []*graph.Node{send,
			rebuildEventTopic(nativeFile, "swift", bridge, "battery", "rn_native_event"),
		}, rebuildEventSite(send, bridge, graph.EdgeEmits, 6, "rn_native_event")),
		// Complete keys order the two same-line sites by recording path.
		// Seed SQLite in that order for this tie, not only the comparator.
		rebuildEventFacts(otherFile, "typescript", nil, &tied),
	}
	b, native := openRebuildEventStores(t, facts...)
	selection, err := NewFrameworkSynthesizerSelection([]string{SynthEventChannel})
	require.NoError(t, err)

	before := rebuildEventSavedRows(t, b)
	require.Equal(t, rebuildEventSQLiteRows(native), before)
	disabled, err := NewFrameworkSynthesizerSelection(nil)
	require.NoError(t, err)
	stats, err := RunRebuildEventChannel(t.Context(), b, rebuildGRPCReceipts(facts...), disabled, 1)
	require.NoError(t, err)
	require.Zero(t, stats)
	require.Equal(t, before, rebuildEventSavedRows(t, b))

	for range 2 {
		ResolveEventChannelCalls(native)
		stats, err := RunRebuildEventChannel(t.Context(), b, rebuildGRPCReceipts(facts...), selection, 1)
		require.NoError(t, err)
		require.Positive(t, stats.SpillBytes)
		require.Equal(t, rebuildEventSQLiteRows(native), rebuildEventSavedRows(t, b))
		calls := slices.Collect(native.EdgesByKind(graph.EdgeCalls))
		require.Len(t, calls, 3)
		order := synthEventEdge(native, publish.ID, handle.ID)
		require.NotNil(t, order)
		require.Equal(t, 4, order.Line)
		require.Equal(t, file, order.FilePath)
		require.Equal(t, "orders", order.Meta["event_topic"])
		rn := synthEventEdge(native, send.ID, handle.ID)
		require.NotNil(t, rn)
		require.Equal(t, "rn_native_event", rn.Meta["event_transport"])
		i := slices.IndexFunc(calls, func(e *graph.Edge) bool { return e.To == stationary.ID })
		require.NotEqual(t, -1, i)
		require.Equal(t, existing, calls[i])
	}
}

func TestRebuildEventChannelTopicCollision(t *testing.T) {
	const file, language = "topics.ts", "typescript"
	const first, last = "event::emitter::bus::a", "event::emitter::bus::b"
	emitter := rebuildEventNode(file, "publish", language, "")
	listener := rebuildEventNode(file, "listen", language, "")
	facts := rebuildEventFacts(file, language, []*graph.Node{
		emitter, listener,
		rebuildEventTopic(file, language, first, "a", "emitter"),
		rebuildEventTopic(file, language, last, "b", "emitter"),
	}, rebuildEventSite(emitter, last, graph.EdgeEmits, 2, "emitter"),
		rebuildEventSite(emitter, first, graph.EdgeEmits, 2, "emitter"),
		rebuildEventSite(listener, first, graph.EdgeListensOn, 4, "emitter"),
		rebuildEventSite(listener, last, graph.EdgeListensOn, 5, "emitter"))
	b, native := openRebuildEventStores(t, facts)
	selection, err := NewFrameworkSynthesizerSelection([]string{SynthEventChannel})
	require.NoError(t, err)
	// Exercise the native batch adapter. Its last staged payload wins
	// across topics; SQLite still preserves an existing durable key.
	require.NoError(t, RunFrameworkSynthesizersWithSelection(native, selection).Err)
	call := synthEventEdge(native, emitter.ID, listener.ID)
	require.NotNil(t, call)
	require.Equal(t, "bus::b", call.Meta["event_topic"])
	stats, err := RunRebuildEventChannel(t.Context(), b, rebuildGRPCReceipts(facts), selection, 1)
	require.NoError(t, err)
	require.Equal(t, uint64(1), stats.Pairs)
	require.Equal(t, rebuildEventSQLiteRows(native), rebuildEventSavedRows(t, b))
}

func TestRebuildEventChannelFirstNonemptyTransport(t *testing.T) {
	const file, language = "transport.ts", "typescript"
	// The parser normally puts the same transport in both the topic ID and
	// edge metadata. Deliberately vary the saved metadata to exercise native
	// precedence, with distinct site keys so neither store drops a row.
	topic := "event::pubsub::kafka::" + strings.Repeat("x", 1<<20)
	emitter := rebuildEventNode(file, "emit", language, "")
	listener := rebuildEventNode(file, "listen", language, "")
	facts := rebuildEventFacts(file, language, []*graph.Node{
		emitter, listener, rebuildEventTopic(file, language, topic, "tick", "kafka"),
	}, rebuildEventSite(emitter, topic, graph.EdgeEmits, 2, ""),
		rebuildEventSite(emitter, topic, graph.EdgeEmits, 4, "socketio"),
		rebuildEventSite(emitter, topic, graph.EdgeEmits, 6, "kafka"),
		rebuildEventSite(listener, topic, graph.EdgeListensOn, 8, "kafka"))
	b, native := openRebuildEventStores(t, facts)
	require.Equal(t, 1, ResolveEventChannelCalls(native))
	call := synthEventEdge(native, emitter.ID, listener.ID)
	require.NotNil(t, call)
	require.Equal(t, 2, call.Line)
	require.Equal(t, "socketio", call.Meta["event_transport"])

	stats, err := RunRebuildEventChannel(t.Context(), b, rebuildGRPCReceipts(facts), AllFrameworkSynthesizers(), 1<<20)
	require.NoError(t, err)
	require.Equal(t, uint64(1), stats.Pairs)
	require.Positive(t, stats.SpillBytes)
	require.Equal(t, rebuildEventSQLiteRows(native), rebuildEventSavedRows(t, b))
}

func TestRebuildEventChannelRawSiteCaps(t *testing.T) {
	for _, tc := range []struct {
		name, topic, transport string
		emits, listens, pairs  int
	}{
		{"emitter at cap", "event::emitter::bus::tick", "emitter", 6, 1, 1},
		{"emitter over cap", "event::emitter::bus::tick", "emitter", 7, 1, 0},
		{"emitter literal listeners at cap", "event::emitter::bus::tick", "emitter", 1, 6, 1},
		{"emitter literal listeners over cap", "event::emitter::bus::tick", "emitter", 1, 7, 0},
		{"listener at cap", "event::pubsub::socketio::tick", "socketio", 1, 32, 1},
		{"listener over cap", "event::pubsub::socketio::tick", "socketio", 1, 33, 0},
		{"socketio emitters at cap", "event::pubsub::socketio::tick", "socketio", 32, 1, 1},
		{"socketio emitters over cap", "event::pubsub::socketio::tick", "socketio", 33, 1, 0},
		{"broker skipped", "event::pubsub::kafka::tick", "kafka", 1, 1, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			const file, language = "bus.ts", "typescript"
			emitter := rebuildEventNode(file, "send", language, "")
			listener := rebuildEventNode(file, "onTick", language, "")
			var sites []*graph.Edge
			for i := range tc.emits {
				sites = append(sites, rebuildEventSite(emitter, tc.topic, graph.EdgeEmits, i+1, tc.transport))
			}
			for i := range tc.listens {
				sites = append(sites, rebuildEventSite(listener, tc.topic, graph.EdgeListensOn, i+20, tc.transport))
			}
			facts := rebuildEventFacts(file, language, []*graph.Node{
				emitter, listener, rebuildEventTopic(file, language, tc.topic, "tick", tc.transport),
			}, sites...)
			b, native := openRebuildEventStores(t, facts)
			require.Equal(t, tc.pairs, ResolveEventChannelCalls(native))
			stats, err := RunRebuildEventChannel(t.Context(), b, rebuildGRPCReceipts(facts), AllFrameworkSynthesizers(), 1<<20)
			require.NoError(t, err)
			require.Equal(t, uint64(tc.pairs), stats.Pairs)
			require.Equal(t, rebuildEventSQLiteRows(native), rebuildEventSavedRows(t, b))
		})
	}
}

func TestRebuildEventChannelNodeGates(t *testing.T) {
	const file, lang = "bus.ts", "typescript"
	const topic, transport = "event::pubsub::eventemitter::tick", "eventemitter"
	emitter := rebuildEventNode(file, "emit", lang, "")
	listener := rebuildEventNode(file, "listen", lang, "")
	topicNode := rebuildEventTopic(file, lang, topic, "tick", transport)
	selection, err := NewFrameworkSynthesizerSelection([]string{SynthEventChannel})
	require.NoError(t, err)

	for _, tc := range []struct {
		name  string
		nodes []*graph.Node
	}{
		{"missing topic", []*graph.Node{emitter, listener}},
		{"missing listener node", []*graph.Node{emitter, topicNode}},
		{"missing emitter node", []*graph.Node{listener, topicNode}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			facts := rebuildEventFacts(file, lang, tc.nodes,
				rebuildEventSite(emitter, topic, graph.EdgeEmits, 4, transport),
				rebuildEventSite(listener, topic, graph.EdgeListensOn, 8, transport))
			b, native := openRebuildEventStores(t, facts)
			before := rebuildEventSQLiteRows(native)
			require.Len(t, before.edges, 2)
			require.Equal(t, before, rebuildEventSavedRows(t, b))

			report := RunFrameworkSynthesizersWithSelection(native, selection)
			require.NoError(t, report.Err)
			require.Zero(t, report.Total)
			require.Equal(t, before, rebuildEventSQLiteRows(native))
			stats, err := RunRebuildEventChannel(t.Context(), b, rebuildGRPCReceipts(facts), selection, 1<<20)
			require.NoError(t, err)
			require.Zero(t, stats.Pairs)
			require.Equal(t, before, rebuildEventSavedRows(t, b))

			if tc.name == "missing topic" {
				// The bare resolver can pair these sites. Only the framework
				// driver checks for a topic node before running it.
				require.Equal(t, 1, ResolveEventChannelCalls(native))
			}
		})
	}
}

func TestRebuildEventChannelTransportFallback(t *testing.T) {
	const file, lang = "bus.ts", "typescript"
	const topic, transport = "event::pubsub::eventemitter::tick", "eventemitter"
	emitter := rebuildEventNode(file, "emit", lang, "")
	listener := rebuildEventNode(file, "listen", lang, "")
	web := rebuildEventFacts(file, lang, []*graph.Node{
		emitter, listener, rebuildEventTopic(file, lang, topic, "tick", transport),
	}, rebuildEventSite(emitter, topic, graph.EdgeEmits, 4, ""),
		rebuildEventSite(listener, topic, graph.EdgeListensOn, 8, "kafka"))
	// The Python site supplies the only Emit label but cannot pair with
	// the listener. The sole output needs Python only for transport proof.
	pyEmitter := rebuildEventNode("producer.py", "produce", "python", "foreign")
	py := rebuildEventFacts(pyEmitter.FilePath, pyEmitter.Language, []*graph.Node{pyEmitter},
		rebuildEventSite(pyEmitter, topic, graph.EdgeEmits, 2, "socketio"))

	for _, tc := range []struct {
		name          string
		facts         []store_pebble.ParseFacts
		wantTransport string
	}{
		{"topic ID beats listener label", []store_pebble.ParseFacts{web}, transport},
		{"Python emit label overrides topic ID", []store_pebble.ParseFacts{web, py}, "socketio"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			b, native := openRebuildEventStores(t, tc.facts...)
			require.Equal(t, 1, ResolveEventChannelCalls(native))
			calls := slices.Collect(native.EdgesByKind(graph.EdgeCalls))
			require.Len(t, calls, 1)
			call := calls[0]
			require.Equal(t, emitter.ID, call.From)
			require.Equal(t, listener.ID, call.To)
			require.Equal(t, tc.wantTransport, call.Meta["event_transport"])

			stats, err := RunRebuildEventChannel(t.Context(), b, rebuildGRPCReceipts(tc.facts...), AllFrameworkSynthesizers(), 1<<20)
			require.NoError(t, err)
			require.Equal(t, uint64(1), stats.Pairs)
			require.Equal(t, rebuildEventSQLiteRows(native), rebuildEventSavedRows(t, b))
			if len(tc.facts) == 2 {
				unproved, _ := openRebuildEventStores(t, tc.facts...)
				before := rebuildEventSavedRows(t, unproved)
				stats, err := RunRebuildEventChannel(t.Context(), unproved, rebuildGRPCReceipts(web), AllFrameworkSynthesizers(), 1<<20)
				var refusal *store_pebble.RebuildUnknownProvenanceError
				require.ErrorAs(t, err, &refusal)
				require.Equal(t, uint64(1), refusal.Count)
				require.Equal(t, graph.EdgeIdentityFor(call), refusal.First)
				require.Zero(t, stats)
				require.Equal(t, before, rebuildEventSavedRows(t, unproved))
			}
		})
	}
}

func TestRebuildEventChannelLowerLineInLaterPath(t *testing.T) {
	const file, laterFile, lang = "a.ts", "z.ts", "typescript"
	const topic, transport = "event::pubsub::eventemitter::tick", "eventemitter"
	emitter := rebuildEventNode(file, "emit", lang, "")
	listener := rebuildEventNode(file, "listen", lang, "")
	early := rebuildEventFacts(file, lang, []*graph.Node{
		emitter, listener, rebuildEventTopic(file, lang, topic, "tick", transport),
	}, rebuildEventSite(emitter, topic, graph.EdgeEmits, 11, transport),
		rebuildEventSite(listener, topic, graph.EdgeListensOn, 15, transport))
	lower := rebuildEventSite(emitter, topic, graph.EdgeEmits, 4, transport)
	lower.FilePath = laterFile
	later := rebuildEventFacts(laterFile, lang, nil, lower)
	b, native := openRebuildEventStores(t, early, later)
	require.Equal(t, 1, ResolveEventChannelCalls(native))
	call := synthEventEdge(native, emitter.ID, listener.ID)
	require.NotNil(t, call)
	require.Equal(t, laterFile, call.FilePath)
	require.Equal(t, 4, call.Line)

	stats, err := RunRebuildEventChannel(t.Context(), b, rebuildGRPCReceipts(early, later), AllFrameworkSynthesizers(), 1<<20)
	require.NoError(t, err)
	require.Equal(t, uint64(1), stats.Pairs)
	require.Equal(t, rebuildEventSQLiteRows(native), rebuildEventSavedRows(t, b))
}

func TestRebuildEventChannelRejectsUnprovedWinner(t *testing.T) {
	const (
		file      = "bus.ts"
		lang      = "typescript"
		topic     = "event::pubsub::eventemitter::tick"
		validFile = "a.ts"
		validID   = "event::pubsub::eventemitter::alpha"
	)
	for _, mode := range []string{"complete", "missing emitter", "incompatible emitter", "missing listener", "missing endpoint", "incompatible endpoint"} {
		t.Run(mode, func(t *testing.T) {
			emitter := rebuildEventNode(file, "send", lang, "work")
			listener := rebuildEventNode(file, "onTick", lang, "work")
			winner := rebuildEventSite(emitter, topic, graph.EdgeEmits, 4, "eventemitter")
			runnerUp := rebuildEventSite(emitter, topic, graph.EdgeEmits, 8, "eventemitter")
			listen := rebuildEventSite(listener, topic, graph.EdgeListensOn, 12, "eventemitter")
			facts := rebuildEventFacts(file, lang, []*graph.Node{
				emitter, listener, rebuildEventTopic(file, lang, topic, "tick", "eventemitter"),
			}, winner, runnerUp, listen)
			// This independent pair sorts first and remains provable even
			// when the later pair fails. No synthesized row may escape.
			validEmitter := rebuildEventNode(validFile, "send", lang, "work")
			validListener := rebuildEventNode(validFile, "onAlpha", lang, "work")
			valid := rebuildEventFacts(validFile, lang, []*graph.Node{
				validEmitter, validListener, rebuildEventTopic(validFile, lang, validID, "alpha", "eventemitter"),
			}, rebuildEventSite(validEmitter, validID, graph.EdgeEmits, 4, "eventemitter"),
				rebuildEventSite(validListener, validID, graph.EdgeListensOn, 12, "eventemitter"))
			b, native := openRebuildEventStores(t, valid, facts)
			before := rebuildEventSavedRows(t, b)
			require.Equal(t, 2, ResolveEventChannelCalls(native))
			want := synthEventEdge(native, emitter.ID, listener.ID)
			require.NotNil(t, want)
			require.Equal(t, winner.Line, want.Line)

			proof := facts
			switch mode {
			case "missing emitter":
				proof.Edges = []*graph.Edge{runnerUp, listen}
			case "incompatible emitter":
				wrong := *winner
				wrong.Meta = maps.Clone(winner.Meta)
				wrong.Meta["transport"] = "socketio"
				proof.Edges = []*graph.Edge{&wrong, runnerUp, listen}
			case "missing listener":
				proof.Edges = []*graph.Edge{winner, runnerUp}
			case "missing endpoint":
				proof.Nodes = []*graph.Node{emitter, facts.Nodes[2]}
			case "incompatible endpoint":
				// Parse receipts carry no workspace ID (checkParseNode), so the
				// endpoint proof compares receipt fields such as Kind.
				wrong := *listener
				wrong.Kind = graph.KindMethod
				proof.Nodes = []*graph.Node{emitter, &wrong, facts.Nodes[2]}
			}
			proof.File.NodeCount = len(proof.Nodes)
			stats, err := RunRebuildEventChannel(t.Context(), b, rebuildGRPCReceipts(valid, proof), AllFrameworkSynthesizers(), 1)
			if mode == "complete" {
				require.NoError(t, err)
				require.Equal(t, uint64(2), stats.Pairs)
				require.Equal(t, rebuildEventSQLiteRows(native), rebuildEventSavedRows(t, b))
				return
			}
			require.ErrorIs(t, err, store_pebble.ErrRebuildUnknownProvenance)
			require.Zero(t, stats)
			require.Equal(t, before, rebuildEventSavedRows(t, b))
		})
	}
}

func rebuildEventNode(file, name, language, workspace string) *graph.Node {
	return &graph.Node{
		ID:          file + "::" + name,
		Name:        name,
		Kind:        graph.KindFunction,
		FilePath:    file,
		Language:    language,
		WorkspaceID: workspace,
	}
}

func rebuildEventTopic(file, language, id, name, transport string) *graph.Node {
	kind := "pubsub"
	if transport == "emitter" {
		kind = "emitter"
	}
	return &graph.Node{
		ID: id, Name: name, Kind: graph.KindEvent, FilePath: file, Language: language,
		Meta: map[string]any{"transport": transport, "event_kind": kind},
	}
}

func rebuildEventSite(n *graph.Node, topic string, kind graph.EdgeKind, line int, transport string) *graph.Edge {
	return &graph.Edge{
		From: n.ID, To: topic, Kind: kind, FilePath: n.FilePath, Line: line, Origin: graph.OriginASTInferred,
		Meta: map[string]any{"transport": transport},
	}
}

func rebuildEventFacts(file, language string, nodes []*graph.Node, edges ...*graph.Edge) store_pebble.ParseFacts {
	return store_pebble.ParseFacts{
		EffectiveLanguage: language,
		File:              graph.FileMetaRow{FilePath: file, NodeCount: len(nodes)},
		Nodes:             nodes,
		Edges:             edges,
	}
}

func openRebuildEventStores(t *testing.T, facts ...store_pebble.ParseFacts) (*store_pebble.BuildStore, *store_sqlite.Store) {
	t.Helper()
	b := openRebuildGRPCBuild(t, facts...)
	native, err := store_sqlite.Open(filepath.Join(t.TempDir(), "event.sqlite"))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, native.Close()) })
	for _, f := range facts {
		require.NoError(t, native.AddBatchChecked(f.Nodes, f.Edges))
	}
	return b, native
}

type rebuildEventRows struct {
	nodes []*graph.Node
	edges []*graph.Edge
}

func rebuildEventSavedRows(t *testing.T, b *store_pebble.BuildStore) rebuildEventRows {
	t.Helper()
	var rows rebuildEventRows
	view := b.RebuildView()
	require.NoError(t, view.VisitNodes(t.Context(), func(n *graph.Node) error {
		rows.nodes = append(rows.nodes, n)
		return nil
	}))
	require.NoError(t, view.VisitEdges(t.Context(), func(e *graph.Edge) error {
		rows.edges = append(rows.edges, e)
		return nil
	}))
	rows.sort()
	return rows
}

func rebuildEventSQLiteRows(native *store_sqlite.Store) rebuildEventRows {
	rows := rebuildEventRows{nodes: native.AllNodes(), edges: native.AllEdges()}
	rows.sort()
	return rows
}

func (rows rebuildEventRows) sort() {
	slices.SortFunc(rows.nodes, func(a, b *graph.Node) int { return cmp.Compare(a.ID, b.ID) })
	slices.SortFunc(rows.edges, func(a, b *graph.Edge) int {
		return cmp.Or(cmp.Compare(a.From, b.From), cmp.Compare(a.Kind, b.Kind), cmp.Compare(a.To, b.To),
			cmp.Compare(a.FilePath, b.FilePath), cmp.Compare(a.Line, b.Line))
	})
}
