package resolver

import (
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/graph/store_pebble"
)

func TestRebuildLaravelCallClaim(t *testing.T) {
	const file, lang = "app.php", "php"
	publisher := rebuildEventNode(file, "publish", lang, "")
	listener := rebuildEventNode(file, "listen", lang, "")
	listener.Meta = map[string]any{"laravel_listener_type": "Notice"}
	raw := &graph.Edge{
		From: publisher.ID, To: "unresolved::*.handle", Kind: graph.EdgeCalls, FilePath: file, Line: 3,
		Meta: map[string]any{"via": laravelEventVia, "laravel_event_type": "Notice"},
	}
	facts := rebuildEventFacts(file, lang, []*graph.Node{publisher, listener}, raw)
	b := openRebuildGRPCBuild(t, facts)
	mover := *raw
	mover.To = "unresolved::changed"
	require.NoError(t, b.ApplyLinkReindexes(t.Context(), []graph.EdgeReindex{{Edge: &mover, OldTo: raw.To}}))
	stationary := *raw
	stationary.Meta = map[string]any{"incumbent": true}
	b.AddEdge(&stationary)
	before := rebuildEventSavedRows(t, b)
	stats, err := RunRebuildLaravelEvent(t.Context(), b, rebuildGRPCReceipts(facts), rebuildLaravelTestSelection(t))
	require.ErrorIs(t, err, store_pebble.ErrRebuildLaravelEvent)
	require.ErrorIs(t, err, store_pebble.ErrRebuildUnknownProvenance)
	require.Zero(t, stats)
	require.Equal(t, before, rebuildEventSavedRows(t, b))
}

func rebuildLaravelTestSelection(t *testing.T) FrameworkSynthesizerSelection {
	t.Helper()
	selection, err := NewFrameworkSynthesizerSelection([]string{SynthLaravelEvent})
	require.NoError(t, err)
	return selection
}

func TestRebuildLaravelStationaryCallRoute(t *testing.T) {
	const file, lang = "app.php", "php"
	publisher := rebuildEventNode(file, "publish", lang, "")
	listener := rebuildEventNode(file, "listen", lang, "")
	listener.Meta = map[string]any{"laravel_listener_type": "Notice"}
	call := &graph.Edge{
		From: publisher.ID, To: "unresolved::*.handle", Kind: graph.EdgeCalls, FilePath: file, Line: 3,
		Meta: map[string]any{"via": laravelEventVia, "laravel_event_type": "Missing"},
	}
	for _, event := range []string{"StillMissing", "Notice"} {
		t.Run(event, func(t *testing.T) {
			facts := rebuildEventFacts(file, lang, []*graph.Node{publisher, listener}, call)
			b := openRebuildGRPCBuild(t, facts)
			before := rebuildEventSavedRows(t, b)
			raw := *call
			raw.Meta = map[string]any{"via": laravelEventVia, "laravel_event_type": event}
			facts.Edges = []*graph.Edge{&raw}
			stats, err := RunRebuildLaravelEvent(t.Context(), b, rebuildGRPCReceipts(facts), rebuildLaravelTestSelection(t))
			if event == "Notice" {
				require.ErrorIs(t, err, store_pebble.ErrRebuildUnknownProvenance)
				require.Zero(t, stats)
			} else {
				require.NoError(t, err)
				require.Equal(t, store_pebble.RebuildLaravelEventStats{Stubs: 1}, stats)
			}
			require.Equal(t, before, rebuildEventSavedRows(t, b))
		})
	}
}

