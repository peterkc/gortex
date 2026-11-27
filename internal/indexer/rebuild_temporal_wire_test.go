package indexer

import (
	"bytes"
	"context"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/graph/store_pebble"
)

func TestRebuildTemporalMoveSizeWireRefusal(t *testing.T) {
	ctx := t.Context()
	b, err := store_pebble.OpenBuild(filepath.Join(t.TempDir(), "working"), store_pebble.FullProfile)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, b.Close()) })
	stats, err := b.RebuildTemporal(ctx, func(func(store_pebble.ParseFacts, error) bool) {
		t.Fatal("an oversized move must refuse before receipt proof")
	}, 1<<20, func(p *store_pebble.RebuildTemporalPass) (store_pebble.RebuildTemporalStats, error) {
		_, err := p.View().WithTemporalConstDeref(ctx, 1<<20, func(func(string) error) error {
			return nil
		}, func(*graph.Node) store_pebble.TemporalConstCandidate {
			return store_pebble.TemporalConstCandidate{}
		}, func(*store_pebble.TemporalConstDeref) error {
			return p.QueueMove(ctx, "old", &graph.Edge{
				From: "caller", To: "target", Kind: graph.EdgeCalls, FilePath: "flow.go", Line: 1,
				Meta: map[string]any{"payload": strings.Repeat("x", 16<<20)},
			}, true)
		})
		return store_pebble.RebuildTemporalStats{}, err
	})
	require.ErrorIs(t, err, store_pebble.ErrRebuildTemporal)
	require.ErrorIs(t, err, store_pebble.ErrRebuildLinkInputSize)
	require.NotErrorIs(t, err, store_pebble.ErrRebuildTemporalConst)
	require.Zero(t, stats)
	r := buildRefusal(err)
	require.Equal(t, uint8(17), r.Refusal)
	var reply, acks bytes.Buffer
	require.NoError(t, writeRebuildBuildReply(&reply, r))
	err = receiveRebuildStages(ctx, t.TempDir(), rebuildBuildRequest{}, &reply, &acks,
		func(context.Context, store_pebble.StageTables) error {
			t.Fatal("a refused Temporal turn must not publish")
			return nil
		})
	require.ErrorIs(t, err, ErrRebuildWorker)
	require.ErrorIs(t, err, store_pebble.ErrRebuildLinkInputSize)
	require.Empty(t, acks.Bytes())
}
