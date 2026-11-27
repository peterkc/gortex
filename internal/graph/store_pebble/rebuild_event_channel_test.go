package store_pebble

import (
	"path/filepath"
	"testing"

	"github.com/cockroachdb/pebble/v2"
	"github.com/stretchr/testify/require"
	"github.com/zzet/gortex/internal/graph"
)

func TestRebuildEventChannelRejectsInvalidScratch(t *testing.T) {
	b, err := OpenBuild(filepath.Join(t.TempDir(), "working"), FullProfile)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, b.Close()) })
	ctx := t.Context()
	const prefix = "event::emitter::"
	const topic = prefix + "bus::tick"
	b.AddNode(&graph.Node{ID: topic, Kind: graph.KindEvent, Name: "tick", FilePath: "bus.ts"})
	run := func(prefixes []string) (RebuildEventChannelStats, error) {
		return b.RebuildEventChannel(ctx, func(func(ParseFacts, error) bool) {
			t.Fatal("scratch refusal must precede receipt reads")
		}, prefixes, func(string, string) (string, int) {
			t.Fatal("scratch refusal must precede election")
			return "", 0
		}, func(string, string, *graph.Edge, *graph.Edge) *graph.Edge {
			t.Fatal("scratch refusal must precede call synthesis")
			return nil
		})
	}
	for _, tc := range []struct {
		name     string
		prefixes []string
	}{
		{"nil prefixes", nil},
		{"empty prefixes", []string{}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			stats, err := run(tc.prefixes)
			require.ErrorIs(t, err, ErrRebuildEventChannel)
			require.ErrorIs(t, err, ErrRebuildLinkInputSize)
			require.Zero(t, stats)
		})
	}

	// The turn must not read or replace scratch owned by another pass.
	key := eventScratch('t', "occupied")
	require.NoError(t, b.pending.Set(key, []byte("owner"), pebble.NoSync))
	stats, err := run([]string{prefix})
	require.ErrorIs(t, err, ErrRebuildEventChannel)
	require.ErrorContains(t, err, "event channel scratch range occupied")
	require.Zero(t, stats)
	owner, err := pendingGet(b.pending, key, func(raw []byte) (string, error) { return string(raw), nil })
	require.NoError(t, err)
	require.Equal(t, "owner", owner)
	require.NoError(t, b.pending.Delete(key, pebble.NoSync))

	large := make([]byte, 16<<20)
	scratch := rebuildEventChannel{b: b}
	err = scratch.put(eventScratch('t', "oversized"), large)
	require.ErrorIs(t, err, ErrRebuildLinkInputSize)
	require.ErrorContains(t, err, "event channel scratch row")
	busy, err := rebuildGuardHasPrefix(ctx, b.pending, rebuildEventPrefix)
	require.NoError(t, err)
	require.False(t, busy, "the oversized scratch row was not stored")

	// An earlier valid emit populates scratch before the oversized input
	// arrives. The rejection must clean up those partial scratch rows.
	site := &graph.Edge{From: "a", To: topic, Kind: graph.EdgeEmits, FilePath: "bus.ts", Line: 1}
	raw, err := encodeEdge(site)
	require.NoError(t, err)
	require.NoError(t, b.pending.Set(pendingEdgeKey(site), raw, pebble.NoSync))
	site.From = "z"
	oversized := pendingEdgeKey(site)
	require.NoError(t, b.pending.Set(oversized, large, pebble.NoSync))
	stats, err = run([]string{prefix})
	require.ErrorIs(t, err, ErrRebuildEventChannel)
	require.ErrorIs(t, err, ErrRebuildLinkInputSize)
	require.ErrorContains(t, err, "event channel input row")
	require.Zero(t, stats)
	busy, err = rebuildGuardHasPrefix(ctx, b.pending, rebuildEventPrefix)
	require.NoError(t, err)
	require.False(t, busy)
	busy, err = rebuildGuardHasPrefix(ctx, b.pending, keyStrings([]byte{familyEdge, edgeOut}, "a", string(graph.EdgeCalls)))
	require.NoError(t, err)
	require.False(t, busy)
}

