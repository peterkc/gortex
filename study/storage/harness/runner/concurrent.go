package runner

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"gxstudy/adapter"
	"gxstudy/dataset"
	"gxstudy/oracle"
	"gxstudy/workload"
	"math/rand/v2"
	"path/filepath"
	"sync"
	"sync/atomic"
	"time"
)

type readerSample struct {
	start, end       time.Time
	ns               int64
	pinStart, pinEnd time.Time
	check            workload.Check
	newVersion       bool
	err              error
}
type cacheOpen struct {
	done    chan struct{}
	segment adapter.Segment
	err     error
}
type readCache struct {
	pending  map[adapter.SegmentRef]*cacheOpen
	mu       sync.Mutex
	segments map[adapter.SegmentRef]adapter.Segment
	owned    []adapter.Segment
	b        adapter.Backend
}

func (c *readCache) get(ctx context.Context, ref adapter.SegmentRef) (adapter.Segment, error) {
	c.mu.Lock()
	if existing := c.segments[ref]; existing != nil {
		c.mu.Unlock()
		return existing, nil
	}
	if call := c.pending[ref]; call != nil {
		c.mu.Unlock()
		select {
		case <-call.done:
			return call.segment, call.err
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	if c.pending == nil {
		c.pending = map[adapter.SegmentRef]*cacheOpen{}
	}
	call := &cacheOpen{done: make(chan struct{})}
	c.pending[ref] = call
	c.mu.Unlock()
	// Only this version's waiters wait. Other pinned versions remain readable.
	segment, err := c.b.Open(ctx, ref.Path, ref)
	c.mu.Lock()
	call.segment = segment
	call.err = err
	if err == nil {
		if c.segments == nil {
			c.segments = map[adapter.SegmentRef]adapter.Segment{}
		}
		c.segments[ref] = segment
		c.owned = append(c.owned, segment)
	}
	close(call.done)
	delete(c.pending, ref)
	c.mu.Unlock()
	return segment, err
}
func (c *readCache) close() error {
	var errs []error
	for _, s := range c.owned {
		errs = append(errs, s.Close())
	}
	c.segments = nil
	c.owned = nil
	return errors.Join(errs...)
}

type readerControl struct {
	stop         chan struct{}
	ready        chan struct{}
	start        chan struct{}
	out          chan []readerSample
	wg           sync.WaitGroup
	publishReady atomic.Bool
	active       chan struct{}
	published    atomic.Bool
	newCount     [8]atomic.Int64
	progress     chan struct{}
}

func startReaders(ctx context.Context, b adapter.Backend, cat adapter.Catalog, cache *readCache, segments map[string]adapter.Segment, old adapter.SegmentRef, qs []workload.Query, oldWant, newWant []workload.Expected) *readerControl {
	c := &readerControl{stop: make(chan struct{}), ready: make(chan struct{}, 8), start: make(chan struct{}), out: make(chan []readerSample, 8), active: make(chan struct{}), progress: make(chan struct{}, 1)}
	for worker := 0; worker < 8; worker++ {
		c.wg.Add(1)
		go readerLoop(ctx, b, cat, cache, segments, old, qs, oldWant, newWant, c, worker)
	}
	for i := 0; i < 8; i++ {
		<-c.ready
	}
	close(c.start)
	return c
}
func readerLoop(ctx context.Context, b adapter.Backend, cat adapter.Catalog, cache *readCache, segments map[string]adapter.Segment, old adapter.SegmentRef, qs []workload.Query, oldWant, newWant []workload.Expected, c *readerControl, worker int) {
	defer c.wg.Done()
	c.ready <- struct{}{}
	<-c.start
	var samples []readerSample
	caps := b.Capabilities()
	order := rand.New(rand.NewPCG(uint64(worker+20261022), 31)).Perm(len(qs))
	for i := 0; ; i = (i + 1) % len(qs) {
		index := order[i%len(qs)]
		select {
		case <-c.stop:
			c.out <- samples
			return
		case <-ctx.Done():
			c.out <- append(samples, readerSample{err: ctx.Err()})
			return
		default:
		}
		q := withPolicy(qs[index], caps)
		if unsupported(caps, q) != "" {
			continue
		}
		pinStart := time.Now()
		view, release, e := cat.Pin(ctx, []string{old.Repo})
		if e != nil {
			c.out <- append(samples, readerSample{err: e})
			return
		}
		s, e := cache.get(ctx, view[old.Repo])
		pinEnd := time.Now()
		if e != nil {
			release()
			c.out <- append(samples, readerSample{err: e})
			return
		}
		local := make(map[string]adapter.Segment, len(segments))
		for repo, seg := range segments {
			local[repo] = seg
		}
		local[old.Repo] = s
		expected := oldWant[index]
		if view[old.Repo] != old {
			expected = newWant[index]
		}
		if c.publishReady.Load() {
			select {
			case c.active <- struct{}{}:
			default:
			}
		}
		queryMulti, inputs := traceNativeFanout(adapter.Fanout{Segments: local}, q)
		publishedAtStart := c.published.Load()
		start := time.Now()
		got, e := workload.Evaluate(ctx, local, queryMulti, q)
		end := time.Now()
		release()
		check := compareRankedAnswer(q, expected, got, inputs)
		newVersion := view[old.Repo] != old && publishedAtStart
		if newVersion && e == nil && !check.Mismatch {
			c.newCount[worker].Add(1)
			select {
			case c.progress <- struct{}{}:
			default:
			}
		}
		samples = append(samples, readerSample{start: start, end: end, ns: end.Sub(start).Nanoseconds(), pinStart: pinStart, pinEnd: pinEnd, check: check, err: e, newVersion: newVersion})
		if e != nil {
			c.out <- samples
			return
		}
	}
}
func finishReaders(c *readerControl) ([]readerSample, error) {
	close(c.stop)
	c.wg.Wait()
	var samples []readerSample
	for i := 0; i < 8; i++ {
		samples = append(samples, (<-c.out)...)
	}
	for _, s := range samples {
		if s.err != nil {
			return samples, s.err
		}
	}
	return samples, nil
}
func changedAnswers(ctx context.Context, ds []dataset.Dataset, changed dataset.Dataset, qs []workload.Query) ([]workload.Expected, error) {
	copies := append([]dataset.Dataset(nil), ds...)
	copies[0] = changed
	s := map[string]adapter.Segment{}
	for _, d := range copies {
		s[d.Repo] = oracle.New(d)
	}
	m := oracle.NewMulti(copies, s)
	var want []workload.Expected
	for _, q := range qs {
		a, e := workload.Evaluate(ctx, s, m, q)
		if e != nil {
			return nil, e
		}
		want = append(want, workload.Expected{QueryID: q.ID, Answer: a})
	}
	return oracle.ReferenceAnswers(copies, qs, want)
}
func concurrent(ctx context.Context, b adapter.Backend, factory adapter.CatalogFactory, segments map[string]adapter.Segment, ds []dataset.Dataset, old adapter.SegmentRef, changed dataset.Dataset, qs []workload.Query, want []workload.Expected, dir string, safetyTimeout time.Duration) (ConcurrentResult, error) {
	r := ConcurrentResult{}
	supported := 0
	for _, q := range qs {
		if unsupported(b.Capabilities(), q) == "" {
			supported++
		}
	}
	if supported == 0 {
		r.Checks = []workload.Check{{QueryID: "concurrent", Operation: "concurrent", Status: "unsupported", Detail: "no supported reader operations"}}
		return r, nil
	}
	newWant, e := changedAnswers(ctx, ds, changed, qs)
	if e != nil {
		return r, e
	}
	cat, e := factory(filepath.Join(dir, "catalog"))
	if e != nil {
		return r, e
	}
	defer cat.Close()
	if e = cat.Publish(ctx, old.Repo, old, nil); e != nil {
		return r, e
	}
	pinned, release, e := cat.Pin(ctx, []string{old.Repo})
	if e != nil {
		return r, e
	}
	defer release()
	cache := &readCache{segments: map[adapter.SegmentRef]adapter.Segment{old: segments[old.Repo]}, b: b}
	defer cache.close()
	preparePhase()
	idleControl := startReaders(ctx, b, cat, cache, segments, old, qs, want, newWant)
	timer := time.NewTimer(150 * time.Millisecond)
	<-timer.C
	idle, e := finishReaders(idleControl)
	if e != nil {
		return r, e
	}
	var ns []int64
	for _, s := range idle {
		ns = append(ns, s.ns)
		r.Checks = append(r.Checks, s.check)
	}
	r.Idle = Summarize(ns)
	preparePhase()
	control := startReaders(ctx, b, cat, cache, segments, old, qs, want, newWant)
	begin := time.Now()
	ref, _, buildErr := b.Build(ctx, changed, filepath.Join(dir, "new-segment"))
	buildEnd := time.Now()
	if buildErr != nil {
		finishReaders(control)
		return r, buildErr
	}
	control.publishReady.Store(true)
	handshake := time.NewTimer(30 * time.Second)
	select {
	case <-control.active:
		handshake.Stop()
	case <-handshake.C:
		finishReaders(control)
		return r, fmt.Errorf("no active supported reader reached publication start")
	case <-ctx.Done():
		handshake.Stop()
		finishReaders(control)
		return r, ctx.Err()
	}
	control.publishReady.Store(false)
	times, publishErr := measurePublication(ctx, b, cat, ref, old)
	publishStart, publishEnd := times.Begin, times.End
	r.OpenNS = times.OpenNS
	r.PrecommitNS = times.PrecommitNS
	if publishErr != nil {
		finishReaders(control)
		return r, publishErr
	}
	r.PublishNS = publishEnd.Sub(publishStart).Nanoseconds()
	r.BuildPublishNS = publishEnd.Sub(begin).Nanoseconds()
	control.published.Store(true)
	afterTimer := time.NewTimer(30 * time.Second)
	enough := func() bool {
		for i := 0; i < 8; i++ {
			if control.newCount[i].Load() < MinimumNewSamplesPerReader {
				return false
			}
		}
		return true
	}
	var afterErr error
	for !enough() && afterErr == nil {
		select {
		case <-control.progress:
		case <-afterTimer.C:
			afterErr = fmt.Errorf("new-version checked sample minimum not reached")
		case <-ctx.Done():
			afterErr = ctx.Err()
		}
	}
	afterTimer.Stop()
	samples, readerErr := finishReaders(control)
	for i := 0; i < 8; i++ {
		r.NewSamplesPerReader = append(r.NewSamplesPerReader, control.newCount[i].Load())
	}
	if afterErr != nil {
		return r, afterErr
	}
	if readerErr != nil {
		return r, readerErr
	}
	var duringBuild, duringPublish, after, pins, afterPins []int64
	for _, s := range samples {
		r.Checks = append(r.Checks, s.check)
		if s.start.Before(buildEnd) && s.end.After(begin) {
			duringBuild = append(duringBuild, s.ns)
		}
		if s.start.Before(publishEnd) && s.end.After(publishStart) {
			duringPublish = append(duringPublish, s.ns)
		}
		if s.start.After(publishEnd) && s.newVersion {
			after = append(after, s.ns)
			afterPins = append(afterPins, s.pinEnd.Sub(s.pinStart).Nanoseconds())
		}
		if s.pinStart.Before(publishEnd) && s.pinEnd.After(publishStart) {
			pins = append(pins, s.pinEnd.Sub(s.pinStart).Nanoseconds())
		}
	}
	r.DuringBuild = Summarize(duringBuild)
	r.DuringPublish = Summarize(duringPublish)
	r.AfterPublish = Summarize(after)
	r.PinOpenAfterPublish = Summarize(afterPins)
	r.PinOpenDuringPublish = Summarize(pins)
	r.ReaderPasses = len(samples) / max(1, len(qs))

	view, done, e := cat.Pin(ctx, []string{old.Repo})
	if e != nil {
		return r, e
	}
	done()
	if view[old.Repo] != ref {
		return r, fmt.Errorf("new publication not visible")
	}
	previous, e := cat.Previous(ctx, old.Repo)
	if e != nil || previous != old {
		return r, fmt.Errorf("previous ref lost: %w", e)
	}
	// This checks retention and actual old content, not equality of a copied map.
	oldSegment, e := b.Open(ctx, pinned[old.Repo].Path, pinned[old.Repo])
	if e != nil {
		return r, fmt.Errorf("pinned old version became unopenable: %w", e)
	}
	defer oldSegment.Close()
	change, e := MakeChange(ds[0])
	if e != nil {
		return r, e
	}
	_, removed, _ := deltaIdentities(ds[0], change)
	actual, e := oldSegment.NodeByID(ctx, removed.ID)
	if e != nil {
		return r, fmt.Errorf("pinned deleted symbol was lost: %w", e)
	}
	a, _ := json.Marshal(actual)
	v, _ := json.Marshal(removed)
	if string(a) != string(v) {
		return r, fmt.Errorf("pinned old content changed")
	}
	newSegment, e := cache.get(ctx, ref)
	if e != nil {
		return r, e
	}
	if _, e = newSegment.NodeByID(ctx, removed.ID); !errors.Is(e, adapter.ErrNotFound) {
		return r, fmt.Errorf("removed symbol remains in new version: %v", e)
	}
	r.CommitRaceMinimum = MinimumCommitRaceSamples
	windows, raceChecks, raceErr := commitRace(ctx, b, factory, ds[0], ref, old, filepath.Join(dir, "commit-race"), safetyTimeout)
	r.CommitRaceWindows = windows
	r.Checks = append(r.Checks, raceChecks...)
	if raceErr != nil {
		return r, raceErr
	}
	return r, nil
}
