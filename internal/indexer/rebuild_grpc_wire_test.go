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
		{"store factory", store_pebble.ErrRebuildStoreFactory, 60},
		{"redux thunk", store_pebble.ErrRebuildReduxThunk, 61},
		{"NgRx Effect", store_pebble.ErrRebuildNgRxEffect, 62},
		{"object registry", store_pebble.ErrRebuildObjectRegistry, 63},
		{"RTK Query", store_pebble.ErrRebuildRTKQuery, 64},
		{"Vuex Dispatch", store_pebble.ErrRebuildVuexDispatch, 65},
		{"Celery", store_pebble.ErrRebuildCelery, 66},
		{"Spring events", store_pebble.ErrRebuildSpringEvents, 67},
		{"MediatR", store_pebble.ErrRebuildMediatR, 68},
		{"C# DI interfaces", store_pebble.ErrRebuildCSharpDIIfaces, 69},
		{"C# partial merge", store_pebble.ErrRebuildCSharpPartial, 70},
		{"Sidekiq", store_pebble.ErrRebuildSidekiq, 71},
		{"Laravel event", store_pebble.ErrRebuildLaravelEvent, 72},
		{"function pointer", store_pebble.ErrRebuildFnPointer, 73},
		{"C# interface dispatch", store_pebble.ErrRebuildCSharpIfaceDispatch, 74},
		{"EF Core models", store_pebble.ErrRebuildEFCoreModels, 75},
		{"macro expansion", store_pebble.ErrRebuildMacroExpansion, 76},
		{"Gin middleware", store_pebble.ErrRebuildGinMiddleware, 77},
		{"Express", store_pebble.ErrRebuildExpress, 78},
		{"React hooks and context", store_pebble.ErrRebuildReactResolve, 79},
		{"FastAPI", store_pebble.ErrRebuildFastAPI, 80},
		{"Rails", store_pebble.ErrRebuildRails, 81},
		{"Godot autoload", store_pebble.ErrRebuildGodotAutoload, 82},
		{"Godot preload alias", store_pebble.ErrRebuildGodotPreloadAlias, 83},
		{"SwiftUI", store_pebble.ErrRebuildSwiftUI, 85},
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

func TestRebuildCallMoveOrderWireCodesRetired(t *testing.T) {
	for _, tc := range []struct {
		retired uint8
		stage   error
	}{
		{55, store_pebble.ErrRebuildMyBatis},
		{57, store_pebble.ErrRebuildSQLCallsite},
		{59, store_pebble.ErrRebuildStoreFactory},
	} {
		t.Run(fmt.Sprint(tc.retired), func(t *testing.T) {
			require.Nil(t, rebuildFailureKinds[tc.retired-1])
			require.Equal(t, tc.retired+1, buildRefusal(tc.stage).Refusal)
			var reply, acks bytes.Buffer
			require.NoError(t, writeRebuildBuildReply(&reply, rebuildBuildReply{Refusal: tc.retired}))
			err := receiveRebuildStages(t.Context(), t.TempDir(), rebuildBuildRequest{}, &reply, &acks,
				func(context.Context, store_pebble.StageTables) error {
					t.Fatal("a retired refusal must not publish")
					return nil
				})
			require.ErrorIs(t, err, errRebuildBuildWire)
			require.ErrorContains(t, err, "retired refusal code")
			require.Empty(t, acks.Bytes())
		})
	}
}
