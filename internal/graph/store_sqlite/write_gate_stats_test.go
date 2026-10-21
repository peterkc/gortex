package store_sqlite

import (
	"bytes"
	"context"
	"encoding/json"
	"log"
	"math"
	"strings"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Done is evaluated by LockContext's select after it observes the holder.
// This channel orders release after that observation without a sleep or poll.
type writeGateStatsWaitContext struct {
	context.Context
	started chan struct{}
}

func (c writeGateStatsWaitContext) Done() <-chan struct{} {
	close(c.started)
	return c.Context.Done()
}

func writeGateStatsHoldA(g *sqliteWriteGate, acquired, release, done chan struct{}) {
	g.Lock()
	close(acquired)
	<-release
	g.Unlock()
	close(done)
}

func writeGateStatsHoldB(s *Store, acquired, release chan struct{}, done chan error) {
	unlock, err := s.HoldWriteGate(context.Background())
	if err != nil {
		done <- err
		return
	}
	close(acquired)
	<-release
	unlock()
	done <- nil
}

func writeGateStatsWait(g *sqliteWriteGate, started chan struct{}, done chan error) {
	err := g.LockContext(writeGateStatsWaitContext{Context: context.Background(), started: started})
	if err == nil {
		g.Unlock()
	}
	done <- err
}

func TestWriteGateStatsCounters(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		store := &Store{storeCore: &storeCore{}}
		g := &store.writeMu
		acquiredA, releaseA, doneA := make(chan struct{}), make(chan struct{}), make(chan struct{})
		go writeGateStatsHoldA(g, acquiredA, releaseA, doneA)
		<-acquiredA
		require.False(t, g.TryLock())
		started := make(chan struct{})
		done := make(chan error, 1)
		go writeGateStatsWait(g, started, done)
		<-started
		synctest.Wait()
		<-time.NewTimer(2 * time.Millisecond).C
		close(releaseA)
		<-doneA
		require.NoError(t, <-done)

		acquiredB, releaseB := make(chan struct{}), make(chan struct{})
		doneB := make(chan error, 1)
		go writeGateStatsHoldB(store, acquiredB, releaseB, doneB)
		<-acquiredB
		started = make(chan struct{})
		go writeGateStatsWait(g, started, done)
		<-started
		synctest.Wait()
		<-time.NewTimer(20 * time.Millisecond).C
		close(releaseB)
		require.NoError(t, <-doneB)
		require.NoError(t, <-done)

		// Reproduce the observational gap during a token hand-off. The token
		// remains held, but the next waiter must not invent a holder name.
		acquiredA, releaseA, doneA = make(chan struct{}), make(chan struct{}), make(chan struct{})
		go writeGateStatsHoldA(g, acquiredA, releaseA, doneA)
		<-acquiredA
		holder := g.holder.Swap(nil)
		require.NotNil(t, holder)
		started = make(chan struct{})
		go writeGateStatsWait(g, started, done)
		<-started
		g.holder.Store(holder)
		synctest.Wait()
		<-time.NewTimer(5 * time.Millisecond).C
		close(releaseA)
		<-doneA
		require.NoError(t, <-done)

		snapshot := g.stats.snapshot("test")
		require.Len(t, snapshot.Sites, 4)
		require.Len(t, snapshot.Pairs, 3)
		var acquisitions, failures, holds, waits uint64
		for _, site := range snapshot.Sites {
			acquisitions += site.Acquisitions
			failures += site.TryLockFailures
			holds += site.Hold.Samples
			waits += site.Wait.Samples
			for _, timing := range []writeGateTiming{site.Wait, site.Hold} {
				assert.LessOrEqual(t, timing.P50UpperNS, timing.P95UpperNS)
				assert.LessOrEqual(t, timing.P95UpperNS, timing.MaxNS)
			}
			switch {
			case strings.Contains(site.Site, ".writeGateStatsHoldA "):
				assert.EqualValues(t, 2, site.Acquisitions)
				assert.EqualValues(t, 5*time.Millisecond, site.Hold.MaxNS)
			case strings.Contains(site.Site, ".writeGateStatsHoldB "):
				assert.EqualValues(t, 1, site.Acquisitions)
				assert.EqualValues(t, 20*time.Millisecond, site.Hold.MaxNS)
			case strings.Contains(site.Site, ".writeGateStatsWait "):
				assert.EqualValues(t, 3, site.Acquisitions)
				assert.EqualValues(t, 20*time.Millisecond, site.Wait.MaxNS)
			default:
				assert.EqualValues(t, 1, site.TryLockFailures)
			}
		}
		assert.EqualValues(t, 6, acquisitions)
		assert.EqualValues(t, 1, failures)
		assert.EqualValues(t, 6, holds)
		assert.EqualValues(t, 6, waits)
		for _, pair := range snapshot.Pairs {
			assert.Contains(t, pair.Waiter, ".writeGateStatsWait ")
			assert.EqualValues(t, 1, pair.Waits)
			switch {
			case strings.Contains(pair.Holder, ".writeGateStatsHoldA "):
				assert.EqualValues(t, 2*time.Millisecond, pair.WaitSumNS)
			case strings.Contains(pair.Holder, ".writeGateStatsHoldB "):
				assert.EqualValues(t, 20*time.Millisecond, pair.WaitSumNS)
			default:
				assert.Equal(t, "unknown", pair.Holder)
				assert.EqualValues(t, 5*time.Millisecond, pair.WaitSumNS)
			}
		}
	})
}

