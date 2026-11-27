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
