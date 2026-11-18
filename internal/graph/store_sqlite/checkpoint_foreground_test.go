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

// Done is evaluated by the reservation's join select before writeMu or SQL.
// Park that evaluation so the test can inspect the join without a timing race.
// A no-join implementation instead reaches Done in SQL with writeMu held.
type checkpointJoinProbe struct {
	context.Context
	once    sync.Once
	entered chan struct{}
	proceed chan struct{}
}

func (p *checkpointJoinProbe) Done() <-chan struct{} {
	p.once.Do(func() {
		close(p.entered)
		select {
		case <-p.proceed:
		case <-p.Context.Done():
		}
	})
	return p.Context.Done()
}

func TestForegroundCheckpointJoinsInternalCheckpoint(t *testing.T) {
	for _, background := range []string{"passive", "reclaim"} {
		t.Run(background, func(t *testing.T) {
			s, db := foregroundCheckpointFixture(t)
			s.BeginBulkLoad()
			exit, err := s.flushBulkLocked()
			require.NoError(t, err)
			require.True(t, exit.durabilityCheckpoint)
			require.Nil(t, s.bulkConn, "durability runs after releasing the bulk pin")
			growWAL(t, s, 1)
			synctest.Test(t, func(t *testing.T) {
				s.stopCheckpoint = nil
				entered, release := stallReclaimCheckpointSync(t, db)
				ctx, cancel := context.WithTimeout(t.Context(), checkpointWaitTimeout)
				probe := &checkpointJoinProbe{Context: ctx, entered: make(chan struct{}), proceed: make(chan struct{})}
				var releaseOnce, proceedOnce sync.Once
				unpark := func() { releaseOnce.Do(func() { close(release) }) }
				proceed := func() { proceedOnce.Do(func() { close(probe.proceed) }) }
				backgroundDone, foregroundDone := make(chan struct{}), make(chan struct{})
				backgroundResult := make(chan checkpointAttemptResult, 1)
				foregroundResult := make(chan error, 1)
				t.Cleanup(func() {
					cancel()
					proceed()
					unpark()
					waitCheckpointSignal(t, backgroundDone)
					waitCheckpointSignal(t, foregroundDone)
				})
				attempt, err := s.beginCheckpointAttempt(checkpointIgnoresCycle, background == "reclaim")
				require.NoError(t, err)
				go func() {
					defer close(backgroundDone)
					defer s.finishBackgroundCheckpointAttempt(attempt)
					var err error
					if background == "reclaim" {
						_, err = s.pacedPassive(attempt.ctx, db, attempt)
					} else {
						_, err = checkpointWALOnceOn(attempt.ctx, db, "PASSIVE")
					}
					backgroundResult <- checkpointAttemptResult{complete: err == nil, cause: context.Cause(attempt.ctx)}
				}()
				waitCheckpointSignal(t, entered)
				go func() {
					defer close(foregroundDone)
					// Same checkpoint path finishBulkWindowExit calls, with a
					// deadline-bearing context that exposes the join select.
					foregroundResult <- s.checkpointBulkWALContext(probe)
				}()
				waitCheckpointSignal(t, probe.entered)
				require.NoError(t, attempt.ctx.Err(), "durability must not cancel the parked pass")
				locked := s.writeMu.TryLock()
				if locked {
					s.writeMu.Unlock()
				}
				require.True(t, locked, "join must precede writeMu and checkpoint SQL")
				proceed()
				synctest.Wait()
				select {
				case <-foregroundDone:
					t.Fatal("durability returned before the parked SQLite call joined")
				default:
				}
				unpark()
				waitCheckpointSignal(t, backgroundDone)
				waitCheckpointSignal(t, foregroundDone)
				out := <-backgroundResult
				require.True(t, out.complete, "parked pass must finish normally")
				require.NoError(t, out.cause, "context must remain live until the pass returns")
				require.NoError(t, <-foregroundResult)
				require.Zero(t, walFileSize(s.dbPath+"-wal"))
				var count int
				require.NoError(t, s.db.QueryRowContext(ctx, `SELECT count(*) FROM wal_churn`).Scan(&count))
				require.Equal(t, 4000, count)
				growWAL(t, s, 1)
				p := passiveCheckpointer{store: s, db: db}
				complete, retry := p.run(false)
				require.True(t, complete)
				require.False(t, retry)
			})
		})
	}
}