func TestRebuildLaravelScriptHostReceipt(t *testing.T) {
	const file = "app.php"
	publisher := rebuildEventNode(file, "publish", "php", "")
	listener := rebuildEventNode(file, "listen", "php", "")
	listener.Meta = map[string]any{"laravel_listener_type": "Notice"}
	call := &graph.Edge{
		From: publisher.ID, To: "unresolved::*.handle", Kind: graph.EdgeCalls, FilePath: file, Line: 3,
		Meta: map[string]any{"via": laravelEventVia, "laravel_event_type": "Notice"},
	}
	facts := rebuildEventFacts(file, "php", []*graph.Node{publisher, listener}, call)
	b, native := openRebuildEventStores(t, facts)
	raw := *listener
	raw.Language = "script-host-policy"
	facts.Nodes = []*graph.Node{publisher, &raw}
	facts.EffectiveLanguage = "script-host"
	stats, err := RunRebuildLaravelEvent(t.Context(), b, rebuildGRPCReceipts(facts), rebuildLaravelTestSelection(t))
	require.NoError(t, err)
	require.Equal(t, store_pebble.RebuildLaravelEventStats{Stubs: 1, Resolved: 1, Moves: 1}, stats)
	require.Equal(t, 1, ResolveLaravelEventCalls(native))
	require.Equal(t, rebuildEventSQLiteRows(native), rebuildEventSavedRows(t, b))
}

func TestRebuildLaravelListenMapScope(t *testing.T) {
	const file, lang = "app.php", "php"
	publisher := rebuildEventNode(file, "publish", lang, "")
	provider := rebuildEventNode(file, "Provider", lang, "")
	provider.Kind = graph.KindType
	provider.Meta = map[string]any{"laravel_listen_map": "Notice=>Listener;Other=>Unrelated"}
	handle := rebuildEventNode(file, "Listener.handle", lang, "")
	handle.Kind, handle.Name = graph.KindMethod, "handle"
	member := &graph.Edge{From: handle.ID, To: file + "::Listener", Kind: graph.EdgeMemberOf, FilePath: file, Line: 3}
	for _, tc := range []struct {
		name, event, rawMap string
		want                string
		refuse              bool
	}{
		{"unrelated entry changed", "Notice", "Notice=>Listener;Other=>Another", handle.ID, false},
		{"empty elected event with another activation", "Missing", "Else=>Another", "unresolved::*.handle", false},
		{"no activation witness", "Missing", "", "", true},
		{"deciding map pair lost", "Notice", "Notice=>Unrelated", "", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			call := &graph.Edge{
				From: publisher.ID, To: publisher.ID, Kind: graph.EdgeCalls, FilePath: file, Line: 4,
				Meta: map[string]any{"via": laravelEventVia, "laravel_event_type": tc.event},
			}
			facts := rebuildEventFacts(file, lang, []*graph.Node{publisher, provider, handle}, member, call)
			b, native := openRebuildEventStores(t, facts)
			before := rebuildEventSavedRows(t, b)
			raw := *provider
			raw.Meta = map[string]any{"laravel_listen_map": tc.rawMap}
			facts.Nodes = []*graph.Node{publisher, &raw, handle}
			stats, err := RunRebuildLaravelEvent(t.Context(), b, rebuildGRPCReceipts(facts), rebuildLaravelTestSelection(t))
			if tc.refuse {
				require.ErrorIs(t, err, store_pebble.ErrRebuildUnknownProvenance)
				require.Zero(t, stats)
				require.Equal(t, before, rebuildEventSavedRows(t, b))
				return
			}
			require.NoError(t, err)
			ResolveLaravelEventCalls(native)
			saved := rebuildEventSavedRows(t, b)
			require.Equal(t, rebuildEventSQLiteRows(native), saved)
			for _, e := range saved.edges {
				if e.Kind == graph.EdgeCalls {
					require.Equal(t, tc.want, e.To)
				}
			}
		})
	}
}

