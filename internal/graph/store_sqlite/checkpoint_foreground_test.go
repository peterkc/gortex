package store_sqlite

import (
	"context"
	"database/sql"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"github.com/stretchr/testify/require"
)

// Fixture owns the checkpoint frontier; no timer drives the interleaving.
func foregroundCheckpointFixture(t *testing.T) (*Store, *sql.DB) {
	t.Helper()
	t.Setenv("GORTEX_SQLITE_LAZY_INDEXES", "off")
	s, db := finalBackfillFixture(t)
	s.stopCheckpointLoop()
	s.stopMaintenanceLane()
	stop := make(chan struct{})
	s.stopCheckpoint = stop
	t.Cleanup(func() { close(stop) })
	s.busyRetryTimeout = 20 * time.Millisecond
	return s, db
}

func TestForegroundCheckpointJoinsInternalCheckpoint(t *testing.T) {
	for _, foreground := range []string{"bulk", "explicit", "residue"} {
		for _, background := range []string{"passive", "reclaim"} {
			t.Run(foreground+"/"+background, func(t *testing.T) {
				s, db := foregroundCheckpointFixture(t)
				var exit bulkWindowExit
				if foreground == "bulk" {
					s.BeginBulkLoad()
					var err error
					exit, err = s.flushBulkLocked()
					require.NoError(t, err)
					require.True(t, exit.durabilityCheckpoint)
				}
				growWAL(t, s, 1)
				entered, release := stallReclaimCheckpointSync(t, db)
				var once sync.Once
				unpark := func() { once.Do(func() { close(release) }) }
				backgroundDone := make(chan struct{})
				foregroundDone := make(chan struct{})
				result := make(chan error, 1)
				ctx, cancel := context.WithCancel(t.Context())
				t.Cleanup(func() {
					cancel()
					unpark()
					waitCheckpointSignal(t, backgroundDone)
					waitCheckpointSignal(t, foregroundDone)
				})
				go func() {
					defer close(backgroundDone)
					if background == "passive" {
						p := passiveCheckpointer{store: s, db: db}
						_, _ = p.run(false)
						return
					}
					_ = s.reclaimWALOnce(walReclaimConfig{thresholdBytes: 1, drainDeadline: time.Second, truncateBudget: walReclaimTruncateBudget}, db, s.dbPath+"-wal")
				}()
				waitCheckpointSignal(t, entered)
				s.backgroundCheckpoint.mu.Lock()
				attempt := s.backgroundCheckpoint.active
				s.backgroundCheckpoint.mu.Unlock()
				require.NotNil(t, attempt)
				go func() {
					defer close(foregroundDone)
					switch foreground {
					case "bulk":
						result <- s.finishBulkWindowExit(exit)
					case "explicit":
						// Borrowed handles share the reservation; the maintenance
						// token may already be held by a foreground caller.
						handle := s.AtGeneration(701)
						result <- handle.runMaintenance(ctx, maintenanceCheckpoint, false, handle.checkpointWALWithContext)
					case "residue":
						result <- s.drainWALResidue(ctx)
					}
				}()
				select {
				case <-attempt.ctx.Done():
				case err := <-result:
					t.Fatalf("foreground returned before cancel/join of its internal checkpoint lock owner: %v", err)
				case <-time.After(checkpointWaitTimeout):
					t.Fatal("foreground did not cancel its internal checkpoint lock owner")
				}
				require.True(t, s.writeMu.TryLock(), "joining must not hold writeMu")
				s.writeMu.Unlock()
				select {
				case <-foregroundDone:
					t.Fatal("foreground returned before the parked SQLite call joined")
				default:
				}
				unpark()
				waitCheckpointSignal(t, backgroundDone)
				waitCheckpointSignal(t, foregroundDone)
				require.NoError(t, <-result)
				require.Zero(t, walFileSize(s.dbPath+"-wal"))
				var count int
				require.NoError(t, s.db.QueryRow(`SELECT count(*) FROM wal_churn`).Scan(&count))
				require.Equal(t, 4000, count)
				// A later private-pool PASSIVE must run normally, not remain
				// excluded by the completed foreground checkpoint.
				growWAL(t, s, 1)
				p := passiveCheckpointer{store: s, db: db}
				complete, retry := p.run(false)
				require.True(t, complete)
				require.False(t, retry)
			})
		}
	}
}

