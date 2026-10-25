package store_sqlite

import (
	"context"
	"database/sql"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func observeDelayedReclaimSync(t *testing.T, db *sql.DB, before func(int)) {
	t.Helper()
	conn, err := db.Conn(t.Context())
	require.NoError(t, err)
	key, ok := pausableConn(conn)
	require.NoError(t, conn.Close())
	require.True(t, ok)
	reclaimSyncStallState.Store(&reclaimSyncStall{tls: key, beforeSync: before})
	t.Cleanup(func() { reclaimSyncStallState.Store(nil) })
}

func TestReclaimOperationCancellationJoinsSlowCheckpoint(t *testing.T) {
	s, db := finalBackfillFixture(t)
	entered, release := stallReclaimCheckpointSync(t, db)
	operation, cancel := context.WithCancel(t.Context())
	writerCtx, stop := context.WithTimeout(operation, time.Second)
	defer stop()
	done := make(chan error, 1)
	go func() {
		s.writeMu.Lock()
		writer := newWALReclaimWriterCredit(s, time.Now())
		defer writer.release()
		_, err := writer.passive(writerCtx, operation, db, 25*time.Millisecond, false)
		done <- err
	}()
	select {
	case <-entered:
	case <-time.After(time.Second):
		close(release)
		t.Fatal("checkpoint never entered sync")
	}
	cancel()
	admission, end := context.WithTimeout(t.Context(), 100*time.Millisecond)
	err := s.writeMu.LockContext(admission)
	end()
	if err == nil {
		s.writeMu.Unlock()
	}
	select {
	case <-done:
		close(release)
		t.Fatal("SQL returned before the VFS sync ended")
	default:
	}
	close(release)
	require.NoError(t, err, "parent cancellation failed to release the foreground gate")
	require.ErrorIs(t, <-done, context.Canceled)
	require.True(t, s.writeMu.TryLock(), "operation retained its writer credit after joining SQL")
	s.writeMu.Unlock()
}

// A completed slow copy gets one final admission opportunity from the unused
// portion of the original writer allowance, not a fresh full hold budget.
func TestReclaimSlowCopyCanResetWithinRemainingWriterCredit(t *testing.T) {
	for _, kind := range []string{"pressure", "short"} {
		t.Run(kind, func(t *testing.T) {
			s, db := finalBackfillFixture(t)
			var syncs atomic.Int64
			observeDelayedReclaimSync(t, db, func(fileKind int) {
				if fileKind == vfsFileWAL {
					syncs.Add(1)
					time.Sleep(300 * time.Millisecond)
				}
			})
			ctx, cancel := context.WithTimeout(t.Context(), time.Second)
			defer cancel()
			res := &walReclaimResult{}
			started := time.Now()
			err := runFinalReclaimStep(ctx, kind, s, db, res)
			t.Logf("kind=%s joined_copy=%s max_writer_hold=%s reset=%t reason=%q err=%v", kind, time.Since(started), res.writerHold, res.openGate, res.reason, err)
			require.NoError(t, err)
			require.True(t, res.openGate, "joined durable copy never reached reset admission")
			require.Positive(t, syncs.Load(), "the real WAL sync was not delayed")
			require.LessOrEqual(t, res.writerHold, walReclaimPressureHold+testTimingSlack(10*time.Millisecond))
			require.True(t, s.writeMu.TryLock(), "reset retained writer ownership")
			s.writeMu.Unlock()
			var payload string
			require.NoError(t, s.db.QueryRow(`SELECT payload FROM wal_churn WHERE id = 1`).Scan(&payload))
			require.Equal(t, "before", payload)
		})
	}
}

func TestReclaimReadmissionSharesAggregateCredit(t *testing.T) {
	s, db := finalBackfillFixture(t)
	observeDelayedReclaimSync(t, db, func(kind int) {
		if kind == vfsFileWAL {
			time.Sleep(300 * time.Millisecond)
		}
	})
	operation, cancel := context.WithTimeout(t.Context(), time.Second)
	defer cancel()
	s.writeMu.Lock()
	writer := newWALReclaimWriterCredit(s, time.Now())
	defer writer.release()
	original, stop := context.WithTimeout(operation, 50*time.Millisecond)
	defer stop()
	_, err := writer.passive(original, operation, db, 25*time.Millisecond, false)
	require.NoError(t, err)
	require.True(t, writer.readmitted)
	require.GreaterOrEqual(t, writer.spent, 20*time.Millisecond)
	resetCtx := writer.resetContext(original)
	deadline, bounded := resetCtx.Deadline()
	require.True(t, bounded)
	require.LessOrEqual(t, time.Until(deadline), 50*time.Millisecond-writer.spent+time.Millisecond, "readmission renewed a full allowance")
	_, err = s.resetWALForReclaim(resetCtx)
	require.NoError(t, err)
	writer.release()
	require.LessOrEqual(t, writer.spent, 50*time.Millisecond+testTimingSlack(10*time.Millisecond), "writer slices exceeded their aggregate allowance")
}

func TestReclaimReadmissionDoesNotReviveCancellationOrSpentCredit(t *testing.T) {
	for _, kind := range []string{"spent", "cancel_cause", "writer_demand"} {
		t.Run(kind, func(t *testing.T) {
			s, db := finalBackfillFixture(t)
			operation, cancel := context.WithTimeout(t.Context(), time.Second)
			defer cancel()
			s.writeMu.Lock()
			writer := newWALReclaimWriterCredit(s, time.Now())
			defer writer.release()
			original, stop := context.WithTimeout(operation, 50*time.Millisecond)
			defer stop()
			original, cancelCause := context.WithCancelCause(original)
			defer cancelCause(context.Canceled)
			credit := 25 * time.Millisecond
			if kind == "spent" {
				credit = 50 * time.Millisecond
			}
			writer.yieldToWriters = kind == "writer_demand"
			releaseDemand := func() {}
			defer func() { releaseDemand() }()
			observeDelayedReclaimSync(t, db, func(fileKind int) {
				if fileKind != vfsFileWAL {
					return
				}
				if kind == "cancel_cause" {
					cancelCause(context.DeadlineExceeded)
				}
				time.Sleep(300 * time.Millisecond)
				if kind == "writer_demand" {
					releaseDemand = s.AnnounceWrite()
				}
			})
			_, err := writer.passive(original, operation, db, credit, false)
			want := error(context.DeadlineExceeded)
			switch kind {
			case "cancel_cause":
				want = context.Canceled
			case "writer_demand":
				want = errWALReclaimWriterWaiting
			}
			require.True(t, errors.Is(err, want), "readmission error %v, want %v", err, want)
			require.False(t, writer.held)
			require.False(t, writer.readmitted)
		})
	}
}

func TestReclaimReadmissionPreservesNonurgentWithdrawal(t *testing.T) {
	s, db := finalBackfillFixture(t)
	releaseDemand := func() {}
	defer func() { releaseDemand() }()
	observeDelayedReclaimSync(t, db, func(kind int) {
		if kind == vfsFileWAL {
			time.Sleep(300 * time.Millisecond)
			releaseDemand = s.AnnounceWrite()
		}
	})
	operation, cancel := context.WithTimeout(t.Context(), time.Second)
	defer cancel()
	res := &walReclaimResult{}
	done, err := s.reclaimWALResetHold(operation, walReclaimConfig{thresholdBytes: 1}, db, res, false, false)
	require.ErrorIs(t, err, errWALReclaimWriterWaiting)
	require.False(t, done)
	require.Equal(t, "writer_waiting", res.reason)
	require.True(t, s.writeMu.TryLock())
	s.writeMu.Unlock()
}