func TestWriteGateStatsHistogram(t *testing.T) {
	var histogram writeGateHistogram
	for i := range 100 {
		histogram.record(time.Duration(i+1) * time.Microsecond)
	}
	require.Equal(t, writeGateTiming{
		Samples: 100, P50UpperNS: 64_000, P95UpperNS: 100_000, MaxNS: 100_000,
	}, histogram.snapshot())
	histogram.record(time.Duration(math.MaxInt64))
	assert.EqualValues(t, math.MaxInt64, histogram.snapshot().MaxNS)
}

func TestWriteGateStatsCanceledWait(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		var gate sqliteWriteGate
		gate.Lock()
		ctx, cancel := context.WithCancel(context.Background())
		started := make(chan struct{})
		done := make(chan error, 1)
		go func() {
			done <- gate.LockContext(writeGateStatsWaitContext{Context: ctx, started: started})
		}()
		<-started
		synctest.Wait()
		<-time.NewTimer(3 * time.Millisecond).C
		cancel()
		require.ErrorIs(t, <-done, context.Canceled)
		gate.Unlock()
		snapshot := gate.stats.snapshot("test")
		require.Len(t, snapshot.Pairs, 1)
		assert.EqualValues(t, 1, snapshot.Pairs[0].Waits)
		assert.EqualValues(t, 3*time.Millisecond, snapshot.Pairs[0].WaitSumNS)
		var acquisitions uint64
		for _, site := range snapshot.Sites {
			acquisitions += site.Acquisitions
		}
		assert.EqualValues(t, 1, acquisitions)
	})
}

func TestWriteGateStatsNoAllocationAfterRegistration(t *testing.T) {
	var gate sqliteWriteGate
	take := func() { gate.Lock(); gate.Unlock() } //nolint:staticcheck // SA2001: an empty acquisition is the measured operation
	takeContext := func() { _ = gate.LockContext(context.Background()); gate.Unlock() }
	try := func() {
		if gate.TryLock() {
			gate.Unlock()
		}
	}
	for _, acquire := range []func(){take, takeContext, try} {
		assert.Zero(t, testing.AllocsPerRun(100, acquire))
	}
	gate.Lock()
	assert.Zero(t, testing.AllocsPerRun(100, func() { gate.TryLock() }))
	gate.Unlock()
}

type writeGateStatsLogBuffer struct {
	sync.Mutex
	buffer bytes.Buffer
}

func (b *writeGateStatsLogBuffer) Write(p []byte) (int, error) {
	b.Lock()
	defer b.Unlock()
	return b.buffer.Write(p)
}

func (b *writeGateStatsLogBuffer) String() string {
	b.Lock()
	defer b.Unlock()
	return b.buffer.String()
}

func writeGateStatsLogLines(t *testing.T, output string) []writeGateSnapshot {
	t.Helper()
	const prefix = "store_sqlite write_gate_stats "
	var result []writeGateSnapshot
	for _, line := range strings.Split(output, "\n") {
		_, payload, ok := strings.Cut(line, prefix)
		if !ok {
			continue
		}
		var snapshot writeGateSnapshot
		require.NoError(t, json.Unmarshal([]byte(payload), &snapshot))
		// Decode fields independently, so a renamed/missing JSON tag cannot
		// silently pass through the same struct at both ends.
		var fields map[string]json.RawMessage
		require.NoError(t, json.Unmarshal([]byte(payload), &fields))
		require.Contains(t, fields, "reason")
		require.Contains(t, fields, "sites")
		require.Contains(t, fields, "waiter_holder_pairs")
		result = append(result, snapshot)
	}
	return result
}

