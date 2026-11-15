package indexer

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/zzet/gortex/internal/config"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/graph/store_sqlite"
	"github.com/zzet/gortex/internal/graphview"
	"go.uber.org/zap"
	"go.uber.org/zap/zaptest/observer"
)

func TestRepositoryCleanupPendingPreservesDurableWriteFailure(t *testing.T) {
	writeFailure := errors.New("journal write failure")
	for _, test := range []struct {
		name      string
		err       error
		pending   bool
		wantError error
	}{
		{"direct", ErrRepositoryCleanupPending, true, nil},
		{"wrapped", fmt.Errorf("graph: %w", ErrRepositoryCleanupPending), true, nil},
		{"nested pending", errors.Join(ErrRepositoryCleanupPending, errors.Join(fmt.Errorf("nested: %w", ErrRepositoryCleanupPending))), true, nil},
		{"joined write failure", errors.Join(ErrRepositoryCleanupPending, writeFailure), true, writeFailure},
		{"ordinary write failure", writeFailure, false, writeFailure},
	} {
		t.Run(test.name, func(t *testing.T) {
			out, err := repositoryCleanupUntrackResult(UntrackResult{Prefix: "repo"}, test.err)
			if out.Prefix != "repo" || out.Pending != test.pending {
				t.Fatalf("result=%+v", out)
			}
			if test.wantError == nil && err != nil {
				t.Fatalf("pure deferral became error: %v", err)
			}
			if test.wantError != nil && !errors.Is(err, test.wantError) {
				t.Fatalf("durable failure suppressed: %v", err)
			}
		})
	}
}

func TestRepositoryCleanupRetryWaitsForOuterAttemptAndOwnedDrain(t *testing.T) {
	state := &repositoryCleanupState{graphID: "graph", initialized: true, attemptDone: make(chan struct{}), waitReady: make(chan struct{})}
	runtime := &repositoryCleanupRuntime{states: map[string]*repositoryCleanupState{"graph": state}, wake: make(chan struct{}, 1)}
	lifecycle := &CheckoutLifecycle{repositoryCleanup: runtime}
	hooks := cleanupHooks{l: lifecycle}
	finish, err := hooks.BeginGraphCleanupAttempt(context.Background(), "graph")
	if err != nil || finish == nil {
		t.Fatalf("capture completion: finish_nil=%v err=%v", finish == nil, err)
	}
	close(state.waitReady)
	if runtime.actionable() {
		t.Fatal("drain completion raced outer journal persistence")
	}
	finish()
	if !runtime.actionable() {
		t.Fatal("completed outer attempt did not become retryable")
	}
	select {
	case <-runtime.wake:
	default:
		t.Fatal("missing targeted first wake")
	}
	for n := 0; n < 100; n++ {
		finish()
	}
	select {
	case <-runtime.wake:
		t.Fatal("retry completion created a self-wake loop")
	default:
	}
}

func TestRepositoryCleanupPartialInitializationRemainsRetryable(t *testing.T) {
	state := &repositoryCleanupState{graphID: "graph", attemptDone: make(chan struct{}), waitReady: make(chan struct{})}
	runtime := &repositoryCleanupRuntime{states: map[string]*repositoryCleanupState{"graph": state}}
	if runtime.actionable() {
		t.Fatal("partial close retried before original journal write")
	}
	close(state.attemptDone)
	if !runtime.actionable() {
		t.Fatal("partial close failure has no retry owner")
	}
}

// cleanupReadyObserver records finalization without making assertions inside
// the cleanup runtime's callback. The real publisher still owns its admission.
type cleanupReadyObserver struct {
	*DedicatedBaseRuntime
	finalized chan string
}

func (o *cleanupReadyObserver) FinalizeDedicatedBaseOwner(graphID string, owner store_sqlite.DedicatedBaseOwner, drained <-chan struct{}) error {
	if err := o.DedicatedBaseRuntime.FinalizeDedicatedBaseOwner(graphID, owner, drained); err != nil {
		return err
	}
	o.finalized <- graphID
	return nil
}

