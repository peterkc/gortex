package store_sqlite

import (
	"context"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// sqliteWriteGate is a zero-value, context-aware binary semaphore. It keeps
// the Lock/Unlock surface used by existing mutation paths while allowing
// maintenance and bounded resolver work to stop waiting when their context
// expires. A channel is used instead of racing a helper goroutine against a
// sync.Mutex; the latter leaks a waiter that can acquire the mutex after the
// caller has already returned.
type sqliteWriteGate struct {
	once  sync.Once
	token chan struct{}
	stats writeGateStats
	// holder is observational only. The token still owns exclusion; the empty
	// windows before publication and after clearing are reported as unknown.
	holder atomic.Pointer[writeGateSiteStats]
	heldAt time.Time // protected by the token
}

func (g *sqliteWriteGate) init() {
	g.once.Do(func() {
		g.token = make(chan struct{}, 1)
		g.token <- struct{}{}
	})
}

// callSite skips this file, including Lock and HoldWriteGate, so an outer
// writer window is attributed to its caller rather than the wrapper. Cache
// skipped PCs too: resolving inlined frames may allocate on the cold path.
func (g *sqliteWriteGate) callSite() *writeGateSiteStats {
	var pcs [8]uintptr
	n := runtime.Callers(2, pcs[:])
	for _, pc := range pcs[:n] {
		if cached, ok := g.stats.sites.Load(pc); ok {
			if site := cached.(*writeGateSiteStats); site != nil {
				return site
			}
			continue
		}
		fn := runtime.FuncForPC(pc - 1)
		if fn == nil {
			// An unresolvable frame must not panic a store write; skip it.
			continue
		}
		file, line := fn.FileLine(pc - 1)
		if strings.HasSuffix(file, "/write_gate.go") {
			g.stats.sites.Store(pc, (*writeGateSiteStats)(nil))
			continue
		}
		return g.stats.register(pc, fn.Name(), file, line)
	}
	return g.stats.register(0, "unknown", "unknown", 0)
}

func (g *sqliteWriteGate) Lock() {
	_ = g.LockContext(context.Background())
}

func (g *sqliteWriteGate) LockContext(ctx context.Context) error {
	g.init()
	// A single select may choose the token nondeterministically when ctx is
	// already canceled and the gate is available. Check before waiting, then
	// re-check after acquisition and restore the token on cancellation so a
	// maintenance deadline can never start work after it has expired.
	if err := ctx.Err(); err != nil {
		return err
	}
	site := g.callSite()
	started := time.Now()
	holder := g.holder.Load()
	waiting := len(g.token) == 0
	select {
	case <-ctx.Done():
		site.recordWait(time.Since(started), holder, waiting)
		g.stats.used.Store(true)
		return ctx.Err()
	case <-g.token:
		g.holder.Store(site)
		g.heldAt = time.Now()
		site.recordWait(g.heldAt.Sub(started), holder, waiting)
		if err := ctx.Err(); err != nil {
			g.stats.used.Store(true)
			g.holder.Store(nil)
			g.token <- struct{}{}
			return err
		}
		site.acquisitions.Add(1)
		g.stats.used.Store(true)
		return nil
	}
}

func (g *sqliteWriteGate) TryLock() bool {
	g.init()
	site := g.callSite()
	select {
	case <-g.token:
		g.holder.Store(site)
		g.heldAt = time.Now()
		site.acquisitions.Add(1)
		site.wait.record(0)
		g.stats.used.Store(true)
		return true
	default:
		site.tryLockFailures.Add(1)
		g.stats.used.Store(true)
		return false
	}
}

func (g *sqliteWriteGate) Unlock() {
	g.init()
	if site := g.holder.Load(); site != nil {
		site.hold.record(time.Since(g.heldAt))
		g.stats.used.Store(true)
	}
	g.holder.Store(nil)
	select {
	case g.token <- struct{}{}:
	default:
		panic("store_sqlite: unlock of unlocked write gate")
	}
}

// HoldWriteGate takes the store's mutation gate and returns the release.
//
// It is the exclusive-writer window in its own right, the way ResolveMutex is
// the resolver's: a caller that needs every other write to queue behind it
// holds this, and the read pool keeps answering throughout. It is also the
// only way a package that cannot see the gate can reproduce a saturated
// writer — the state a long build leaves the store in — and prove that a read
// path never reaches for it.
//
// Nothing bounds how long the gate may be held, so the release belongs on a
// defer. It is idempotent; releasing twice does not free somebody else's turn.
func (s *Store) HoldWriteGate(ctx context.Context) (func(), error) {
	if err := s.writeMu.LockContext(ctx); err != nil {
		return nil, err
	}
	var once sync.Once
	return func() { once.Do(s.writeMu.Unlock) }, nil
}
