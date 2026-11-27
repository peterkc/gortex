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
		want               string
		refuse             bool
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