func observeCleanupReady(t *testing.T, f *lifecycleFixture) *cleanupReadyObserver {
	t.Helper()
	runtime, err := NewDedicatedBaseRuntime(f.store, testCatalog(f.store), f.lc.ViewLeases())
	require.NoError(t, err)
	observer := &cleanupReadyObserver{DedicatedBaseRuntime: runtime, finalized: make(chan string, 8)}
	require.NoError(t, f.lc.SetDedicatedBaseCleanupRuntime(observer))
	return observer
}

func cleanupReadyState(t *testing.T, l *CheckoutLifecycle, graphID string) *repositoryCleanupState {
	t.Helper()
	l.repositoryCleanupMu.Lock()
	runtime := l.repositoryCleanup
	l.repositoryCleanupMu.Unlock()
	require.NotNil(t, runtime)
	runtime.mu.Lock()
	state := runtime.states[graphID]
	runtime.mu.Unlock()
	require.NotNil(t, state)
	return state
}

func drainedCleanupState(t *testing.T, l *CheckoutLifecycle, graphID string) *repositoryCleanupState {
	t.Helper()
	state := cleanupReadyState(t, l, graphID)
	select {
	case <-state.waitReady:
	case <-time.After(10 * time.Second):
		t.Fatal("repository cleanup did not drain its captured owners")
	}
	return state
}

func awaitCleanupReadyFinalization(t *testing.T, observer *cleanupReadyObserver, state *repositoryCleanupState) {
	t.Helper()
	select {
	case graphID := <-observer.finalized:
		require.Equal(t, state.graphID, graphID)
	case <-time.After(10 * time.Second):
		t.Fatal("ready did not wake the drained repository cleanup")
	}
	// The callback runs under state.mu before finalization. Taking the same
	// lock joins the remaining owner/lease finalization, not a timing delay.
	state.mu.Lock()
	finalized := state.finalized
	state.mu.Unlock()
	require.True(t, finalized)
}

type cleanupReadyPayload struct {
	*store_sqlite.Store
	mu      sync.Mutex
	retired []int64
	purged  []string
}

func (s *cleanupReadyPayload) RetirePayloadGeneration(ctx context.Context, generationID int64, inUse func(int64) bool) error {
	s.mu.Lock()
	s.retired = append(s.retired, generationID)
	s.mu.Unlock()
	return s.Store.RetirePayloadGeneration(ctx, generationID, inUse)
}

func (s *cleanupReadyPayload) PurgeRepo(prefix string) error {
	s.mu.Lock()
	s.purged = append(s.purged, prefix)
	s.mu.Unlock()
	return s.Store.PurgeRepo(prefix)
}

func (s *cleanupReadyPayload) calls() ([]int64, []string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return slices.Clone(s.retired), slices.Clone(s.purged)
}

