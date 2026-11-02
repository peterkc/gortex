package oracle

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"gxstudy/adapter"
	"gxstudy/dataset"
	"gxstudy/workload"
	"path/filepath"
	"slices"
	"testing"
)

func TestDiskOracleParityAllQueryCallers(t *testing.T) {
	ctx := context.Background()
	d := tiny()
	d.Edges = append(d.Edges, d.Edges[0])
	d.Nodes[0].Payload = map[string]json.RawMessage{"opaque": json.RawMessage(`{"x":[1,null]}`)}
	for _, reverse := range []bool{false, true} {
		if reverse {
			slices.Reverse(d.Nodes)
			slices.Reverse(d.Edges)
		}
		dir := t.TempDir()
		if _, err := dataset.Write(dir, d); err != nil {
			t.Fatal(err)
		}
		source, err := dataset.OpenSource(dir)
		if err != nil {
			t.Fatal(err)
		}
		disk := &DiskSegment{Data: source}
		memory := New(d)
		qs := []workload.Query{{Op: "id", Key: "a"}, {Op: "id", Key: "absent"}, {Op: "name", Key: "Alpha", K: -1}, {Op: "name", Key: "Alpha", K: 0}, {Op: "name", Key: "Alpha", K: 1}, {Op: "name", Key: "absent", K: 5}, {Op: "file", Key: "a.go"}, {Op: "file", Key: "absent"}, {Op: "out", Key: "b", Kind: "any"}, {Op: "in", Key: "a", Kind: "calls"}, {Op: "in", Key: "absent"}, {Op: "traverse2", Key: "a", Depth: 2}, {Op: "traverse3", Key: "a", Depth: 3}, {Op: "symbols", Key: "GRAPH node graph", K: 2}, {Op: "symbols", Key: "absent", K: 2}, {Op: "content", Key: "graph", K: 1}, {Op: "vector", Vec: d.Vectors[0].Values, K: 1}}
		for i, q := range qs {
			q.Repo = d.Repo
			q.ID = string(rune('A' + i))
			want, e := workload.Evaluate(ctx, map[string]adapter.Segment{d.Repo: memory}, nil, q)
			if e != nil {
				t.Fatal(e)
			}
			got, e := workload.Evaluate(ctx, map[string]adapter.Segment{d.Repo: disk}, nil, q)
			if e != nil {
				t.Fatal(e)
			}
			a, _ := json.Marshal(workload.Canonical(want))
			b, _ := json.Marshal(workload.Canonical(got))
			if string(a) != string(b) {
				t.Fatalf("%s %s: %s != %s", q.Op, q.Key, a, b)
			}
		}
		cancelled, cancel := context.WithCancel(ctx)
		cancel()
		if _, e := disk.SearchSymbols(cancelled, "graph", 10); !errors.Is(e, context.Canceled) {
			t.Fatal("cancellation lost", e)
		}
		if _, e := disk.VectorTopK(ctx, []float32{1}, 10); e == nil {
			t.Fatal("bad dimensions accepted")
		}
	}
}
func TestDiskReferenceNativeMembershipAndCutoff(t *testing.T) {
	d := tiny()
	dir := t.TempDir()
	if _, err := dataset.Write(dir, d); err != nil {
		t.Fatal(err)
	}
	source, err := dataset.OpenSource(dir)
	if err != nil {
		t.Fatal(err)
	}
	q := workload.Query{ID: "native", Op: "symbols", Repo: d.Repo, Key: "graph", K: 1, NativeScoring: true}
	expected, err := DiskReference(context.Background(), []dataset.Dataset{source}, q, []string{"a", "b", "c", "made-up"})
	if err != nil {
		t.Fatal(err)
	}
	if len(expected.MatchRepos) != 2 || expected.MatchRepos["a"] != d.Repo || expected.MatchRepos["b"] != d.Repo {
		t.Fatal("candidate membership not checked", expected.MatchRepos)
	}
	q.MatchRepos = expected.MatchRepos
	if !workload.Compare(q, expected.Answer, workload.Answer{Hits: []adapter.Hit{{ID: "c", Score: 1}}}).Mismatch {
		t.Fatal("nonmatching native hit accepted")
	}
}
func TestDiskBuildOpenDeltaCatalogRoundTrip(t *testing.T) {
	ctx := context.Background()
	d := tiny()
	root := t.TempDir()
	input := filepath.Join(root, "input")
	if _, err := dataset.Write(input, d); err != nil {
		t.Fatal(err)
	}
	source, err := dataset.OpenSource(input)
	if err != nil {
		t.Fatal(err)
	}
	b := Backend{}
	old, _, err := b.Build(ctx, source, filepath.Join(root, "base"))
	if err != nil {
		t.Fatal(err)
	}
	replacement := dataset.Dataset{Repo: d.Repo, Nodes: []dataset.Node{{ID: "new", Name: "New", FilePath: "a.go"}}}
	change := adapter.FileChange{Path: "a.go", Replacement: replacement}
	next, _, err := b.BuildDelta(ctx, old, change, filepath.Join(root, "next"))
	if err != nil {
		t.Fatal(err)
	}
	opened, err := b.Open(ctx, next.Path, next)
	if err != nil {
		t.Fatal(err)
	}
	defer opened.Close()
	if _, err = opened.NodeByID(ctx, "a"); !errors.Is(err, adapter.ErrNotFound) {
		t.Fatal("removed node survives", err)
	}
	if n, e := opened.NodeByID(ctx, "new"); e != nil || n.Name != "New" {
		t.Fatal(n, e)
	}
	incoming, e := opened.InEdges(ctx, "a", "calls")
	if e != nil || len(incoming) != 1 {
		t.Fatal("dangling external incoming edge lost", incoming, e)
	}
	cat, err := NewCatalog(filepath.Join(root, "catalog"))
	if err != nil {
		t.Fatal(err)
	}
	if err = cat.Publish(ctx, d.Repo, old, nil); err != nil {
		t.Fatal(err)
	}
	if err = cat.Publish(ctx, d.Repo, next, &old); err != nil {
		t.Fatal(err)
	}
	if err = cat.Close(); err != nil {
		t.Fatal(err)
	}
	cat, err = NewCatalog(filepath.Join(root, "catalog"))
	if err != nil {
		t.Fatal(err)
	}
	defer cat.Close()
	if err = cat.Recover(ctx); err != nil {
		t.Fatal(err)
	}
	view, release, err := cat.Pin(ctx, []string{d.Repo})
	if err != nil {
		t.Fatal(err)
	}
	release()
	if view[d.Repo] != next {
		t.Fatal("restart changed view")
	}
}

