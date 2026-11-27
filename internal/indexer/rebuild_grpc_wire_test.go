package indexer

import (
	"bytes"
	"context"
	"fmt"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/graph/store_pebble"
)

func TestRebuildGRPCWireRefusals(t *testing.T) {
	const first = `first=("caller","calls","target","flow.go",42)`
	for _, stage := range []struct {
		name string
		err  error
		code uint8
	}{
		{"grpc", store_pebble.ErrRebuildGRPC, 42},
		{"temporal", store_pebble.ErrRebuildTemporal, 43},
		{"event channel", store_pebble.ErrRebuildEventChannel, 44},
		{"swift objc", store_pebble.ErrRebuildSwiftObjC, 45},
		{"react native bridge", store_pebble.ErrRebuildReactNativeBridge, 46},
		{"react native pairing", store_pebble.ErrRebuildReactNativeNativePairing, 47},
		{"observer", store_pebble.ErrRebuildObserver, 48},
		{"closure collection", store_pebble.ErrRebuildClosureCollection, 49},
		{"react setState", store_pebble.ErrRebuildReactSetState, 50},
		{"flutter setState", store_pebble.ErrRebuildFlutterSetState, 51},
		{"kmp expect/actual", store_pebble.ErrRebuildKMPExpectActual, 52},
		{"expo module bridge", store_pebble.ErrRebuildExpoModuleBridge, 53},
		{"fabric", store_pebble.ErrRebuildFabric, 54},
		{"mybatis", store_pebble.ErrRebuildMyBatis, 56},
	} {
		t.Run(stage.name, func(t *testing.T) {
			for _, tc := range []struct {
				name  string
				cause error
				want  error
				code  uint8
				count uint64
			}{
				{"stage", stage.err, stage.err, stage.code, 0},
				{"provenance", &store_pebble.RebuildUnknownProvenanceError{
					Count: 2,
					First: graph.EdgeIdentity{From: "caller", To: "target", Kind: graph.EdgeCalls, FilePath: "flow.go", Line: 42},
				}, store_pebble.ErrRebuildUnknownProvenance, 36, 2},
				{"input size", store_pebble.ErrRebuildLinkInputSize, store_pebble.ErrRebuildLinkInputSize, 17, 0},
			} {
				t.Run(tc.name, func(t *testing.T) {
					cause := fmt.Errorf("%w: %w", stage.err, tc.cause)
					r := buildRefusal(cause)
					require.Equal(t, tc.code, r.Refusal)
					require.Equal(t, tc.count, r.UnknownProvenance)
					if tc.count > 0 {
						require.Contains(t, r.Detail, "count=2")
						require.Contains(t, r.Detail, first)
					}
					var reply, acks bytes.Buffer
					require.NoError(t, writeRebuildBuildReply(&reply, r))
					err := receiveRebuildStages(t.Context(), t.TempDir(), rebuildBuildRequest{}, &reply, &acks,
						func(context.Context, store_pebble.StageTables) error {
							t.Fatal("a refusal must not publish")
							return nil
						})
					require.ErrorIs(t, err, ErrRebuildWorker)
					require.ErrorIs(t, err, tc.want)
					if tc.count > 0 {
						require.ErrorContains(t, err, "unknown_provenance=2")
						require.ErrorContains(t, err, first)
					}
					if tc.code != stage.code {
						require.NotErrorIs(t, err, stage.err)
					}
					require.Empty(t, acks.Bytes())
				})
			}
		})
	}
}

func TestRebuildMyBatisOrderWireRefusal(t *testing.T) {
	cause := &store_pebble.RebuildMyBatisOrderError{
		Count: 2,
		First: graph.EdgeIdentity{From: "statement", To: "method", Kind: graph.EdgeCalls, FilePath: "mapper.xml", Line: 7},
	}
	valid := buildRefusal(fmt.Errorf("%w: %w", store_pebble.ErrRebuildMyBatis, cause))
	require.EqualValues(t, 55, valid.Refusal)
	require.EqualValues(t, 2, valid.MyBatisOrder)
	for _, tc := range []struct {
		name  string
		reply rebuildBuildReply
		want  error
	}{
		{"valid", valid, store_pebble.ErrRebuildMyBatisOrder},
		{"missing count", rebuildBuildReply{Refusal: valid.Refusal}, errRebuildBuildWire},
		{"wrong refusal", rebuildBuildReply{Refusal: 56, MyBatisOrder: 2}, errRebuildBuildWire},
		{"offer with count", rebuildBuildReply{MyBatisOrder: 2}, errRebuildBuildWire},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var reply, acks bytes.Buffer
			require.NoError(t, writeRebuildBuildReply(&reply, tc.reply))
			err := receiveRebuildStages(t.Context(), t.TempDir(), rebuildBuildRequest{}, &reply, &acks,
				func(context.Context, store_pebble.StageTables) error {
					t.Fatal("a refusal must not publish")
					return nil
				})
			require.ErrorIs(t, err, tc.want)
			if tc.want == store_pebble.ErrRebuildMyBatisOrder {
				require.ErrorContains(t, err, "mybatis_order=2")
				require.ErrorContains(t, err, `first=("statement","calls","method","mapper.xml",7)`)
			}
			require.Empty(t, acks.Bytes())
		})
	}
}