func TestDeadlineCheckpointLeavesBulkOverrideUncancelled(t *testing.T) {
	for _, caller := range []string{"explicit", "residue"} {
		t.Run(caller, func(t *testing.T) {
			s, _ := foregroundCheckpointFixture(t)
			s.BeginBulkLoad()
			s.backgroundCheckpoint.generationLease = 1
			t.Cleanup(func() { s.backgroundCheckpoint.generationLease = 0 })
			attempt, err := s.beginCheckpointAttempt(checkpointOverridesLeaseAndCycle, true)
			require.NoError(t, err)
			defer s.finishBackgroundCheckpointAttempt(attempt)
			ctx, cancel := context.WithTimeout(t.Context(), time.Second)
			defer cancel()
			if caller == "explicit" {
				err = s.runMaintenance(ctx, maintenanceCheckpoint, false, s.checkpointWALWithContext)
			} else {
				err = s.drainWALResidue(ctx)
			}
			require.NoError(t, attempt.ctx.Err(), "bulk ceiling reclaim must remain live")
			require.ErrorIs(t, err, errWALCheckpointDeferredBulk)
		})
	}
}

func TestDeadlineCheckpointLeavesPressureAndCopyUncancelled(t *testing.T) {
	for _, copying := range []bool{false, true} {
		t.Run(map[bool]string{false: "pressure", true: "copying"}[copying], func(t *testing.T) {
			s, _ := foregroundCheckpointFixture(t)
			policy := checkpointIgnoresCycle // pressure and hard-cap policy
			if copying {
				policy = checkpointYieldsToCycle
			}
			attempt, err := s.beginCheckpointAttempt(policy, true)
			require.NoError(t, err)
			defer s.finishBackgroundCheckpointAttempt(attempt)
			attempt.copy.copying.Store(copying)
			ctx, cancel := context.WithTimeout(t.Context(), time.Second)
			defer cancel()
			err = s.AtGeneration(701).checkpointWALWithContext(ctx)
			require.NoError(t, attempt.ctx.Err(), "deadline callers must not withdraw a pressure/copy pass")
			require.NoError(t, err)
		})
	}
}

func TestForegroundCheckpointCancellationReleasesReservation(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		s := &Store{storeCore: &storeCore{}}
		attempt, err := s.beginBackgroundCheckpointAttempt(checkpointIgnoresCycle)
		require.NoError(t, err)
		defer s.finishBackgroundCheckpointAttempt(attempt)
		ctx, cancel := context.WithTimeout(t.Context(), checkpointWaitTimeout)
		defer cancel()
		result := make(chan error, 1)
		go func() { result <- s.checkpointBulkWALContext(ctx) }()
		synctest.Wait()
		require.True(t, s.backgroundCheckpoint.foregroundReserved.Load())
		cancel()
		synctest.Wait()
		require.ErrorIs(t, <-result, context.Canceled)
		require.NoError(t, attempt.ctx.Err())
		s.finishBackgroundCheckpointAttempt(attempt)
		next, err := s.beginBackgroundCheckpointAttempt(checkpointIgnoresCycle)
		require.NoError(t, err, "cancelled durability waiter must release reservation")
		s.finishBackgroundCheckpointAttempt(next)
	})
}

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
		ctx, cancel := context.WithTimeout(t.Context(), checkpointWaitTimeout)
		defer cancel()
		results := make(chan error, 2)
		go func() { results <- s.checkpointBulkWALContext(ctx) }()
		synctest.Wait()
		// No active attempt: the reservation alone must refuse all policies,
		// including a non-overriding policy with a generation lease present.
		for _, lease := range []generationBulkCheckpointLease{0, 1} {
			s.backgroundCheckpoint.generationLease = lease
			for _, policy := range []checkpointCyclePolicy{checkpointYieldsToCycle, checkpointIgnoresCycle, checkpointOverridesLease, checkpointOverridesLeaseAndCycle} {
				attempt, err := s.beginCheckpointAttempt(policy, true)
				if attempt != nil {
					s.finishBackgroundCheckpointAttempt(attempt)
				}
				require.ErrorIs(t, err, errWALCheckpointInFlight)
			}
		}
		s.backgroundCheckpoint.generationLease = 0
		go func() { results <- s.AtGeneration(702).checkpointBulkWALContext(ctx) }()
		synctest.Wait()
		require.EqualValues(t, 1, s.writeMu.waiting())
		s.writeMu.Unlock()
		locked = false
		synctest.Wait()
		require.ErrorIs(t, <-results, errWALCheckpointDeferredBulk)
		require.ErrorIs(t, <-results, errWALCheckpointDeferredBulk)
		attempt, err := s.beginBackgroundCheckpointAttempt(checkpointIgnoresCycle)
		require.NoError(t, err, "error paths must release reservation")
		s.finishBackgroundCheckpointAttempt(attempt)
	})
}

