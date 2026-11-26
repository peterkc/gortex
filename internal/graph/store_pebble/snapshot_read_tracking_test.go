package store_pebble

import (
	"context"
	"io"

	"github.com/cockroachdb/pebble/v2"
)

type snapshotReadTracker struct {
	snapshotReader
	points []string
	scans  []string
}

func trackSnapshotReads(s *Snapshot) *snapshotReadTracker {
	tracker := &snapshotReadTracker{snapshotReader: s.snap}
	s.snap = tracker
	return tracker
}

func (r *snapshotReadTracker) Get(key []byte) ([]byte, io.Closer, error) {
	r.points = append(r.points, string(key))
	return r.snapshotReader.Get(key)
}

func (r *snapshotReadTracker) NewIterWithContext(ctx context.Context, opts *pebble.IterOptions) (*pebble.Iterator, error) {
	r.scans = append(r.scans, string(opts.LowerBound))
	return r.snapshotReader.NewIterWithContext(ctx, opts)
}