func TestRepositoryCleanupReleaseWaitsForDaemonReady(t *testing.T) {
	// Given a drained closing graph with both positive and generation-zero
	// payload, daemon readiness is the only remaining release barrier.
	f := newLifecycleFixture(t)
	t.Cleanup(f.close)
	observer := observeCleanupReady(t, f)
	ctx := t.Context()
	root := f.gitRepo("release-after-ready")
	tracked, err := f.lc.Register(ctx, config.RepoEntry{Path: root, Name: "release-after-ready"}, TrackSourceCLI)
	require.NoError(t, err)
	require.NoError(t, tracked.CatalogErr)
	node := sentinelNode(tracked.Prefix, "ReadySentinel")
	f.store.AddBatch([]*graph.Node{node}, nil)
	generationID, handle, err := f.store.BeginPayloadGeneration(ctx, store_sqlite.PayloadGenerationRequest{
		OwnerKind: checkoutLayerOwnerKind, GraphID: tracked.GraphID, CheckoutID: tracked.CheckoutID,
		GenerationKind: "commit", LayerID: "ready-layer", TreeOID: "ready-tree", ConfigHash: "ready-config",
	})
	require.NoError(t, err)
	handle.AddBatch([]*graph.Node{sentinelNode(tracked.Prefix, "PositiveSentinel")}, nil)
	require.NoError(t, f.store.PublishPayloadGeneration(ctx, generationID, 1))
	payload := &cleanupReadyPayload{Store: f.store}
	f.mi.graph = payload
	f.lc.EnableDeferredRepositoryCleanup()
	f.lc.EnableDeferredRepositoryCleanup()

	// When explicit cleanup, direct replay, and the janitor all reach release
	// before ready, no caller may cross the physical release boundary.
	out, err := f.lc.Untrack(ctx, root)
	require.NoError(t, err)
	require.True(t, out.Pending)
	state := drainedCleanupState(t, f.lc, tracked.GraphID)
	require.ErrorIs(t, f.lc.releaseRepositoryGraph(ctx, tracked.GraphID), ErrRepositoryCleanupPending)
	require.True(t, onlyRepositoryCleanupPending(f.lc.rec.Resume(ctx)))
	_, err = f.lc.SweepDeferredRetirement(ctx)
	require.NoError(t, err)
	retired, purged := payload.calls()
	require.Empty(t, retired)
	require.Empty(t, purged)
	require.NotNil(t, f.store.GetNode(node.ID))
	_, found, err := f.catalog.GetDedicatedGraph(ctx, tracked.GraphID)
	require.NoError(t, err)
	require.True(t, found)
	_, err = f.lc.AcquireRepositoryRead(tracked.GraphID)
	require.ErrorIs(t, err, graphview.ErrRepositoryAdmissionClosed)
	entries, err := f.catalog.ListCleanupEntries(ctx)
	require.NoError(t, err)
	require.NotEmpty(t, entries)

	// Then opening ready wakes the already-drained runtime without another
	// Seed, Resume, or janitor tick and completes the real SQLite purge.
	f.lc.MarkRepositoryCleanupReady()
	f.lc.MarkRepositoryCleanupReady()
	awaitCleanupReadyFinalization(t, observer, state)
	retired, purged = payload.calls()
	require.Equal(t, []int64{generationID}, retired)
	require.Equal(t, []string{tracked.Prefix}, purged)
	require.Nil(t, f.store.GetNode(node.ID))
	entries, err = f.catalog.ListCleanupEntries(ctx)
	require.NoError(t, err)
	require.Empty(t, entries)
	_, found, err = f.catalog.GetDedicatedGraph(ctx, tracked.GraphID)
	require.NoError(t, err)
	require.False(t, found)
}

func TestRepositoryCleanupReadyStillWaitsForCapturedReader(t *testing.T) {
	// Given a reader admitted before cleanup closed the graph.
	f := newLifecycleFixture(t)
	t.Cleanup(f.close)
	observer := observeCleanupReady(t, f)
	root := f.gitRepo("ready-reader")
	tracked, err := f.lc.Register(t.Context(), config.RepoEntry{Path: root, Name: "ready-reader"}, TrackSourceCLI)
	require.NoError(t, err)
	require.NoError(t, tracked.CatalogErr)
	node := sentinelNode(tracked.Prefix, "CapturedReaderSentinel")
	f.store.AddBatch([]*graph.Node{node}, nil)
	pin := f.lc.ViewLeases().AcquireBaseCorpus(tracked.Prefix)
	t.Cleanup(pin.Release)
	require.True(t, pin.OwnerPinned())
	f.lc.EnableDeferredRepositoryCleanup()
	out, err := f.lc.Untrack(t.Context(), root)
	require.NoError(t, err)
	require.True(t, out.Pending)

	state := cleanupReadyState(t, f.lc, tracked.GraphID)
	// When ready opens before that reader closes, physical release still
	// refuses and the reader's real corpus remains intact.
	f.lc.MarkRepositoryCleanupReady()
	require.ErrorIs(t, f.lc.releaseRepositoryGraph(t.Context(), tracked.GraphID), ErrRepositoryCleanupPending)
	require.NotNil(t, f.store.GetNode(node.ID))

	// Then draining that exact reader wakes cleanup and only then removes it.
	pin.Release()
	awaitCleanupReadyFinalization(t, observer, state)
	require.Nil(t, f.store.GetNode(node.ID))
}

