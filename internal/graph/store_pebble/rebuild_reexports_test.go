package store_pebble

import (
	"errors"
	"path"
	"testing"

	"github.com/cockroachdb/pebble/v2"
	"github.com/zzet/gortex/internal/graph"
)

func TestRebuildReexportsFailureCleanup(t *testing.T) {
	ctx := t.Context()
	b := newBuild(t, FullProfile)
	v := b.RebuildView()
	seed := &graph.Node{ID: "r/main.ts", Kind: graph.KindFile, FilePath: "r/main.ts", RepoPrefix: "r"}
	b.AddNode(seed)
	prefix := []byte{familyEdge, rebuildReexportColumn}
	assertClean := func() {
		t.Helper()
		if err := pendingVisit(ctx, b.pending, prefix, func(row) error { return errors.New("left a scratch row") }); err != nil {
			t.Fatal(err)
		}
		n, err := v.Node(ctx, seed.ID)
		if err != nil || n == nil || n.FilePath != seed.FilePath {
			t.Fatalf("scratch cleanup changed input: %v, %v", n, err)
		}
	}
	withClosure := func(consume func(*RebuildImportClosure) error) (RebuildReexportStats, error) {
		return v.WithImportClosure(ctx, nil, func(string) bool { return false }, path.Dir, consume)
	}
	bad := pendingEdgeKey(&graph.Edge{From: seed.ID, To: "target.ts", Kind: graph.EdgeReExports})
	if err := b.pending.Set(bad, []byte("corrupt"), pebble.NoSync); err != nil {
		t.Fatal(err)
	}
	stats, err := withClosure(func(*RebuildImportClosure) error {
		t.Error("consumed closure after a corrupt saved edge")
		return nil
	})
	if !errors.Is(err, ErrRebuildReexports) || !errors.Is(err, ErrRebuildView) || stats != (RebuildReexportStats{}) {
		t.Fatalf("corrupt saved edge = %+v, %v", stats, err)
	}
	assertClean()
	if err := b.pending.Delete(bad, pebble.NoSync); err != nil {
		t.Fatal(err)
	}
	refuse := errors.New("guard refused to consume closure")
	var expired *RebuildImportClosure
	stats, err = withClosure(func(c *RebuildImportClosure) error {
		expired = c
		if found, err := c.Reachable(ctx, seed.FilePath, "r"); !found || err != nil {
			t.Errorf("completed result unavailable: %t, %v", found, err)
		}
		return refuse
	})
	if !errors.Is(err, ErrRebuildReexports) || !errors.Is(err, refuse) || stats != (RebuildReexportStats{}) {
		t.Fatalf("consumer refusal = %+v, %v", stats, err)
	}
	assertClean()
	if expired == nil {
		t.Fatal("callback did not receive the completed result")
	}
	if found, err := expired.Reachable(ctx, seed.FilePath, "r"); found || !errors.Is(err, ErrRebuildReexports) {
		t.Fatalf("expired result = %t, %v", found, err)
	}
	flushed := b.pending.Metrics().Levels[0].TableBytesFlushed
	stats, err = withClosure(func(*RebuildImportClosure) error { return nil })
	if err != nil || stats.Directories != 1 {
		t.Fatalf("fresh pass after refusal = %+v, %v", stats, err)
	}
	if after := b.pending.Metrics().Levels[0].TableBytesFlushed; after != flushed {
		t.Fatalf("small import closure flushed pending table bytes: before=%d, after=%d", flushed, after)
	}
	assertClean()
}
