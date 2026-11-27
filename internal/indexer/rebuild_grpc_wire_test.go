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
		{"sql callsite", store_pebble.ErrRebuildSQLCallsite, 58},
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

func TestRebuildSQLCallsiteOrderWireRefusal(t *testing.T) {
	const first = `first=("caller","calls","load_calendar","calendar.ts",7)`
	cause := &store_pebble.RebuildSQLCallsiteOrderError{
		Count: 2,
		First: graph.EdgeIdentity{From: "caller", To: "load_calendar", Kind: graph.EdgeCalls, FilePath: "calendar.ts", Line: 7},
	}
	valid := buildRefusal(fmt.Errorf("%w: %w", store_pebble.ErrRebuildSQLCallsite, cause))
	require.EqualValues(t, 57, valid.Refusal)
	require.EqualValues(t, 2, valid.SQLCallsiteOrder)
	require.Zero(t, valid.MyBatisOrder)
	require.Zero(t, valid.AttributionChains)
	require.Zero(t, valid.UnknownProvenance)
	for _, tc := range []struct {
		name  string
		reply rebuildBuildReply
		want  error
		text  string
	}{
		{"valid", valid, store_pebble.ErrRebuildSQLCallsiteOrder, "sql_callsite_order=2"},
		{"missing count", rebuildBuildReply{Refusal: 57}, errRebuildBuildWire, "invalid SQL callsite move order count"},
		{"wrong refusal", rebuildBuildReply{Refusal: 58, SQLCallsiteOrder: 2}, errRebuildBuildWire, "invalid SQL callsite move order count"},
		{"MyBatis refusal with SQL count", rebuildBuildReply{Refusal: 55, MyBatisOrder: 1, SQLCallsiteOrder: 2}, errRebuildBuildWire, "invalid SQL callsite move order count"},
		{"stage offer with count", rebuildBuildReply{Witness: store_pebble.Witness{Stage: store_pebble.StageManifest}, SQLCallsiteOrder: 2},
			errRebuildBuildWire, "SQL callsite move order count on a stage offer"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var reply, acks bytes.Buffer
			require.NoError(t, writeRebuildBuildReply(&reply, tc.reply))
			err := receiveRebuildStages(t.Context(), t.TempDir(), rebuildBuildRequest{}, &reply, &acks,
				func(context.Context, store_pebble.StageTables) error {
					t.Fatal("a refusal or invalid offer must not publish")
					return nil
				})
			require.ErrorIs(t, err, tc.want)
			require.ErrorContains(t, err, tc.text)
			if tc.want == store_pebble.ErrRebuildSQLCallsiteOrder {
				require.ErrorIs(t, err, ErrRebuildWorker)
				require.ErrorContains(t, err, first)
			}
			require.Empty(t, acks.Bytes())
		})
	}
}

func TestRebuildStoreFactoryOrderWireRefusal(t *testing.T) {
	const first = `first=("caller","calls","reset","store.ts",7)`
	cause := &store_pebble.RebuildStoreFactoryOrderError{
		Count: 2,
		First: graph.EdgeIdentity{From: "caller", To: "reset", Kind: graph.EdgeCalls, FilePath: "store.ts", Line: 7},
	}
	valid := buildRefusal(fmt.Errorf("%w: %w", store_pebble.ErrRebuildStoreFactory, cause))
	require.EqualValues(t, 59, valid.Refusal)
	require.EqualValues(t, 2, valid.StoreFactoryOrder)
	require.Zero(t, valid.MyBatisOrder)
	require.Zero(t, valid.AttributionChains)
	require.Zero(t, valid.UnknownProvenance)
	for _, tc := range []struct {
		name  string
		reply rebuildBuildReply
		want  error
		text  string
	}{
		{"valid", valid, store_pebble.ErrRebuildStoreFactoryOrder, "store_factory_order=2"},
		{"missing count", rebuildBuildReply{Refusal: 59}, errRebuildBuildWire, "invalid Store Factory move order count"},
		{"wrong refusal", rebuildBuildReply{Refusal: 60, StoreFactoryOrder: 2}, errRebuildBuildWire, "invalid Store Factory move order count"},
		{"MyBatis refusal with Store Factory count", rebuildBuildReply{Refusal: 55, MyBatisOrder: 1, StoreFactoryOrder: 2}, errRebuildBuildWire, "invalid Store Factory move order count"},
		{"stage offer with count", rebuildBuildReply{Witness: store_pebble.Witness{Stage: store_pebble.StageManifest}, StoreFactoryOrder: 2},
			errRebuildBuildWire, "Store Factory move order count on a stage offer"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var reply, acks bytes.Buffer
			require.NoError(t, writeRebuildBuildReply(&reply, tc.reply))
			err := receiveRebuildStages(t.Context(), t.TempDir(), rebuildBuildRequest{}, &reply, &acks,
				func(context.Context, store_pebble.StageTables) error {
					t.Fatal("a refusal or invalid offer must not publish")
					return nil
				})
			require.ErrorIs(t, err, tc.want)
			require.ErrorContains(t, err, tc.text)
			if tc.want == store_pebble.ErrRebuildStoreFactoryOrder {
				require.ErrorIs(t, err, ErrRebuildWorker)
				require.ErrorContains(t, err, first)
			}
			require.Empty(t, acks.Bytes())
		})
	}
}