func TestWriteGateStatsLogFlushAndClose(t *testing.T) {
	var output writeGateStatsLogBuffer
	previous := log.Writer()
	log.SetOutput(&output)
	t.Cleanup(func() { log.SetOutput(previous) })
	store, err := Open(t.TempDir() + "/graph.sqlite")
	require.NoError(t, err)
	t.Cleanup(func() { _ = store.Close() })
	release, err := store.HoldWriteGate(context.Background())
	require.NoError(t, err)
	require.False(t, store.writeMu.TryLock())
	started := make(chan struct{})
	done := make(chan error, 1)
	go writeGateStatsWait(&store.writeMu, started, done)
	<-started
	release()
	require.NoError(t, <-done)
	store.writeMu.logStats("periodic")
	lines := writeGateStatsLogLines(t, output.String())
	require.Len(t, lines, 1)
	assert.Equal(t, "periodic", lines[0].Reason)
	require.NotEmpty(t, lines[0].Sites)
	var failures uint64
	for _, site := range lines[0].Sites {
		failures += site.TryLockFailures
	}
	assert.EqualValues(t, 1, failures)
	require.Len(t, lines[0].Pairs, 1)
	assert.EqualValues(t, 1, lines[0].Pairs[0].Waits)
	assert.Positive(t, lines[0].Pairs[0].WaitSumNS)
	assert.Contains(t, output.String(), `"waiter":`)
	assert.Contains(t, output.String(), `"holder_seen_at_wait_start":`)
	assert.Contains(t, output.String(), `"waits":`)
	assert.Contains(t, output.String(), `"wait_sum_ns":`)
	assert.Contains(t, output.String(), `"acquisitions":`)
	assert.Contains(t, output.String(), `"try_lock_failures":`)
	assert.Contains(t, output.String(), `"p50_upper_ns":`)
	assert.Contains(t, output.String(), `"p95_upper_ns":`)
	assert.Contains(t, output.String(), `"max_ns":`)
	store.writeMu.logStats("periodic")
	assert.Len(t, writeGateStatsLogLines(t, output.String()), 1, "unused intervals are silent")
	require.NoError(t, store.Close())
	lines = writeGateStatsLogLines(t, output.String())
	require.Len(t, lines, 2)
	assert.Equal(t, "close", lines[1].Reason)
	assert.Equal(t, lines[0].Pairs, lines[1].Pairs)
	assert.GreaterOrEqual(t, len(lines[1].Sites), len(lines[0].Sites))
	// stopCheckpointLoop joined the goroutine before the final checkpoint.
	select {
	case <-store.checkpointDone:
	default:
		t.Fatal("checkpoint/stats loop still running after Close")
	}
}

func TestWriteGateStatsPeriodicLogger(t *testing.T) {
	var output writeGateStatsLogBuffer
	previous := log.Writer()
	log.SetOutput(&output)
	t.Cleanup(func() { log.SetOutput(previous) })
	synctest.Test(t, func(t *testing.T) {
		store := &Store{storeCore: &storeCore{
			stopCheckpoint: make(chan struct{}), checkpointDone: make(chan struct{}),
		}}
		go store.runCheckpointLoopWithAttempt(time.Hour, time.Second, time.Second, func() bool {
			t.Error("unexpected checkpoint")
			return false
		})
		store.writeMu.Lock()
		synctest.Wait()
		<-time.NewTimer(30 * time.Second).C
		synctest.Wait()
		lines := writeGateStatsLogLines(t, output.String())
		require.Len(t, lines, 1)
		assert.Equal(t, "periodic", lines[0].Reason)
		<-time.NewTimer(30 * time.Second).C
		synctest.Wait()
		assert.Len(t, writeGateStatsLogLines(t, output.String()), 1)
		store.writeMu.Unlock()
		<-time.NewTimer(30 * time.Second).C
		synctest.Wait()
		lines = writeGateStatsLogLines(t, output.String())
		require.Len(t, lines, 2, "a hold completed since the last flush must be logged")
		require.Len(t, lines[1].Sites, 1)
		assert.EqualValues(t, 1, lines[1].Sites[0].Hold.Samples)
		assert.EqualValues(t, time.Minute, lines[1].Sites[0].Hold.MaxNS)
		store.stopCheckpointLoop()
	})
}
