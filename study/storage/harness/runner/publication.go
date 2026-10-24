package runner

import (
	"context"
	"fmt"
	"gxstudy/adapter"
	"gxstudy/dataset"
	"gxstudy/workload"
	"sync"
	"time"
)

const MinimumNewSamplesPerReader = 2
const MinimumCommitRaceSamples = 2

func verifyPublication(ctx context.Context, b adapter.Backend, cat adapter.Catalog, ref adapter.SegmentRef) error {
	if v, ok := cat.(adapter.PublicationVerifier); ok {
		return v.VerifyPublication(ctx, ref)
	}
	s, e := b.Open(ctx, ref.Path, ref)
	if e != nil {
		return e
	}
	return s.Close()
}

type publicationTimes struct {
	Begin, End          time.Time
	OpenNS, PrecommitNS int64
}

// Measure only the commit's before-write..after-dirsync interval. Verification
// Open is explicit and outside it, including for the oracle command.
func measurePublication(ctx context.Context, b adapter.Backend, cat adapter.Catalog, next, old adapter.SegmentRef) (publicationTimes, error) {
	var t publicationTimes
	start := time.Now()
	if e := verifyPublication(ctx, b, cat, next); e != nil {
		return t, e
	}
	t.OpenNS = time.Since(start).Nanoseconds()
	previous := adapter.Checkpoint
	index := 0
	var hookErr error
	adapter.Checkpoint = func(stage string) {
		if index >= len(stages) || stage != stages[index] {
			hookErr = fmt.Errorf("publication checkpoint order: %s", stage)
		}
		index++
		if stage == "before-write" {
			t.Begin = time.Now()
		}
		if stage == "after-dirsync" {
			t.End = time.Now()
		}
		previous(stage)
	}
	defer func() { adapter.Checkpoint = previous }()
	invoke := time.Now()
	e := cat.Publish(ctx, old.Repo, next, &old)
	if e != nil {
		return t, e
	}
	if hookErr != nil {
		return t, hookErr
	}
	if index != len(stages) || t.Begin.IsZero() || t.End.IsZero() {
		return t, fmt.Errorf("commit timing requires all ordered checkpoints")
	}
	t.PrecommitNS = t.Begin.Sub(invoke).Nanoseconds()
	return t, nil
}

// CommitRace is deliberately untimed. At every checkpoint two independent
// readers Pin, Open and compare real changed/removed content before the writer
// proceeds. The minimum is a correctness requirement, never a best-effort metric.
func commitRace(ctx context.Context, b adapter.Backend, factory adapter.CatalogFactory, base dataset.Dataset, next, old adapter.SegmentRef, dir string) (map[string]int, []workload.Check, error) {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	cat, e := factory(dir)
	if e != nil {
		return nil, nil, e
	}
	defer cat.Close()
	if e = cat.Publish(ctx, old.Repo, old, nil); e != nil {
		return nil, nil, e
	}
	if e = verifyPublication(ctx, b, cat, next); e != nil {
		return nil, nil, e
	}
	change, e := MakeChange(base)
	if e != nil {
		return nil, nil, e
	}
	renamed, removed, _ := deltaIdentities(base, change)
	var newRenamed dataset.Node
	for _, n := range change.Replacement.Nodes {
		if n.ID == renamed.ID {
			newRenamed = n
		}
	}
	windows := map[string]int{}
	var checks []workload.Check
	var firstErr error
	index := 0
	previous := adapter.Checkpoint
	adapter.Checkpoint = func(stage string) {
		if firstErr != nil {
			return
		}
		if index >= len(stages) || stage != stages[index] {
			firstErr = fmt.Errorf("commit-race hook order: %s", stage)
			return
		}
		index++
		expectedRef := old
		after := allowedAt(stage) == "new"
		if after {
			expectedRef = next
		}
		out := make(chan struct {
			check workload.Check
			err   error
		}, MinimumCommitRaceSamples)
		var wg sync.WaitGroup
		for reader := 0; reader < MinimumCommitRaceSamples; reader++ {
			wg.Add(1)
			go func(reader int) {
				defer wg.Done()
				q := workload.Query{ID: fmt.Sprintf("commit-race-%s-%d", stage, reader), Op: "id", Repo: old.Repo}
				want := workload.Answer{}
				if reader%2 == 0 {
					q.Key = removed.ID
					if after {
						want.Missing = true
					} else {
						n := removed
						want.Node = &n
					}
				} else {
					q.Key = renamed.ID
					n := renamed
					if after {
						n = newRenamed
					}
					want.Node = &n
				}
				view, release, e := cat.Pin(ctx, []string{old.Repo})
				if e != nil {
					out <- struct {
						check workload.Check
						err   error
					}{err: e}
					return
				}
				defer release()
				if view[old.Repo] != expectedRef {
					out <- struct {
						check workload.Check
						err   error
					}{err: fmt.Errorf("%s recovered wrong commit-side view", stage)}
					return
				}
				s, e := b.Open(ctx, view[old.Repo].Path, view[old.Repo])
				if e != nil {
					out <- struct {
						check workload.Check
						err   error
					}{err: e}
					return
				}
				got, e := workload.Evaluate(ctx, map[string]adapter.Segment{old.Repo: s}, nil, q)
				closeErr := s.Close()
				if e == nil {
					e = closeErr
				}
				c := workload.Compare(q, want, got)
				out <- struct {
					check workload.Check
					err   error
				}{c, e}
			}(reader)
		}
		// Wait and drain every owned reader even on an error; adapters must honor ctx.
		wg.Wait()
		for reader := 0; reader < MinimumCommitRaceSamples; reader++ {
			value := <-out
			if value.err != nil && firstErr == nil {
				firstErr = value.err
			}
			checks = append(checks, value.check)
			if value.err == nil && !value.check.Mismatch {
				windows[stage]++
			}
		}
		if windows[stage] < MinimumCommitRaceSamples && firstErr == nil {
			firstErr = fmt.Errorf("%s has %d checked samples, need %d", stage, windows[stage], MinimumCommitRaceSamples)
		}
	}
	defer func() { adapter.Checkpoint = previous }()
	if e = cat.Publish(ctx, old.Repo, next, &old); e != nil {
		return windows, checks, e
	}
	if firstErr != nil {
		return windows, checks, firstErr
	}
	if index != len(stages) {
		return windows, checks, fmt.Errorf("missing commit-race windows")
	}
	return windows, checks, nil
}
