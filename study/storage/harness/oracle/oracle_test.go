package oracle

import (
	"context"
	"errors"
	"fmt"
	"gxstudy/adapter"
	"gxstudy/dataset"
	"gxstudy/workload"
	"path/filepath"
	"reflect"
	"testing"
)

func tiny() dataset.Dataset {
	a := make([]float32, 384)
	a[0] = 1
	b := make([]float32, 384)
	b[1] = 1
	return dataset.Dataset{Repo: "tiny", Nodes: []dataset.Node{{ID: "a", Name: "Alpha", Kind: "function", FilePath: "a.go"}, {ID: "b", Name: "Beta", Kind: "function", FilePath: "b.go"}, {ID: "c", Name: "Alpha", Kind: "function", FilePath: "a.go"}}, Edges: []dataset.Edge{{From: "b", To: "a", Kind: "calls", FilePath: "b.go"}, {From: "c", To: "b", Kind: "calls", FilePath: "a.go"}, {From: "a", To: "c", Kind: "references", FilePath: "a.go"}}, Symbols: []dataset.Symbol{{NodeID: "a", Tokens: "graph graph"}, {NodeID: "b", Tokens: "graph node"}, {NodeID: "c", Tokens: "other"}}, Content: []dataset.Content{{ChunkID: "a#0", NodeID: "a", FilePath: "a.go", Body: "graph graph"}, {ChunkID: "b#0", NodeID: "b", FilePath: "b.go", Body: "node"}}, Vectors: []dataset.Vector{{NodeID: "a", Values: a}, {NodeID: "b", Values: b}}}
}
func TestKnownAnswers(t *testing.T) {
	ctx := context.Background()
	s := New(tiny())
	n, e := s.NodeByID(ctx, "a")
	if e != nil || n.Name != "Alpha" {
		t.Fatal(n, e)
	}
	if _, e = s.NodeByID(ctx, "missing"); !errors.Is(e, adapter.ErrNotFound) {
		t.Fatal("notfound contract", e)
	}
	ns, e := s.NodesByName(ctx, "Alpha", 1)
	if e != nil || len(ns) != 1 || ns[0].ID != "a" {
		t.Fatal(ns, e)
	}
	ns, e = s.FileNodes(ctx, "a.go")
	if e != nil || len(ns) != 2 {
		t.Fatal(ns, e)
	}
	es, e := s.OutEdges(ctx, "a", "references")
	if e != nil || len(es) != 1 || es[0].To != "c" {
		t.Fatal(es, e)
	}
	es, e = s.InEdges(ctx, "a", "calls")
	if e != nil || len(es) != 1 || es[0].From != "b" {
		t.Fatal(es, e)
	}
	ids, e := workload.Traverse(ctx, s, "a", 2)
	if e != nil || !reflect.DeepEqual(ids, []string{"b", "c"}) {
		t.Fatal(ids, e)
	}
	h, e := s.SearchSymbols(ctx, "graph", 2)
	if e != nil || len(h) != 2 || h[0].ID != "a" || h[1].ID != "b" {
		t.Fatal(h, e)
	}
	h, e = s.SearchContent(ctx, "graph", 10)
	if e != nil || len(h) != 1 || h[0].ID != "a#0" {
		t.Fatal(h, e)
	}
	h, e = s.VectorTopK(ctx, tiny().Vectors[0].Values, 2)
	if e != nil || h[0].ID != "a" || h[0].Score != 1 || h[1].ID != "b" || h[1].Score != 0 {
		t.Fatal(h, e)
	}
	ctx, cancel := context.WithCancel(ctx)
	cancel()
	if _, e = s.NodeByID(ctx, "a"); !errors.Is(e, context.Canceled) {
		t.Fatal("cancellation lost")
	}
}
func TestCatalogCASPreviousPinRecover(t *testing.T) {
	ctx := context.Background()
	b := Backend{}
	dir := t.TempDir()
	old, _, e := b.Build(ctx, tiny(), filepath.Join(dir, "old"))
	if e != nil {
		t.Fatal(e)
	}
	d := tiny()
	d.Nodes[0].Name = "changed"
	next, _, e := b.Build(ctx, d, filepath.Join(dir, "new"))
	if e != nil {
		t.Fatal(e)
	}
	cat, e := NewCatalog(filepath.Join(dir, "catalog"))
	if e != nil {
		t.Fatal(e)
	}
	defer cat.Close()
	if e = cat.Publish(ctx, "tiny", old, nil); e != nil {
		t.Fatal(e)
	}
	v, release, e := cat.Pin(ctx, []string{"tiny"})
	if e != nil {
		t.Fatal(e)
	}
	defer release()
	if e = cat.Publish(ctx, "tiny", next, nil); !errors.Is(e, adapter.ErrConflict) {
		t.Fatal("stale CAS accepted", e)
	}
	if e = cat.Publish(ctx, "tiny", next, &old); e != nil {
		t.Fatal(e)
	}
	if v["tiny"] != old {
		t.Fatal("pin changed")
	}
	prev, e := cat.Previous(ctx, "tiny")
	if e != nil || prev != old {
		t.Fatal(prev, e)
	}
	if e = cat.Recover(ctx); e != nil {
		t.Fatal(e)
	}
	unsealed := next
	unsealed.Path = filepath.Join(dir, "absent")
	if e = cat.Publish(ctx, "tiny", unsealed, &next); e == nil {
		t.Fatal("unsealed segment published")
	}
	got, done, e := cat.Pin(ctx, []string{"tiny"})
	if e != nil {
		t.Fatal(e)
	}
	done()
	if got["tiny"] != next {
		t.Fatal("rejection changed visible pointer")
	}
}

func TestGlobalIDFNotLocalMerge(t *testing.T) {
	a := dataset.Dataset{Repo: "a"}
	b := dataset.Dataset{Repo: "b"}
	for i := 0; i < 10; i++ {
		text := "graph other words text node"
		if i == 0 {
			text = "graph graph graph graph graph"
		}
		a.Symbols = append(a.Symbols, dataset.Symbol{NodeID: fmt.Sprintf("a%d", i), Tokens: text})
		text = "other words text node tokens"
		if i == 0 {
			text = "graph"
		}
		b.Symbols = append(b.Symbols, dataset.Symbol{NodeID: fmt.Sprintf("b%d", i), Tokens: text})
	}
	segments := map[string]adapter.Segment{"a": New(a), "b": New(b)}
	local, err := (adapter.Fanout{Segments: segments}).SearchSymbols(context.Background(), "graph", 1)
	if err != nil || local[0].ID != "b0" {
		t.Fatal(local, err)
	}
	global, err := NewMulti([]dataset.Dataset{a, b}, segments).SearchSymbols(context.Background(), "graph", 1)
	if err != nil || global[0].ID != "a0" {
		t.Fatal(global, err)
	}
}