func TestForegroundCheckpointCancellationReleasesReservation(t *testing.T) {
	s, db := foregroundCheckpointFixture(t)
	entered, release := stallReclaimCheckpointSync(t, db)
	attempt, err := s.beginBackgroundCheckpointAttempt(checkpointIgnoresCycle)
	require.NoError(t, err)
	backgroundDone, foregroundDone := make(chan struct{}), make(chan struct{})
	ctx, cancel := context.WithCancel(t.Context())
	result := make(chan error, 1)
	var once sync.Once
	unpark := func() { once.Do(func() { close(release) }) }
	t.Cleanup(func() {
		cancel()
		unpark()
		waitCheckpointSignal(t, backgroundDone)
		waitCheckpointSignal(t, foregroundDone)
	})
	go func() {
		defer close(backgroundDone)
		_, _ = checkpointWALOnceOn(attempt.ctx, db, "PASSIVE")
		s.finishBackgroundCheckpointAttempt(attempt)
	}()
	waitCheckpointSignal(t, entered)
	go func() {
		defer close(foregroundDone)
		result <- s.checkpointWALWithContext(ctx)
	}()
	select {
	case <-attempt.ctx.Done():
	case err := <-result:
		t.Fatalf("foreground returned without withdrawing internal checkpoint: %v", err)
	case <-time.After(checkpointWaitTimeout):
		t.Fatal("foreground did not withdraw internal checkpoint")
	}
	cancel()
	waitCheckpointSignal(t, foregroundDone)
	require.ErrorIs(t, <-result, context.Canceled)
	unpark()
	waitCheckpointSignal(t, backgroundDone)
	require.NoError(t, s.checkpointWALWithContext(t.Context()))
	p := passiveCheckpointer{store: s, db: db}
	complete, retry := p.run(false)
	require.True(t, complete)
	require.False(t, retry)
}

// A pinned bulk writer rejects checkpoint SQL. Holding writeMu lets synctest
// observe queued foreground calls without using a timer or a fake SQLite call.
func TestForegroundCheckpointSerializesAndRefusesEveryBackgroundPolicy(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		s := &Store{storeCore: &storeCore{bulkConn: &sql.Conn{}}}
		s.writeMu.Lock()
		locked := true
		defer func() {
			if locked {
				s.writeMu.Unlock()
			}
		}()
		results := make(chan error, 2)
		go func() { results <- s.checkpointWALWithContext(t.Context()) }()
		synctest.Wait()
		for _, policy := range []checkpointCyclePolicy{checkpointYieldsToCycle, checkpointIgnoresCycle, checkpointOverridesLease, checkpointOverridesLeaseAndCycle} {
			attempt, err := s.beginCheckpointAttempt(policy, true)
			if attempt != nil {
				s.finishBackgroundCheckpointAttempt(attempt)
			}
			require.ErrorIs(t, err, errWALCheckpointInFlight)
		}
		go func() { results <- s.AtGeneration(702).checkpointWALWithContext(t.Context()) }()
		synctest.Wait()
		require.EqualValues(t, 1, s.writeMu.waiting(), "only one foreground checkpoint may wait for the writer")
		s.writeMu.Unlock()
		locked = false
		synctest.Wait()
		require.ErrorIs(t, <-results, errWALCheckpointDeferredBulk)
		require.ErrorIs(t, <-results, errWALCheckpointDeferredBulk)
		attempt, err := s.beginBackgroundCheckpointAttempt(checkpointIgnoresCycle)
		require.NoError(t, err, "error paths must release the reservation")
		s.finishBackgroundCheckpointAttempt(attempt)
	})
}