func TestForegroundCheckpointPreservesExternalCheckpointBusy(t *testing.T) {
	s, db := foregroundCheckpointFixture(t)
	entered, release := stallReclaimCheckpointSync(t, db)
	done := make(chan struct{})
	ctx, cancel := context.WithTimeout(t.Context(), checkpointWaitTimeout)
	var once sync.Once
	unpark := func() { once.Do(func() { close(release) }) }
	t.Cleanup(func() { cancel(); unpark(); waitCheckpointSignal(t, done) })
	go func() {
		defer close(done)
		_, _ = checkpointWALOnceOn(ctx, db, "PASSIVE") // deliberately not store-owned
	}()
	waitCheckpointSignal(t, entered)
	result, err := s.checkpointWALWithContextResult(ctx)
	require.ErrorIs(t, err, errSQLiteCheckpointIncomplete)
	require.ErrorIs(t, err, errSQLiteBusyRetryExhausted)
	require.Equal(t, 1, result.Busy)
	require.Equal(t, -1, result.WALFrames)
	require.Equal(t, -1, result.CheckpointedFrames)
	require.NoError(t, ctx.Err(), "external lock owner must not be canceled")
	unpark()
	waitCheckpointSignal(t, done)
	require.NoError(t, s.checkpointBulkWALContext(ctx))
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
	s.BeginBulkLoad()
	require.NotNil(t, s.bulkConn)
	synctest.Test(t, func(t *testing.T) {
		ctx, cancel := context.WithTimeout(t.Context(), checkpointWaitTimeout)
		defer cancel()
		require.NoError(t, s.reserveForegroundCheckpoint(ctx))
		reserved := true
		defer func() {
			if reserved {
				s.releaseForegroundCheckpoint()
			}
		}()
		closeDone := make(chan struct{})
		closeResult := make(chan error, 1)
		go func() {
			defer close(closeDone)
			closeResult <- closeStore()
		}()
		synctest.Wait()
		require.Nil(t, s.bulkConn, "Close must release its bulk pin before checkpoint admission")
		require.EqualValues(t, 1, s.backgroundCheckpoint.foregroundGate.waiting(), "bulk Close must queue for the durability reservation")
		select {
		case <-closeDone:
			t.Fatal("Close bypassed the existing durability reservation")
		default:
		}
		s.releaseForegroundCheckpoint()
		reserved = false
		waitCheckpointSignal(t, closeDone)
		require.NoError(t, <-closeResult)
		select {
		case <-s.checkpointDone:
		default:
			t.Fatal("Close returned without joining checkpoint workers")
		}
	})
}

