package store_pebble

import (
	"strings"
	"testing"

	"github.com/cockroachdb/pebble/v2"
	"github.com/stretchr/testify/require"
	"github.com/zzet/gortex/internal/graph"
)

func TestRebuildReactNativeNativePairingLightNodeScan(t *testing.T) {
	for _, lang := range []string{"go", "swift"} {
		t.Run(lang, func(t *testing.T) {
			b := newBuild(t, FullProfile)
			n := &graph.Node{ID: "method", Kind: graph.KindMethod, Language: lang}
			raw, err := encodeNode(n)
			require.NoError(t, err)
			// Preserve a valid record with a length-framed metadata payload.
			// Only decoding the metadata itself will find the invalid tag.
			raw = appendBytes(appendBool(raw[:len(raw)-1], true), []byte{0xff})
			_, err = decodeNodeLight(raw)
			require.NoError(t, err)
			_, err = decodeNode(raw)
			require.ErrorIs(t, err, errInvalidRecord)
			require.NoError(t, b.pending.Set(pendingNodeKey(n.ID), raw, pebble.NoSync))
			stats, err := b.RebuildReactNativeNativePairing(t.Context(), func(func(ParseFacts, error) bool) {
				t.Fatal("no pair can require receipts")
			}, func(language string) string {
				if language == "swift" {
					return "ios"
				}
				return ""
			}, func(*graph.Node, *graph.Node, string, string) *graph.Edge {
				t.Fatal("no pair can propose an edge")
				return nil
			})
			require.Zero(t, stats)
			if lang == "go" {
				require.NoError(t, err)
			} else {
				require.ErrorIs(t, err, ErrRebuildReactNativeNativePairing)
				require.ErrorIs(t, err, errInvalidRecord)
			}
		})
	}
}

func TestRebuildReactNativeNativePairingRefusesMismatchedNodeKey(t *testing.T) {
	b := newBuild(t, FullProfile)
	n := &graph.Node{ID: "method", Kind: graph.KindMethod, Language: "go"}
	raw, err := encodeNode(n)
	require.NoError(t, err)
	require.NoError(t, b.pending.Set(pendingNodeKey("other"), raw, pebble.NoSync))
	stats, err := b.RebuildReactNativeNativePairing(t.Context(), func(func(ParseFacts, error) bool) {
		t.Fatal("refusal must precede receipt reads")
	}, func(string) string { return "" }, func(*graph.Node, *graph.Node, string, string) *graph.Edge {
		t.Fatal("a Go row cannot pair")
		return nil
	})
	require.ErrorIs(t, err, ErrRebuildReactNativeNativePairing)
	require.ErrorIs(t, err, errInvalidRecord)
	require.ErrorContains(t, err, "working node ID does not match key")
	require.Zero(t, stats)
}

func TestRebuildReactNativeNativePairingScratch(t *testing.T) {
	b := newBuild(t, FullProfile)
	ctx := t.Context()
	run := func() (RebuildReactNativeNativePairingStats, error) {
		return b.RebuildReactNativeNativePairing(ctx, func(func(ParseFacts, error) bool) {
			t.Fatal("refusal must precede receipt reads")
		}, func(string) string { return "ios" }, func(*graph.Node, *graph.Node, string, string) *graph.Edge {
			t.Fatal("refusal must precede pairing")
			return nil
		})
	}
	key := rebuildRNPairKey('i', "occupied")
	require.NoError(t, b.pending.Set(key, []byte("owner"), pebble.NoSync))
	stats, err := run()
	require.ErrorIs(t, err, ErrRebuildReactNativeNativePairing)
	require.ErrorContains(t, err, "React Native native pairing scratch range occupied")
	require.Zero(t, stats)
	owner, err := pendingGet(b.pending, key, func(raw []byte) (string, error) { return string(raw), nil })
	require.NoError(t, err)
	require.Equal(t, "owner", owner)
	require.NoError(t, b.pending.Delete(key, pebble.NoSync))

	// Capture one valid method before refusing the oversized scratch key.
	for _, n := range []*graph.Node{
		{ID: "a", Kind: graph.KindMethod, Language: "swift", Meta: map[string]any{"rn_module": "Calendar", "rn_method": "open"}},
		{ID: "z", Kind: graph.KindMethod, Language: "swift", Meta: map[string]any{"rn_module": strings.Repeat("x", 16<<20), "rn_method": "open"}},
	} {
		b.AddNode(n)
	}
	stats, err = run()
	require.ErrorIs(t, err, ErrRebuildReactNativeNativePairing)
	require.ErrorIs(t, err, ErrRebuildLinkInputSize)
	require.ErrorContains(t, err, "React Native native pairing scratch row")
	require.Zero(t, stats)
	busy, err := rebuildGuardHasPrefix(ctx, b.pending, rebuildRNPairPrefix)
	require.NoError(t, err)
	require.False(t, busy)
	require.NoError(t, b.RebuildView().VisitEdges(ctx, func(*graph.Edge) error {
		t.Fatal("a scratch refusal wrote an edge")
		return nil
	}))
}