func TestForegroundCheckpointPreservesExternalCheckpointBusy(t *testing.T) {
	s, db := foregroundCheckpointFixture(t)
	entered, release := stallReclaimCheckpointSync(t, db)
	done := make(chan struct{})
	ctx, cancel := context.WithCancel(t.Context())
	var once sync.Once
	unpark := func() { once.Do(func() { close(release) }) }
	t.Cleanup(func() { cancel(); unpark(); waitCheckpointSignal(t, done) })
	go func() {
		defer close(done)
		_, _ = checkpointWALOnceOn(ctx, db, "PASSIVE") // deliberately not store-owned
	}()
	waitCheckpointSignal(t, entered)
	result, err := s.checkpointWALWithContextResult(t.Context())
	require.ErrorIs(t, err, errSQLiteCheckpointIncomplete)
	require.ErrorIs(t, err, errSQLiteBusyRetryExhausted)
	require.Equal(t, 1, result.Busy)
	require.Equal(t, -1, result.WALFrames)
	require.Equal(t, -1, result.CheckpointedFrames)
	require.NoError(t, ctx.Err(), "external lock owner must not be canceled")
	unpark()
	waitCheckpointSignal(t, done)
	require.NoError(t, s.checkpointWALWithContext(t.Context()))
}

func TestCloseJoinsWhileForegroundCheckpointIsReserved(t *testing.T) {
	t.Setenv("GORTEX_SQLITE_CLOSE_CHECKPOINT", "full")
	t.Setenv("GORTEX_SQLITE_WAL_RECLAIM_MB", "0")
	t.Setenv("GORTEX_SQLITE_LAZY_INDEXES", "off")
	s, _ := openWALReclaimStore(t)
	var closeOnce sync.Once
	var closeErr error
	closeStore := func() error {
		closeOnce.Do(func() { closeErr = s.Close() })
		return closeErr
	}
	t.Cleanup(func() { _ = closeStore() })
	seedWALChurnTable(t, s)
	entered, release := stallReclaimCheckpointSync(t, s.writerDB)
	foregroundDone, closeDone := make(chan struct{}), make(chan struct{})
	foregroundResult, closeResult := make(chan error, 1), make(chan error, 1)
	var once sync.Once
	unpark := func() { once.Do(func() { close(release) }) }
	t.Cleanup(func() {
		unpark()
		waitCheckpointSignal(t, foregroundDone)
		waitCheckpointSignal(t, closeDone)
	})
	go func() {
		defer close(foregroundDone)
		foregroundResult <- s.checkpointWALWithContext(t.Context())
	}()
	waitCheckpointSignal(t, entered)
	go func() {
		defer close(closeDone)
		closeResult <- closeStore()
	}()
	// Close has begun joining its actual workers while foreground SQL is
	// still parked and holds both the reservation and the writer gate.
	waitCheckpointSignal(t, s.stopCheckpoint)
	select {
	case <-closeDone:
		t.Fatal("Close returned before the foreground checkpoint released its writer")
	default:
	}
	unpark()
	waitCheckpointSignal(t, foregroundDone)
	waitCheckpointSignal(t, closeDone)
	require.NoError(t, <-foregroundResult)
	require.NoError(t, <-closeResult)
	select {
	case <-s.checkpointDone:
	default:
		t.Fatal("Close returned without joining checkpoint workers")
	}
}

func TestQueuedForegroundCheckpointCancellationDoesNotReleaseOwner(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		s := &Store{storeCore: &storeCore{bulkConn: &sql.Conn{}}}
		s.writeMu.Lock()
		locked := true
		defer func() {
			if locked {
				s.writeMu.Unlock()
			}
		}()
		first := make(chan error, 1)
		go func() { first <- s.checkpointWALWithContext(t.Context()) }()
		synctest.Wait()
		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()
		second := make(chan error, 1)
		go func() { second <- s.checkpointWALWithContext(ctx) }()
		synctest.Wait()
		cancel()
		synctest.Wait()
		require.ErrorIs(t, <-second, context.Canceled)
		attempt, err := s.beginBackgroundCheckpointAttempt(checkpointIgnoresCycle)
		if attempt != nil {
			s.finishBackgroundCheckpointAttempt(attempt)
		}
		require.ErrorIs(t, err, errWALCheckpointInFlight, "a canceled waiter cannot release the current owner")
		s.writeMu.Unlock()
		locked = false
		synctest.Wait()
		require.ErrorIs(t, <-first, errWALCheckpointDeferredBulk)
	})
}