func TestRepositoryCleanupCloseJoinsPreReadyWaiter(t *testing.T) {
	// Given a fully drained cleanup whose daemon never becomes ready.
	f := newLifecycleFixture(t)
	t.Cleanup(f.close)
	root := f.gitRepo("never-ready")
	tracked, err := f.lc.Register(t.Context(), config.RepoEntry{Path: root, Name: "never-ready"}, TrackSourceCLI)
	require.NoError(t, err)
	require.NoError(t, tracked.CatalogErr)
	node := sentinelNode(tracked.Prefix, "NeverReadySentinel")
	f.store.AddBatch([]*graph.Node{node}, nil)
	f.lc.EnableDeferredRepositoryCleanup()
	out, err := f.lc.Untrack(t.Context(), root)
	require.NoError(t, err)
	require.True(t, out.Pending)
	drainedCleanupState(t, f.lc, tracked.GraphID)

	// When lifecycle shutdown cancels and joins the runtime before ready.
	require.NoError(t, f.lc.Close())
	f.lc.MarkRepositoryCleanupReady()

	// Then neither cancellation nor a late ready signal purges durable work.
	require.NotNil(t, f.store.GetNode(node.ID))
	entries, err := f.catalog.ListCleanupEntries(t.Context())
	require.NoError(t, err)
	require.NotEmpty(t, entries)
	_, found, err := f.catalog.GetDedicatedGraph(t.Context(), tracked.GraphID)
	require.NoError(t, err)
	require.True(t, found)
}

func TestRepositoryCleanupRuntimeDoesNotReplayBeforeReady(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		// Given actionable cleanup and a journal entry that would report a
		// hard replay error, the daemon is still waiting for readiness.
		core, logs := observer.New(zap.WarnLevel)
		f := newLifecycleFixture(t, zap.New(core))
		t.Cleanup(f.close)
		require.NoError(t, f.catalog.UpsertCleanupEntry(t.Context(), store_sqlite.CleanupEntry{
			CleanupID: "broken-ready-probe", OpaqueTargetIDs: "{not json", Reason: "x", Phase: store_sqlite.CleanupPhasePending,
		}))
		f.lc.EnableDeferredRepositoryCleanup()
		runtime, err := f.lc.repositoryCleanupRuntime()
		require.NoError(t, err)
		state := &repositoryCleanupState{graphID: "ready-probe", initialized: true,
			attemptDone: make(chan struct{}), waitReady: make(chan struct{})}
		close(state.attemptDone)
		close(state.waitReady)
		runtime.mu.Lock()
		runtime.states[state.graphID] = state
		runtime.mu.Unlock()

		// When a drain wake is consumed before readiness, let all owned
		// goroutines reach their next blocked state, then cancel and join.
		runtime.signal()
		synctest.Wait()
		f.lc.closeRepositoryCleanup()

		// Then it never replays SQL or starts the one-second retry loop.
		require.Empty(t, logs.FilterMessage("repository cleanup remains pending").All())
		entry, found, err := f.catalog.GetCleanupEntry(t.Context(), "broken-ready-probe")
		require.NoError(t, err)
		require.True(t, found)
		require.Equal(t, store_sqlite.CleanupPhasePending, entry.Phase)
	})
}