func TestQueuedForegroundCheckpointCancellationDoesNotReleaseOwner(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ctx, cancelOwner := context.WithTimeout(t.Context(), checkpointWaitTimeout)
		defer cancelOwner()
		s := &Store{storeCore: &storeCore{bulkConn: &sql.Conn{}}}
		s.writeMu.Lock()
		locked := true
		defer func() {
			if locked {
				s.writeMu.Unlock()
			}
		}()
		first := make(chan error, 1)
		go func() { first <- s.checkpointBulkWALContext(ctx) }()
		synctest.Wait()
		ctx, cancel := context.WithTimeout(t.Context(), checkpointWaitTimeout)
		defer cancel()
		second := make(chan error, 1)
		go func() { second <- s.checkpointBulkWALContext(ctx) }()
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

// In-memory residue uses the same deadline writer gate, not a durability
// reservation. Cancelling this waiter must leave the active owner untouched.
func TestCancelledResidueWaiterLeavesBackgroundAttemptLive(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		s := &Store{storeCore: &storeCore{}}
		attempt, err := s.beginBackgroundCheckpointAttempt(checkpointIgnoresCycle)
		require.NoError(t, err)
		defer s.finishBackgroundCheckpointAttempt(attempt)
		s.writeMu.Lock()
		defer s.writeMu.Unlock()
		ctx, cancel := context.WithTimeout(t.Context(), checkpointWaitTimeout)
		defer cancel()
		result := make(chan error, 1)
		go func() { result <- s.drainWALResidue(ctx) }()
		synctest.Wait()
		require.EqualValues(t, 1, s.writeMu.waiting())
		cancel()
		synctest.Wait()
		require.ErrorIs(t, <-result, context.Canceled)
		require.NoError(t, attempt.ctx.Err())
		require.False(t, s.backgroundCheckpoint.foregroundReserved.Load())
	})
}

// Exercise the actual paced PASSIVE with an announced writer that cannot end
// until durability completes. Fake time exposes both pause and budget waits.
func TestBulkDurabilityResumesPausableCheckpoint(t *testing.T) {
	for _, phase := range []string{"pause", "budget"} {
		t.Run(phase, func(t *testing.T) {
			s, db := foregroundCheckpointFixture(t)
			setCopyBudget(t, 1<<20, 0, 0)
			t.Setenv("GORTEX_SQLITE_WAL_COPY_BUDGET_MB", "1")
			synctest.Test(t, func(t *testing.T) {
				ctx, cancel := context.WithTimeout(t.Context(), time.Second)
				defer cancel()
				// The fixture's workers are already joined. The owned watcher
				// and copy use only this bubble's attempt/context channels.
				s.stopCheckpoint = nil
				var attempt *backgroundCheckpointAttempt
				var announceOnce sync.Once
				var unannounce func()
				defer func() {
					if unannounce != nil {
						unannounce()
					}
				}()
				s.SetBuildLaneBusy(func() bool {
					// Announce on the first paced page, after admission while
					// idle, so pacedPassive cannot reject before it starts.
					if phase == "pause" && attempt != nil && attempt.copy.copying.Load() {
						announceOnce.Do(func() { unannounce = s.AnnounceWrite() })
					}
					return false
				})
				var err error
				attempt, err = s.beginCheckpointAttempt(checkpointYieldsToCycle, true)
				require.NoError(t, err)
				defer s.finishBackgroundCheckpointAttempt(attempt)
				copyCtx, cancelCopy := context.WithTimeout(attempt.ctx, time.Second)
				defer cancelCopy()
				attempt.copy.pausable.Store(true)
				attempt.copy.walPath = s.dbPath + "-wal"
				s.walCopy.sawBusy(time.Now())
				copyDone := make(chan struct{})
				copyResult := make(chan checkpointAttemptResult, 1)
				go func() {
					defer close(copyDone)
					defer s.finishBackgroundCheckpointAttempt(attempt)
					_, err := s.pacedPassive(copyCtx, db, attempt)
					copyResult <- checkpointAttemptResult{complete: err == nil, cause: context.Cause(copyCtx)}
				}()
				synctest.Wait()
				require.True(t, attempt.copy.copying.Load(), "real copy must be parked in its page pacer")
				if phase == "budget" {
					unannounce = s.AnnounceWrite()
				}
				require.True(t, s.buildLaneBusy(), "the announcement, not the lane predicate, blocks the copy")
				started := time.Now()
				result := make(chan error, 1)
				go func() { result <- s.checkpointBulkWALContext(ctx) }()
				require.NoError(t, <-result)
				waitCheckpointSignal(t, copyDone)
				out := <-copyResult
				require.True(t, out.complete)
				require.NoError(t, out.cause, "durability must finish the pass, not interrupt it")
				require.Less(t, time.Since(started), 50*time.Millisecond, "durability waited for edit/pause/budget limits")
				if phase == "pause" {
					require.Positive(t, attempt.copy.pauses)
				}
				require.Less(t, attempt.copy.budgetNs, 50*time.Millisecond)
				require.Zero(t, walFileSize(s.dbPath+"-wal"))
			})
		})
	}
}