func TestRebuildEventChannelSkipsReceiptsWithoutBothSides(t *testing.T) {
	const prefix, file = "event::emitter::", "bus.ts"
	const topic = prefix + "bus::tick"
	for _, kind := range []graph.EdgeKind{graph.EdgeEmits, graph.EdgeListensOn} {
		t.Run(string(kind), func(t *testing.T) {
			b := newBuild(t, FullProfile)
			b.AddNode(&graph.Node{ID: topic, Kind: graph.KindEvent, FilePath: file})
			b.AddEdge(&graph.Edge{From: "site", To: topic, Kind: kind, FilePath: file, Line: 1})
			stats, err := b.RebuildEventChannel(t.Context(), func(func(ParseFacts, error) bool) {
				t.Fatal("read receipts without both sides")
			}, []string{prefix}, func(string, string) (string, int) {
				t.Fatal("elected without both sides")
				return "", 0
			}, func(string, string, *graph.Edge, *graph.Edge) *graph.Edge {
				t.Fatal("synthesized without both sides")
				return nil
			})
			require.NoError(t, err)
			require.Zero(t, stats)
		})
	}
}

func TestRebuildEventChannelShortSiteKey(t *testing.T) {
	b := newBuild(t, FullProfile)
	const topic = "event::emitter::bus::tick"
	p := rebuildEventChannel{b: b}
	key := append(eventScratch('e', topic), make([]byte, 8)...)
	require.NoError(t, p.put(key, nil))
	err := p.sites(t.Context(), 'e', topic, func(*graph.Edge) error {
		t.Fatal("visited an edge without a scratch identity")
		return nil
	})
	require.ErrorIs(t, err, errInvalidRecord)
	require.ErrorContains(t, err, "event channel site key")
}

func TestRebuildEventChannelTopicGateChecksOnlyMatchingRows(t *testing.T) {
	b, err := OpenBuild(filepath.Join(t.TempDir(), "working"), FullProfile)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, b.Close()) })
	ctx := t.Context()
	prefixes := []string{"event::pubsub::", "event::emitter::"}
	for _, id := range []string{"a::unrelated", "event::unrelated", "z::unrelated"} {
		// An unrelated value must not be read by the topic presence gate.
		require.NoError(t, b.pending.Set(pendingNodeKey(id), []byte("not a node"), pebble.NoSync))
	}
	found, err := b.hasRebuildEventTopic(ctx, prefixes)
	require.NoError(t, err)
	require.False(t, found)
	b.AddNode(&graph.Node{ID: "event::emitter::bus::tick", Kind: graph.KindEvent, Name: "tick", FilePath: "bus.ts"})
	found, err = b.hasRebuildEventTopic(ctx, prefixes)
	require.NoError(t, err)
	require.True(t, found)
}

func TestRebuildEventChannelTopicGateRefusesInvalidMatch(t *testing.T) {
	b, err := OpenBuild(filepath.Join(t.TempDir(), "working"), FullProfile)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, b.Close()) })
	ctx := t.Context()
	const prefix = "event::pubsub::"
	key := pendingNodeKey(prefix + "socketio::tick")
	other, err := encodeNode(&graph.Node{ID: prefix + "socketio::other", Kind: graph.KindEvent, Name: "other", FilePath: "bus.ts"})
	require.NoError(t, err)
	for _, tc := range []struct {
		name  string
		value []byte
		cause error
	}{
		{"malformed", []byte("not a node"), errInvalidRecord},
		{"mismatched identity", other, errInvalidRecord},
		{"oversized", make([]byte, 16<<20), ErrRebuildLinkInputSize},
	} {
		t.Run(tc.name, func(t *testing.T) {
			require.NoError(t, b.pending.Set(key, tc.value, pebble.NoSync))
			found, err := b.hasRebuildEventTopic(ctx, []string{prefix})
			require.ErrorIs(t, err, tc.cause)
			require.False(t, found)
		})
	}
}
