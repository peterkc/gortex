package indexer

import (
	"context"
	"fmt"
	"math"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/zzet/gortex/internal/graph/store_sqlite"
	"go.uber.org/zap"
)

func TestContractDeltaMemoryBudgetHonorsInstalledLimit(t *testing.T) {
	for _, tc := range []struct {
		name        string
		limit, want int64
	}{
		{"unknown", 0, 64 << 20}, {"unavailable", -1, 64 << 20},
		{"unlimited", math.MaxInt64, 64 << 20}, {"tiny explicit", 1, 1},
		{"low explicit", 256 << 20, 16 << 20}, {"one GiB", 1 << 30, 64 << 20},
		{"four GiB", 4 << 30, 256 << 20}, {"eight GiB", 8 << 30, 512 << 20},
		{"large capped", 32 << 30, 512 << 20},
	} {
		t.Run(tc.name, func(t *testing.T) { require.Equal(t, tc.want, contractDeltaMemoryBudget(tc.limit)) })
	}
}

func TestContractDeltaMemoryBudgetRetainsFiveActorsAndHonorsPinnedPressure(t *testing.T) {
	// Conservative measured charge of the full20-repository checkpoint. Exercise
	// registry policy without allocating hundreds of megabytes of synthetic data.
	const measuredCheckpoint int64 = 84243420
	l := &contractDeltaLRU{budget: contractDeltaMemoryBudget(8 << 30)}
	actors := make([]ContractActor, 7)
	caches := make([]*contractDeltaCache, 7)
	for i := range actors {
		checkout := ""
		if i > 0 {
			checkout = fmt.Sprintf("checkout-%d", i)
		}
		actors[i] = NewContractActor(checkout, []string{"same-complete-cohort"})
		caches[i] = &contractDeltaCache{retainedBytes: measuredCheckpoint, complete: true}
	}
	for i := 0; i < 5; i++ {
		a := l.put(actors[i], caches[i])
		require.Equal(t, "admitted", a.reason)
		require.Zero(t, a.evicted)
		require.LessOrEqual(t, a.retained, a.budget)
	}
	retained := l.bytes
	for i := 0; i < 5; i++ {
		require.Same(t, caches[i], l.borrow(actors[i]))
	}
	require.Equal(t, retained, l.bytes, "borrowing must retain every global charge")
	require.Equal(t, "admitted", l.put(actors[5], caches[5]).reason)
	require.Same(t, caches[5], l.borrow(actors[5]))
	blocked := l.put(actors[6], caches[6])
	require.Equal(t, "pinned_pressure", blocked.reason)
	require.Zero(t, blocked.evicted)
	require.LessOrEqual(t, l.bytes, l.budget)
	// Returning one owned checkpoint makes only that actor eligible for eviction;
	// outstanding borrowed actors remain pinned and charged.
	require.Equal(t, "admitted", l.put(actors[0], caches[0]).reason)
	admitted := l.put(actors[6], caches[6])
	require.Equal(t, "admitted", admitted.reason)
	require.Equal(t, 1, admitted.evicted)
	require.Nil(t, l.borrow(actors[0]))
	require.Same(t, caches[6], l.borrow(actors[6]))
	require.Len(t, l.entries, 6)
	require.LessOrEqual(t, l.bytes, l.budget)
	low := &contractDeltaLRU{budget: contractDeltaMemoryBudget(1 << 30)}
	require.Equal(t, "over_budget", low.put(actors[0], caches[0]).reason)
	require.Nil(t, low.borrow(actors[0]))
}

func TestContractDeltaColdLowBudgetDoesNotCaptureOptionalNames(t *testing.T) {
	f := newContractJobFixture(t, map[string]string{"routes.go": jobRoutes, "consts.go": jobConsts, "helper.go": jobHelper})
	options := f.options
	ctx := t.Context()
	inputs, err := options.Materializer.CaptureContractInputs(ctx, nil, "fixture", "")
	require.NoError(t, err)
	snapshot, targets, err := NewContractFollowupCapture(options)(ctx, nil, inputs, "fixture", "")
	require.NoError(t, err)
	require.Len(t, targets, 1)
	snapshot.Work = targets[0].Work
	witnessCalls, nameCalls := 0, 0
	snapshot.CaptureProjectionWitness = func(context.Context) (*store_sqlite.PayloadInputWitness, error) { witnessCalls++; return nil, nil }
	snapshot.positiveNameCapture = func(context.Context, int64) (*contractPositiveNameProjection, error) { nameCalls++; return nil, nil }
	session, err := newContractDeltaSession(nil, snapshot, options.Config)
	require.NoError(t, err)
	session.cacheBudget = contractDeltaMemoryBudget(1)
	nativeStore := options.Store.(*store_sqlite.Store)
	_, payload, err := nativeStore.BeginPayloadGeneration(ctx, store_sqlite.PayloadGenerationRequest{OwnerKind: "dedicated_graph", GraphID: "low-budget-job", LayerID: "cold", GenerationKind: "contract_analysis", CreatedAt: 1})
	require.NoError(t, err)
	report, err := RunContractFollowup(ctx, ContractFollowupRequest{OpenEvidence: openSlice2TestEvidence, RebuildReason: ContractFollowupUnknownInterval, Snapshot: snapshot, Payload: payload, Catalog: targets[0].Catalog, Registry: options.Registry, Config: options.Config, RepoConfigs: snapshot.RepoConfigs, Leases: options.Materializer.Leases, Logger: zap.NewNop(), Yield: options.Yield, PublishedEpoch: f.epoch.Load, WaitPublished: f.waitPublished, delta: session})
	require.NoError(t, err)
	require.True(t, report.Published)
	require.Zero(t, witnessCalls, "a cold job must check its small budget before acquiring optional proof")
	require.Zero(t, nameCalls, "a cold job must not allocate the optional name projection above its budget")
}