func TestDiskMultiReferencesMatchLocalAndGlobalPolicies(t *testing.T) {
	ctx := context.Background()
	var ds, sources []dataset.Dataset
	memory := map[string]adapter.Segment{}
	for _, id := range []string{"one", "two"} {
		d := dataset.Derive(tiny(), id, 1, 7)
		ds = append(ds, d)
		memory[id] = New(d)
		dir := t.TempDir()
		if _, err := dataset.Write(dir, d); err != nil {
			t.Fatal(err)
		}
		source, err := dataset.OpenSource(dir)
		if err != nil {
			t.Fatal(err)
		}
		sources = append(sources, source)
	}
	disk, globalDisk := DiskView(sources)
	globalMemory := NewMulti(ds, memory)
	for _, op := range []string{"multi_name", "multi_in", "multi_symbols", "multi_vector", "multi_traverse2"} {
		q := workload.Query{ID: op, Op: op, Key: "graph", K: 3, Depth: 2, Vec: ds[0].Vectors[0].Values}
		if op == "multi_name" {
			q.Key = "Alpha"
		}
		if op == "multi_in" || op == "multi_traverse2" {
			q.Key = "one::a"
			q.Kind = "any"
		}
		for _, global := range []bool{false, true} {
			var gotMulti, wantMulti adapter.MultiRepo = adapter.Fanout{Segments: disk}, adapter.Fanout{Segments: memory}
			if global {
				gotMulti = globalDisk
				wantMulti = globalMemory
			}
			want, err := workload.Evaluate(ctx, memory, wantMulti, q)
			if err != nil {
				t.Fatal(err)
			}
			got, err := workload.Evaluate(ctx, disk, gotMulti, q)
			if err != nil {
				t.Fatal(err)
			}
			a, _ := json.Marshal(workload.Canonical(want))
			b, _ := json.Marshal(workload.Canonical(got))
			if string(a) != string(b) {
				t.Fatalf("multi parity %s global=%v: %s != %s", op, global, a, b)
			}
		}
	}
}
func TestDiskDeltaNoopRemoveReaddAndRebuild(t *testing.T) {
	ctx := context.Background()
	d := tiny()
	b := Backend{}
	root := t.TempDir()
	input := filepath.Join(root, "input")
	if _, err := dataset.Write(input, d); err != nil {
		t.Fatal(err)
	}
	source, err := dataset.OpenSource(input)
	if err != nil {
		t.Fatal(err)
	}
	current, _, err := b.Build(ctx, source, filepath.Join(root, "base"))
	if err != nil {
		t.Fatal(err)
	}
	replacement := dataset.Dataset{Repo: d.Repo}
	owned := map[string]bool{}
	for _, n := range d.Nodes {
		if n.FilePath == "a.go" {
			replacement.Nodes = append(replacement.Nodes, n)
			owned[n.ID] = true
		}
	}
	for _, e := range d.Edges {
		if e.FilePath == "a.go" || owned[e.From] {
			replacement.Edges = append(replacement.Edges, e)
		}
	}
	for _, v := range d.Symbols {
		if owned[v.NodeID] {
			replacement.Symbols = append(replacement.Symbols, v)
		}
	}
	for _, v := range d.Content {
		if v.FilePath == "a.go" {
			replacement.Content = append(replacement.Content, v)
		}
	}
	for _, v := range d.Vectors {
		if owned[v.NodeID] {
			replacement.Vectors = append(replacement.Vectors, v)
		}
	}
	expected := d
	for i, next := range []dataset.Dataset{replacement, {Repo: d.Repo}, replacement} {
		change := adapter.FileChange{Path: "a.go", Replacement: next}
		expected = adapter.ApplyChange(expected, change)
		current, _, err = b.BuildDelta(ctx, current, change, filepath.Join(root, fmt.Sprintf("delta-%d", i)))
		if err != nil {
			t.Fatal(err)
		}
		opened, err := b.Open(ctx, current.Path, current)
		if err != nil {
			t.Fatal(err)
		}
		for _, q := range []workload.Query{{Op: "id", Repo: d.Repo, Key: "a"}, {Op: "file", Repo: d.Repo, Key: "a.go"}, {Op: "in", Repo: d.Repo, Key: "a", Kind: "calls"}, {Op: "symbols", Repo: d.Repo, Key: "graph", K: 10}} {
			want, e := workload.Evaluate(ctx, map[string]adapter.Segment{d.Repo: New(expected)}, nil, q)
			if e != nil {
				t.Fatal(e)
			}
			got, e := workload.Evaluate(ctx, map[string]adapter.Segment{d.Repo: opened}, nil, q)
			if e != nil {
				t.Fatal(e)
			}
			a, _ := json.Marshal(workload.Canonical(want))
			c, _ := json.Marshal(workload.Canonical(got))
			if string(a) != string(c) {
				t.Fatal("follow-on parity", i, q.Op, string(a), string(c))
			}
		}
		if err = opened.Close(); err != nil {
			t.Fatal(err)
		}
	}
	sealed, err := dataset.OpenSource(current.Path)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err = b.Build(ctx, sealed, filepath.Join(root, "rebuild")); err != nil {
		t.Fatal(err)
	}
}