func TestRepositoryCleanupReadyBarrierCoversEveryGraphSaga(t *testing.T) {
	for _, plan := range []string{"forget_checkout", "retire_dedicated_graph", "retire_primary_closure", "forget_family"} {
		t.Run(plan, func(t *testing.T) {
			// Given a family and a daemon release barrier, retain real payload
			// under whichever graph this saga plan is authorized to release.
			f := newFamilyFixture(t, "ready-"+plan)
			t.Cleanup(f.close)
			ctx := t.Context()
			graphID, prefix := f.primaryGraph, f.mainPrefix
			checkout := f.checkoutOf(prefix)
			if plan == "retire_dedicated_graph" {
				tracked, err := f.lc.Register(ctx, config.RepoEntry{Path: f.worktree}, TrackSourceCLI)
				require.NoError(t, err)
				require.NoError(t, tracked.CatalogErr)
				graphID, prefix = tracked.GraphID, tracked.Prefix
				checkout = f.checkoutOf(prefix)
			}
			node := sentinelNode(prefix, "SagaReadySentinel")
			f.store.AddBatch([]*graph.Node{node}, nil)
			checkouts, err := f.catalog.ListCheckouts(ctx, f.familyID)
			require.NoError(t, err)
			for _, row := range checkouts {
				_, err = f.lc.rec.RevokeTrackingIntents(ctx, row.CheckoutID)
				require.NoError(t, err)
			}
			family, found, err := f.catalog.GetRepositoryFamily(ctx, f.familyID)
			require.NoError(t, err)
			require.True(t, found)
			f.lc.EnableDeferredRepositoryCleanup()

			// When each fixed saga plan reaches the shared graph-release hook.
			switch plan {
			case "forget_checkout":
				err = f.lc.rec.ForgetCheckout(ctx, checkout.CheckoutID, checkout.Incarnation)
			case "retire_dedicated_graph":
				err = f.lc.rec.RetireDedicatedGraph(ctx, graphID)
			case "retire_primary_closure":
				err = f.lc.rec.RetirePrimaryClosure(ctx, graphID, family.PrimaryEpoch)
			case "forget_family":
				err = f.lc.rec.ForgetFamily(ctx, f.familyID)
			}
			require.True(t, onlyRepositoryCleanupPending(err), "saga release error: %v", err)
			drainedCleanupState(t, f.lc, graphID)
			require.True(t, onlyRepositoryCleanupPending(f.lc.rec.Resume(ctx)))

			// Then no later cheap phase deletes the graph or its payload.
			require.NotNil(t, f.store.GetNode(node.ID))
			_, found, err = f.catalog.GetDedicatedGraph(ctx, graphID)
			require.NoError(t, err)
			require.True(t, found)
			_, found, err = f.catalog.GetCheckout(ctx, checkout.CheckoutID)
			require.NoError(t, err)
			require.True(t, found)
		})
	}
}

func TestRepositoryCleanupReadyBarrierKeepsGraphlessAndGoneReleasesAsNoOps(t *testing.T) {
	// Given a graphless automatic checkout and an already-absent graph.
	f := newFamilyFixture(t, "ready-no-op")
	t.Cleanup(f.close)
	f.lc.EnableDeferredRepositoryCleanup()
	before, err := f.catalog.ListCleanupEntries(t.Context())
	require.NoError(t, err)
	require.Empty(t, before)

	// When cleanup only withdraws that automatic checkout or the absent graph.
	require.NoError(t, f.lc.rec.ForgetCheckout(t.Context(), f.automatic.CheckoutID, f.automatic.Incarnation))
	require.NoError(t, f.lc.rec.RetireDedicatedGraph(t.Context(), "already-gone"))

	// Then neither no-op waits for readiness nor touches the healthy primary.
	after, err := f.catalog.ListCleanupEntries(t.Context())
	require.NoError(t, err)
	require.Empty(t, after)
	_, found, err := f.catalog.GetDedicatedGraph(t.Context(), f.primaryGraph)
	require.NoError(t, err)
	require.True(t, found)
	require.False(t, f.lc.RepositoryAdmissionClosed(f.mainPrefix))
}

