package store_pebble

import (
	"errors"
	"slices"
	"testing"

	"github.com/cockroachdb/pebble/v2"
)

func TestCloneShinglesSnapshotReplacement(t *testing.T) {
	b := newBuild(t, DefaultProfile)
	if err := b.BulkSetCloneShingles("old", map[string][]uint64{"id": {7, 1, 7}}); err != nil {
		t.Fatal(err)
	}
	if err := b.Publish(t.Context()); err != nil {
		t.Fatal(err)
	}
	first := snapshot(t, b)
	if err := b.BulkSetCloneShingles("new", map[string][]uint64{"id": {2, 1}}); err != nil {
		t.Fatal(err)
	}
	if err := b.Publish(t.Context()); err != nil {
		t.Fatal(err)
	}
	next := snapshot(t, b)
	for _, tc := range []struct {
		view *Snapshot
		repo string
		want []uint64
	}{{first, "old", []uint64{7, 1, 7}}, {next, "old", nil}, {next, "new", []uint64{2, 1}}} {
		rows, err := tc.view.LoadCloneShingles(tc.repo)
		if err != nil || rows == nil || !slices.Equal(rows["id"], tc.want) {
			t.Fatalf("load %q = %v, %v; want id shingles %v", tc.repo, rows, err, tc.want)
		}
	}
	if err := b.pending.Set(shingleKey("new", "bad"), []byte{1}, pebble.NoSync); err != nil {
		t.Fatal(err)
	}
	if err := b.Publish(t.Context()); !errors.Is(err, errInvalidRecord) {
		t.Fatalf("corrupt sidecar published: %v", err)
	}
	still := snapshot(t, b)
	rows, err := still.LoadCloneShingles("new")
	if err != nil || len(rows) != 1 || !slices.Equal(rows["id"], []uint64{2, 1}) {
		t.Fatalf("failed publish changed sidecar: %v, %v", rows, err)
	}
}