func TestRebuildLaravelUncertainOwnerScope(t *testing.T) {
	const file, lang = "app.php", "php"
	publisher := rebuildEventNode(file, "publish", lang, "")
	provider := rebuildEventNode(file, "Provider", lang, "")
	provider.Kind = graph.KindType
	provider.Meta = map[string]any{"laravel_listen_map": "Notice=>Listener"}
	handle := rebuildEventNode(file, "Listener.handle", lang, "")
	handle.Kind, handle.Name = graph.KindMethod, "handle"
	handle.Meta = map[string]any{"laravel_listener_type": "Notice"}
	other := &graph.Edge{From: handle.ID, To: file + "::Other", Kind: graph.EdgeMemberOf, FilePath: file, Line: 2}
	member := &graph.Edge{From: handle.ID, To: file + "::Listener", Kind: graph.EdgeMemberOf, FilePath: file, Line: 3}
	call := &graph.Edge{
		From: publisher.ID, To: publisher.ID, Kind: graph.EdgeCalls, FilePath: file, Line: 4,
		Meta: map[string]any{"via": laravelEventVia, "laravel_event_type": "Notice"},
	}
	facts := rebuildEventFacts(file, lang, []*graph.Node{publisher, provider, handle}, other, member, call)
	b, native := openRebuildEventStores(t, facts)
	facts.Edges = facts.Edges[1:] // uncertainty cannot add another listener ID
	_, err := RunRebuildLaravelEvent(t.Context(), b, rebuildGRPCReceipts(facts), rebuildLaravelTestSelection(t))
	require.NoError(t, err)
	require.Equal(t, 1, ResolveLaravelEventCalls(native))
	require.Equal(t, rebuildEventSQLiteRows(native), rebuildEventSavedRows(t, b))
}

func TestRebuildLaravelDuplicateReceiptRoles(t *testing.T) {
	const file, lang = "app.php", "php"
	publisher := rebuildEventNode(file, "publish", lang, "")
	listener := rebuildEventNode(file, "listen", lang, "")
	listener.Meta = map[string]any{"laravel_listener_type": "Notice"}
	call := &graph.Edge{
		From: publisher.ID, To: "unresolved::*.handle", Kind: graph.EdgeCalls, FilePath: file, Line: 3,
		Meta: map[string]any{"via": laravelEventVia, "laravel_event_type": "Notice"},
	}
	for _, change := range []string{"identical", "conflict first", "conflict last"} {
		t.Run(change, func(t *testing.T) {
			facts := rebuildEventFacts(file, lang, []*graph.Node{publisher, listener}, call)
			b, native := openRebuildEventStores(t, facts)
			before := rebuildEventSavedRows(t, b)
			changed := *listener
			if change != "identical" {
				changed.Meta = map[string]any{"laravel_listener_type": "Other"}
			}
			duplicate := rebuildEventFacts(file, lang, []*graph.Node{&changed})
			receipts := rebuildGRPCReceipts(facts, duplicate)
			if change == "conflict first" {
				receipts = rebuildGRPCReceipts(duplicate, facts)
			}
			stats, err := RunRebuildLaravelEvent(t.Context(), b, receipts, rebuildLaravelTestSelection(t))
			if change != "identical" {
				require.ErrorIs(t, err, store_pebble.ErrRebuildUnknownProvenance)
				require.Zero(t, stats)
				require.Equal(t, before, rebuildEventSavedRows(t, b))
				return
			}
			require.NoError(t, err)
			require.Equal(t, store_pebble.RebuildLaravelEventStats{Stubs: 1, Resolved: 1, Moves: 1}, stats)
			require.Equal(t, 1, ResolveLaravelEventCalls(native))
			require.Equal(t, rebuildEventSQLiteRows(native), rebuildEventSavedRows(t, b))
		})
	}
}