func TestRepositoryCleanupRestartDefersInterruptedPhysicalRelease(t *testing.T) {
	for _, window := range []string{"generation_retirement", "prefix_purge_committed"} {
		t.Run(window, func(t *testing.T) {
			// Given a release journal whose previous process committed only
			// part of generation retirement, or all of the prefix purge.
			f := newLifecycleFixture(t)
			t.Cleanup(f.close)
			ctx := t.Context()
			root := f.gitRepo("interrupted-" + window)
			tracked, err := f.lc.Register(ctx, config.RepoEntry{Path: root, Name: "interrupted-" + window}, TrackSourceCLI)
			require.NoError(t, err)
			require.NoError(t, tracked.CatalogErr)
			node := sentinelNode(tracked.Prefix, "InterruptedReadySentinel")
			f.store.AddBatch([]*graph.Node{node}, nil)
			generationID, handle, err := f.store.BeginPayloadGeneration(ctx, store_sqlite.PayloadGenerationRequest{
				OwnerKind: checkoutLayerOwnerKind, GraphID: tracked.GraphID, CheckoutID: tracked.CheckoutID,
				GenerationKind: "commit", LayerID: "interrupted-layer", TreeOID: "interrupted-tree", ConfigHash: "interrupted-config",
			})
			require.NoError(t, err)
			handle.AddBatch([]*graph.Node{sentinelNode(tracked.Prefix, "InterruptedPositiveSentinel")}, nil)
			require.NoError(t, f.store.PublishPayloadGeneration(ctx, generationID, 1))
			pin := f.lc.ViewLeases().AcquireBaseCorpus(tracked.Prefix)
			t.Cleanup(pin.Release)
			require.True(t, pin.OwnerPinned())
			out, err := f.lc.Untrack(ctx, root)
			require.NoError(t, err)
			require.True(t, out.Pending)
			state := cleanupReadyState(t, f.lc, tracked.GraphID)
			f.lc.closeRepositoryCleanup()
			pin.Release()
			<-state.owner.Done()
			require.NoError(t, f.catalog.ReleaseRepositoryCleanupPublication(ctx, state.identity))
			switch window {
			case "generation_retirement":
				require.ErrorIs(t, f.store.RetirePayloadGenerationSlice(ctx, generationID, nil, time.Nanosecond), store_sqlite.ErrPayloadSweepBudgetExhausted)
				generation, found, err := f.catalog.GetViewGeneration(ctx, generationID)
				require.NoError(t, err)
				require.True(t, found)
				require.Equal(t, store_sqlite.ViewGenerationRetiring, generation.State)
			case "prefix_purge_committed":
				require.NoError(t, f.store.RetirePayloadGeneration(ctx, generationID, nil))
				require.NoError(t, f.store.PurgeRepo(tracked.Prefix))
				require.Nil(t, f.store.GetNode(node.ID))
			}
			_, found, err := f.catalog.GetDedicatedGraph(ctx, tracked.GraphID)
			require.NoError(t, err)
			require.True(t, found, "the release journal has not committed graph deletion")

			// When a fresh daemon-mode lifecycle resumes that exact journal,
			// neither physical entry point runs before its new readiness signal.
			f.restart()
			observer := observeCleanupReady(t, f)
			payload := &cleanupReadyPayload{Store: f.store}
			f.mi.graph = payload
			f.lc.EnableDeferredRepositoryCleanup()
			f.lc.EnableDeferredSeedRetirements()
			require.NoError(t, f.lc.Seed(ctx))
			state = drainedCleanupState(t, f.lc, tracked.GraphID)
			retired, purged := payload.calls()
			require.Empty(t, retired)
			require.Empty(t, purged)
			_, found, err = f.catalog.GetDedicatedGraph(ctx, tracked.GraphID)
			require.NoError(t, err)
			require.True(t, found)

			// Then post-ready replay completes real physical cleanup before
			// deleting the checkout, graph, and final journal entries.
			f.lc.MarkRepositoryCleanupReady()
			awaitCleanupReadyFinalization(t, observer, state)
			require.Nil(t, f.store.GetNode(node.ID))
			_, found, err = f.catalog.GetViewGeneration(ctx, generationID)
			require.NoError(t, err)
			require.False(t, found)
			_, found, err = f.catalog.GetCheckout(ctx, tracked.CheckoutID)
			require.NoError(t, err)
			require.False(t, found)
			entries, err := f.catalog.ListCleanupEntries(ctx)
			require.NoError(t, err)
			require.Empty(t, entries)
		})
	}
}
