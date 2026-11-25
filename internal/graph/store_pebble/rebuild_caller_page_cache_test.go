package store_pebble

import (
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/zzet/gortex/internal/graph"
)

func TestRebuildV3CallerPageCache(t *testing.T) {
	b := newBuild(t, FullProfile)
	node := &graph.Node{ID: "caller", FilePath: "repo/use.go", Meta: map[string]any{"raw": []int{1, 2}}}
	b.AddNode(node)
	v := b.RebuildView().WithCallerPageCache()
	ctx := t.Context()
	n, err := v.Node(ctx, node.ID)
	if err != nil || !reflect.DeepEqual(node, n) {
		t.Fatalf("full=%+v err=%v", n, err)
	}
	again, err := v.Node(ctx, node.ID)
	if err != nil || again != n {
		t.Fatal("caller full row missed cache")
	}
	snap := b.pending.NewSnapshot()
	defer snap.Close()
	key := pendingNodeKey(node.ID)
	raw, err := encodeNode(node)
	if err != nil {
		t.Fatal(err)
	}
	charged := 0
	for range 2 {
		light, err := v.callerPage.read(snap, node.ID, key, true, func(bytes int) error { charged += bytes; return nil })
		if err != nil || light.ID != node.ID || light.Meta != nil {
			t.Fatalf("light=%+v err=%v", light, err)
		}
	}
	if charged != 2*len(raw) {
		t.Fatalf("logical value charge=%d want=%d", charged, 2*len(raw))
	}
	rejected := errors.New("fixture logical charge exhausted")
	light, err := v.callerPage.read(snap, node.ID, key, true, func(int) error { return rejected })
	if light != nil || !errors.Is(err, rejected) {
		t.Fatalf("light hit bypassed charge: %v", err)
	}
	n.Meta["raw"].([]int)[0] = 9
	if err := v.ResetCallerPage(); err == nil || !strings.Contains(err.Error(), "cached raw caller") {
		t.Fatalf("mutation not reported: %v", err)
	}
	fresh, err := v.Node(ctx, node.ID)
	if err != nil || !reflect.DeepEqual(node, fresh) {
		t.Fatalf("raw node altered: %+v %v", fresh, err)
	}
	if err := v.ResetCallerPage(); err != nil {
		t.Fatal(err)
	}
}