func TestRebuildLaravelCollidingDispatchOrder(t *testing.T) {
	const file, lang = "events.php", "php"
	publisher := rebuildEventNode(file, "publish", lang, "")
	a := rebuildEventNode(file, "a", lang, "")
	b := rebuildEventNode(file, "b", lang, "")
	for _, n := range []*graph.Node{a, b} {
		n.Meta = map[string]any{"laravel_listener_type": "Notice"}
	}
	call := func(stub, event string) *graph.Edge {
		return &graph.Edge{
			From: publisher.ID, To: stub, Kind: graph.EdgeCalls, FilePath: file, Line: 3,
			Meta: map[string]any{"via": laravelEventVia, "laravel_event_type": event},
		}
	}
	first := call("unresolved::z", "First\\Notice")
	second := call("unresolved::a", "Second\\Notice")
	facts := rebuildEventFacts(file, lang, []*graph.Node{publisher, b, a}, first, second)
	build, native := openRebuildEventStores(t, facts)
	require.Equal(t, 4, ResolveLaravelEventCalls(native))
	want := rebuildEventSQLiteRows(native)
	require.Len(t, want.edges, 2)
	for i, id := range []string{a.ID, b.ID} {
		require.Equal(t, id, want.edges[i].To)
		require.Equal(t, "First\\Notice", want.edges[i].Meta["laravel_event_type"])
	}
	stats, err := RunRebuildLaravelEvent(t.Context(), build, rebuildGRPCReceipts(facts), rebuildLaravelTestSelection(t))
	require.NoError(t, err)
	require.Equal(t, store_pebble.RebuildLaravelEventStats{Stubs: 2, Resolved: 4, Moves: 2, Adds: 2}, stats)
	require.Equal(t, want, rebuildEventSavedRows(t, build))
}

func TestRebuildLaravelReceiptOnlyHandle(t *testing.T) {
	const file, lang = "events.php", "php"
	publisher := rebuildEventNode(file, "publish", lang, "")
	typed := rebuildEventNode(file, "typed", lang, "")
	typed.Meta = map[string]any{"laravel_listener_type": "Notice"}
	provider := rebuildEventNode(file, "Provider", lang, "")
	provider.Kind = graph.KindType
	provider.Meta = map[string]any{"laravel_listen_map": "Notice=>Listener"}
	leaf := rebuildEventNode(file, "Listener.handle", lang, "")
	leaf.Kind = graph.KindMethod
	member := &graph.Edge{From: leaf.ID, To: file + "::Listener", Kind: graph.EdgeMemberOf, FilePath: file, Line: 3}
	call := &graph.Edge{
		From: publisher.ID, To: "unresolved::*.handle", Kind: graph.EdgeCalls, FilePath: file, Line: 4,
		Meta: map[string]any{"via": laravelEventVia, "laravel_event_type": "Notice"},
	}
	facts := rebuildEventFacts(file, lang, []*graph.Node{publisher, typed, provider, leaf}, member, call)
	for _, mode := range []string{"raw handle", "wrong node receipt file", "empty node effective language", "wrong Call receipt file", "empty Call effective language", "wrong Call route"} {
		t.Run(mode, func(t *testing.T) {
			build := openRebuildGRPCBuild(t, facts)
			before := rebuildEventSavedRows(t, build)
			declarations, calls := facts, facts
			declarations.Edges, calls.Nodes = nil, nil
			calls.Edges = []*graph.Edge{call}
			declarations.Edges = []*graph.Edge{member}
			switch mode {
			case "raw handle":
				changed := *leaf
				changed.Name = "handle"
				declarations.Nodes = []*graph.Node{publisher, typed, provider, &changed}
			case "wrong node receipt file":
				declarations.File.FilePath = "wrong.php"
			case "empty node effective language":
				declarations.EffectiveLanguage = ""
			case "wrong Call receipt file":
				calls.File.FilePath = "wrong.php"
			case "empty Call effective language":
				calls.EffectiveLanguage = ""
			case "wrong Call route":
				changed := *call
				changed.Meta = map[string]any{"via": laravelEventVia, "laravel_event_type": "Different"}
				calls.Edges = []*graph.Edge{&changed}
			}
			stats, err := RunRebuildLaravelEvent(t.Context(), build, rebuildGRPCReceipts(declarations, calls), rebuildLaravelTestSelection(t))
			require.ErrorIs(t, err, store_pebble.ErrRebuildUnknownProvenance)
			require.Zero(t, stats)
			require.Equal(t, before, rebuildEventSavedRows(t, build))
		})
	}
}
