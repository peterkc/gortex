package store_pebble

import (
	"context"
	"io"

	"github.com/cockroachdb/pebble/v2"
)

type snapshotReadTracker struct {
	snapshotReader
	points    []string
	scans     []string
	steps     map[string]int
	seeks     map[string]int
	failPoint string
	failure   error
}

func trackSnapshotReads(s *Snapshot) *snapshotReadTracker {
	tracker := &snapshotReadTracker{snapshotReader: s.snap, steps: make(map[string]int), seeks: make(map[string]int)}
	s.snap = tracker
	return tracker
}

func (r *snapshotReadTracker) Get(key []byte) ([]byte, io.Closer, error) {
	r.points = append(r.points, string(key))
	if r.failure != nil && string(key) == r.failPoint {
		return nil, nil, r.failure
	}
	return r.snapshotReader.Get(key)
}

func (r *snapshotReadTracker) NewIterWithContext(ctx context.Context, opts *pebble.IterOptions) (snapshotIterator, error) {
	prefix := string(opts.LowerBound)
	r.scans = append(r.scans, prefix)
	i, err := r.snapshotReader.NewIterWithContext(ctx, opts)
	if err != nil {
		return nil, err
	}
	return &trackedSnapshotIterator{snapshotIterator: i, tracker: r, prefix: prefix}, nil
}

type trackedSnapshotIterator struct {
	snapshotIterator
	tracker *snapshotReadTracker
	prefix  string
}

func (i *trackedSnapshotIterator) Next() bool {
	i.tracker.steps[i.prefix]++
	return i.snapshotIterator.Next()
}

func (i *trackedSnapshotIterator) SeekGE(key []byte) bool {
	i.tracker.seeks[i.prefix]++
	return i.snapshotIterator.SeekGE(key)
}
