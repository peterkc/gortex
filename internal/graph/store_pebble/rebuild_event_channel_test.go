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
	run := func(limit int, prefixes []string) (RebuildEventChannelStats, error) {
		return b.RebuildEventChannel(ctx, func(func(ParseFacts, error) bool) {
			t.Fatal("scratch refusal must precede receipt reads")
		}, limit, prefixes, func(string, string) (string, int) {
			t.Fatal("scratch refusal must precede election")
			return "", 0
		}, func(string, string, *graph.Edge, *graph.Edge) *graph.Edge {
			t.Fatal("scratch refusal must precede call synthesis")
			return nil
		})
	}
	for _, tc := range []struct {
		name     string
		limit    int
		prefixes []string
	}{
		{"zero limit", 0, []string{prefix}},
		{"oversize limit", goldBufferBytes + 1, []string{prefix}},
		{"nil prefixes", 1, nil},
		{"empty prefixes", 1, []string{}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			stats, err := run(tc.limit, tc.prefixes)
			require.ErrorIs(t, err, ErrRebuildEventChannel)
			require.ErrorIs(t, err, ErrRebuildLinkInputSize)
			require.Zero(t, stats)
		})
	}

	key := eventScratch('t', "occupied")
	require.NoError(t, b.pending.Set(key, []byte("owner"), pebble.NoSync))
	stats, err := run(1, []string{prefix})
	require.ErrorIs(t, err, ErrRebuildEventChannel)
	require.ErrorContains(t, err, "event channel scratch range occupied")
	require.Zero(t, stats)
	owner, err := pendingGet(b.pending, key, func(raw []byte) (string, error) { return string(raw), nil })
	require.NoError(t, err)
	require.Equal(t, "owner", owner)
	require.NoError(t, b.pending.Delete(key, pebble.NoSync))

	large := make([]byte, 16<<20)
	scratch := rebuildEventChannel{b: b, threshold: 1 << 20}
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
	stats, err = run(1<<20, []string{prefix})
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
