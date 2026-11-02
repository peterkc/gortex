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
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"time"
)

type readerSample struct {
	start, end       time.Time
	ns               int64
	pinStart, pinEnd time.Time
	pinNS            int64
	check            workload.Check
	pending          *pendingCheck
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

func (c *readCache) get(ctx context.Context, ref adapter.SegmentRef) (adapter.Segment, bool, error) {
	c.mu.Lock()
	if existing := c.segments[ref]; existing != nil {
		c.mu.Unlock()
		return existing, false, nil
	}
	if call := c.pending[ref]; call != nil {
		c.mu.Unlock()
		select {
		case <-call.done:
			return call.segment, true, call.err
		case <-ctx.Done():
			return nil, false, ctx.Err()
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
	return segment, true, err
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
	files        chan string
	wg           sync.WaitGroup
	publishReady atomic.Bool
	active       chan struct{}
	published    atomic.Bool
	newCount     [8]atomic.Int64
	progress     chan struct{}
}

func startReaders(ctx context.Context, b adapter.Backend, cat adapter.Catalog, cache *readCache, segments map[string]adapter.Segment, old adapter.SegmentRef, qs []workload.Query, oldWant, newWant []workload.Expected) *readerControl {
	c := &readerControl{stop: make(chan struct{}), ready: make(chan struct{}, 8), start: make(chan struct{}), out: make(chan []readerSample, 8), files: make(chan string, 8), active: make(chan struct{}), progress: make(chan struct{}, 1)}
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
	var spool *os.File
	if dir, ok := ctx.Value(sampleDirectoryKey{}).(string); ok {
		var err error
		spool, err = os.CreateTemp(dir, fmt.Sprintf("reader-%d-*.jsonl", worker))
		if err != nil {
			c.files <- ""
			c.out <- []readerSample{{err: err}}
			return
		}
		c.files <- spool.Name()
	} else {
		c.files <- ""
	}
	finish := func(err error) {
		if spool != nil {
			err = errors.Join(err, spool.Close())
		}
		if err != nil {
			samples = append(samples, readerSample{err: err})
		}
		c.out <- samples
	}
	emit := func(sample readerSample) error {
		if spool == nil {
			samples = append(samples, sample)
			return nil
		}
		row := storedSample{Start: sample.start, End: sample.end, PinStart: sample.pinStart, PinEnd: sample.pinEnd, PinNS: sample.pinNS, NS: sample.ns, Check: sample.check, Pending: sample.pending, NewVersion: sample.newVersion}
		if sample.err != nil {
			row.Error = sample.err.Error()
		}
		return json.NewEncoder(spool).Encode(row)
	}
	caps := b.Capabilities()
	order := rand.New(rand.NewPCG(uint64(worker+20261022), 31)).Perm(len(qs))
	for i := 0; ; i = (i + 1) % len(qs) {
		index := order[i%len(qs)]
		select {
		case <-c.stop:
			finish(nil)
			return
		case <-ctx.Done():
			finish(ctx.Err())
			return
		default:
		}
		q := withPolicy(qs[index], caps)
		if unsupported(caps, q) != "" {
			continue
		}
		view, release, pinStart, pinEnd, e := pinTimed(ctx, cat, []string{old.Repo})
		if e != nil {
			finish(e)
			return
		}
		s, opened, e := cache.get(ctx, view[old.Repo])
		pinNS := pinEnd.Sub(pinStart).Nanoseconds()
		if remote, ok := s.(*remoteSegment); ok {
			if opened && remote.openEnd.After(pinEnd) {
				begin := remote.openStart
				if begin.Before(pinEnd) {
					begin = pinEnd
				}
				pinNS += remote.openEnd.Sub(begin).Nanoseconds()
				pinEnd = remote.openEnd
			}
		} else {
			pinEnd = time.Now()
			pinNS = pinEnd.Sub(pinStart).Nanoseconds()
		}
		if e != nil {
			release()
			finish(e)
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
		publishedAtStart := c.published.Load()
		got, inputs, start, end, ns, e := evaluateTimed(ctx, local, adapter.Fanout{Segments: local}, q)
		release()
		var check workload.Check
		var pending *pendingCheck
		if spool != nil {
			pending = &pendingCheck{Query: q, Expected: expected, Sources: expected.Sources, NativeScoring: q.NativeScoring, MatchRepos: q.MatchRepos, Answer: got, Inputs: inputs}
		} else {
			var checkErr error
			check, checkErr = checkedAnswer(ctx, q, expected, got, inputs)
			if e == nil {
				e = checkErr
			}
		}
		newVersion := view[old.Repo] != old && publishedAtStart
		if newVersion && e == nil && !check.Mismatch {
			c.newCount[worker].Add(1)
			select {
			case c.progress <- struct{}{}:
			default:
			}
		}
		if writeErr := emit(readerSample{start: start, end: end, ns: ns, pinStart: pinStart, pinEnd: pinEnd, pinNS: pinNS, check: check, pending: pending, err: e, newVersion: newVersion}); writeErr != nil {
			finish(writeErr)
			return
		}
		if e != nil {
			finish(e)
			return
		}
		if check.Mismatch {
			finish(fmt.Errorf("reader check %s: %s", check.QueryID, check.Detail))
			return
		}
	}
}
func finishReaders(c *readerControl) ([]readerSample, error) {
	stream, err := finishReaderStream(c)
	if err != nil {
		return nil, err
	}
	var rows []readerSample
	for row, err := range stream.rows() {
		if err != nil {
			return rows, err
		}
		rows = append(rows, row)
	}
	return rows, nil
}
func changedAnswers(ctx context.Context, ds []dataset.Dataset, changed dataset.Dataset, qs []workload.Query) ([]workload.Expected, error) {
	copies := append([]dataset.Dataset(nil), ds...)
	copies[0] = changed
	if changed.Source != nil {
		var want []workload.Expected
		for _, q := range qs {
			want = append(want, workload.Expected{QueryID: q.ID, Sources: copies})
		}
		return want, nil
	}
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
func concurrent(ctx context.Context, b adapter.Backend, factory adapter.CatalogFactory, segments map[string]adapter.Segment, ds []dataset.Dataset, old adapter.SegmentRef, changed dataset.Dataset, qs []workload.Query, want []workload.Expected, dir string, safetyTimeout time.Duration) (r ConcurrentResult, err error) {
	_, diskMode := ctx.Value(sampleDirectoryKey{}).(string)
	r.SampleSHA256 = map[string]string{}
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
	defer func() { err = errors.Join(err, cat.Close()) }()
	if e = cat.Publish(ctx, old.Repo, old, nil); e != nil {
		return r, e
	}
	pinned, release, e := cat.Pin(ctx, []string{old.Repo})
	if e != nil {
		return r, e
	}
	defer release()
	cache := &readCache{segments: map[adapter.SegmentRef]adapter.Segment{old: segments[old.Repo]}, b: b}
	defer func() { err = errors.Join(err, cache.close()) }()
	memo, e := newReaderCheckMemo(dir)
	if e != nil {
		return r, e
	}
	defer func() { err = errors.Join(err, memo.close()) }()
	preparePhase()
	idleControl := startReaders(ctx, b, cat, cache, segments, old, qs, want, newWant)
	timer := time.NewTimer(150 * time.Millisecond)
	<-timer.C
	idle, e := finishReaderStream(idleControl)
	if e != nil {
		return r, e
	}
	idleStats, e := newDiskDistribution(dir, "idle")
	if e != nil {
		return r, e
	}
	defer func() { err = errors.Join(err, idleStats.close()) }()
	for s, readErr := range idle.rows() {
		if readErr != nil {
			return r, readErr
		}
		s, checkErr := memo.checkReaderSample(ctx, s)
		if checkErr != nil {
			return r, checkErr
		}
		if e = idleStats.add(s.ns); e != nil {
			return r, e
		}
		r.CheckedSamples++
		if !diskMode || s.check.Mismatch {
			r.Checks = append(r.Checks, s.check)
		}
	}
	r.Idle, e = idleStats.finish()
	if e != nil {
		return r, e
	}
	preparePhase()
	control := startReaders(ctx, b, cat, cache, segments, old, qs, want, newWant)
	begin := time.Now()
	ref, _, buildErr := b.Build(ctx, changed, filepath.Join(dir, "new-segment"))
	buildEnd := time.Now()
	if client := backendClient(b); client != nil {
		client.mu.Lock()
		begin, buildEnd = client.buildStart, client.buildEnd
		client.mu.Unlock()
	}
	if buildErr != nil {
		finishReaderStream(control)
		return r, buildErr
	}
	control.publishReady.Store(true)
	handshake := time.NewTimer(30 * time.Second)
	select {
	case <-control.active:
		handshake.Stop()
	case <-handshake.C:
		finishReaderStream(control)
		return r, fmt.Errorf("no active supported reader reached publication start")
	case <-ctx.Done():
		handshake.Stop()
		finishReaderStream(control)
		return r, ctx.Err()
	}
	control.publishReady.Store(false)
	times, publishErr := measurePublication(ctx, b, cat, ref, old)
	publishStart, publishEnd := times.Begin, times.End
	r.OpenNS = times.OpenNS
	r.PrecommitNS = times.PrecommitNS
	if publishErr != nil {
		finishReaderStream(control)
		return r, publishErr
	}
	r.PublishNS = times.ElapsedNS
	r.BuildPublishNS = publishEnd.Sub(begin).Nanoseconds()
	if client := backendClient(b); client != nil {
		client.mu.Lock()
		r.BuildPublishNS = client.lastBuild + times.OpenNS + times.PrecommitNS + times.ElapsedNS
		client.mu.Unlock()
	}
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
			afterErr = fmt.Errorf("new-version sample minimum not reached before deferred checks")
		case <-ctx.Done():
			afterErr = ctx.Err()
		}
	}
	afterTimer.Stop()
	samples, readerErr := finishReaderStream(control)
	for i := 0; i < 8; i++ {
		r.NewSamplesPerReader = append(r.NewSamplesPerReader, control.newCount[i].Load())
	}
	if afterErr != nil {
		return r, afterErr
	}
	if readerErr != nil {
		return r, readerErr
	}
	stats := map[string]*diskDistribution{}
	defer func() {
		for _, value := range stats {
			err = errors.Join(err, value.close())
		}
	}()
	for _, name := range []string{"during-build", "during-publish", "after", "pins", "after-pins"} {
		value, e := newDiskDistribution(dir, name)
		if e != nil {
			return r, e
		}
		stats[name] = value
	}
	count := 0
	for s, readErr := range samples.rows() {
		if readErr != nil {
			return r, readErr
		}
		s, checkErr := memo.checkReaderSample(ctx, s)
		if checkErr != nil {
			return r, checkErr
		}
		count++
		r.CheckedSamples++
		if !diskMode || s.check.Mismatch {
			r.Checks = append(r.Checks, s.check)
		}
		if s.start.Before(buildEnd) && s.end.After(begin) {
			if e = stats["during-build"].add(s.ns); e != nil {
				return r, e
			}
		}
		if s.start.Before(publishEnd) && s.end.After(publishStart) {
			if e = stats["during-publish"].add(s.ns); e != nil {
				return r, e
			}
		}
		if s.start.After(publishEnd) && s.newVersion {
			if e = stats["after"].add(s.ns); e != nil {
				return r, e
			}
			if e = stats["after-pins"].add(s.pinNS); e != nil {
				return r, e
			}
		}
		if s.pinStart.Before(publishEnd) && s.pinEnd.After(publishStart) {
			if e = stats["pins"].add(s.pinNS); e != nil {
				return r, e
			}
		}
	}
	for name, target := range map[string]*Distribution{"during-build": &r.DuringBuild, "during-publish": &r.DuringPublish, "after": &r.AfterPublish, "pins": &r.PinOpenDuringPublish, "after-pins": &r.PinOpenAfterPublish} {
		*target, e = stats[name].finish()
		if e != nil {
			return r, e
		}
	}
	for _, path := range append(idle.files, samples.files...) {
		hash, e := dataset.Hash(path)
		if e != nil {
			return r, e
		}
		r.SampleSHA256[filepath.Base(path)] = hash
	}
	r.ReaderPasses = count / max(1, len(qs))

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
	defer func() { err = errors.Join(err, oldSegment.Close()) }()
	change, e := MakeChange(ds[0])
	if e != nil {
		return r, e
	}
	_, removed, _, e := deltaIdentities(ds[0], change)
	if e != nil {
		return r, e
	}
	actual, e := oldSegment.NodeByID(ctx, removed.ID)
	if e != nil {
		return r, fmt.Errorf("pinned deleted symbol was lost: %w", e)
	}
	a, _ := json.Marshal(actual)
	v, _ := json.Marshal(removed)
	if string(a) != string(v) {
		return r, fmt.Errorf("pinned old content changed")
	}
	newSegment, _, e := cache.get(ctx, ref)
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
