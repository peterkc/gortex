package main

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"slices"
	"sync"
	"testing"

	"gxstudy/adapter"
	"gxstudy/conformance"
	"gxstudy/dataset"
	"gxstudy/runner"
)

func TestConformance(t *testing.T) { conformance.CheckBackend(t, backend{}) }
func TestCatalogConformance(t *testing.T) {
	b := backend{}
	conformance.CheckCatalog(t, b, runner.ReferenceCatalog(b))
}

func artifactHashes(t *testing.T, path string) map[string]string {
	t.Helper()
	hashes := map[string]string{}
	e := filepath.WalkDir(path, func(p string, d os.DirEntry, e error) error {
		if e != nil {
			return e
		}
		if d.IsDir() {
			return nil
		}
		h, e := dataset.Hash(p)
		if e != nil {
			return e
		}
		hashes[p] = h
		return nil
	})
	if e != nil {
		t.Fatal(e)
	}
	return hashes
}
func TestOperationsAndFreshOpen(t *testing.T) {
	ctx := context.Background()
	b := backend{}
	d := conformance.Fixture()
	d.Nodes = append(d.Nodes, dataset.Node{ID: "0", Name: "Alpha", Kind: "function", FilePath: "g.go"})
	d.Edges = append(d.Edges, d.Edges[0])
	ref, stats, e := b.Build(ctx, d, filepath.Join(t.TempDir(), "segment"))
	if e != nil {
		t.Fatal(e)
	}
	t.Logf("build mode=%s bytes=%d", stats.Mode, stats.Bytes)
	before := artifactHashes(t, ref.Path)
	var relativeFiles []string
	for file := range before {
		rel, err := filepath.Rel(ref.Path, file)
		if err != nil {
			t.Fatal(err)
		}
		relativeFiles = append(relativeFiles, rel)
	}
	slices.Sort(relativeFiles)
	t.Logf("sealed artifact files (%d): %v", len(relativeFiles), relativeFiles)
	s, e := b.Open(ctx, ref.Path, ref)
	if e != nil {
		t.Fatal(e)
	}
	var closeOnce sync.Once
	closeSegment := func() {
		closeOnce.Do(func() {
			if e := s.Close(); e != nil {
				t.Error(e)
			}
		})
	}
	defer closeSegment()
	for _, limit := range []int{-1, 0, 1, 4} {
		got, e := s.NodesByName(ctx, "Alpha", limit)
		if e != nil {
			t.Fatal(e)
		}
		want := []string{"0", "a"}
		if limit >= 0 && len(want) > limit {
			want = want[:limit]
		}
		var ids []string
		for _, n := range got {
			ids = append(ids, n.ID)
		}
		if !slices.Equal(ids, want) {
			t.Fatalf("name limit %d got %v want %v", limit, ids, want)
		}
	}
	got, e := s.InEdges(ctx, "a", "calls")
	if e != nil || len(got) != 2 {
		t.Fatalf("duplicate incoming edges: %v %v", got, e)
	}
	for _, row := range got {
		if !reflect.DeepEqual(row, d.Edges[0]) {
			t.Fatal("edge payload changed")
		}
	}
	got, e = s.OutEdges(ctx, "b", "any")
	if e != nil || len(got) != 2 {
		t.Fatalf("duplicate outgoing edges: %v %v", got, e)
	}
	rows, e := s.FileNodes(ctx, "f.go")
	if e != nil || len(rows) != 2 {
		t.Fatal("file nodes", rows, e)
	}
	for _, search := range []func(context.Context, string, int) ([]adapter.Hit, error){s.SearchSymbols, s.SearchContent} {
		for _, q := range []string{"graph", "ALPHA graph", "graph graph", "__absent___"} {
			hits, e := search(ctx, q, 10)
			if e != nil {
				t.Fatal(e)
			}
			want := "a"
			if q == "__absent___" {
				if len(hits) != 0 {
					t.Fatal("missing search returned hits")
				}
				continue
			}
			if len(hits) != 1 || hits[0].ID != want && hits[0].ID != want+"#0" {
				t.Fatalf("query %q: %v", q, hits)
			}
		}
	}
	hits, e := s.VectorTopK(ctx, d.Vectors[0].Values, 1)
	if e != nil || len(hits) != 1 || hits[0].ID != "a" {
		t.Fatal("vector self neighbour", hits, e)
	}
	if _, e = s.VectorTopK(ctx, []float32{1}, 1); e == nil {
		t.Fatal("invalid dimensions accepted")
	}
	// Keep the parent segment open while a fresh process reads the same seal.
	rawRef, err := json.Marshal(ref)
	if err != nil {
		t.Fatal(err)
	}
	concurrentChild := exec.Command(os.Args[0], "-test.run=^TestFreshProcessHelper$", "-test.v")
	concurrentChild.Env = append(os.Environ(), "GX_KV_CHILD="+base64.RawURLEncoding.EncodeToString(rawRef))
	if out, err := concurrentChild.CombinedOutput(); err != nil {
		t.Fatalf("concurrent process reopen: %s: %v", out, err)
	}
	closeSegment()
	after := artifactHashes(t, ref.Path)
	if !reflect.DeepEqual(before, after) {
		t.Fatal("Open/read/Close mutated segment")
	}
	raw, e := json.Marshal(ref)
	if e != nil {
		t.Fatal(e)
	}
	cmd := exec.Command(os.Args[0], "-test.run=^TestFreshProcessHelper$", "-test.v")
	cmd.Env = append(os.Environ(), "GX_KV_CHILD="+base64.RawURLEncoding.EncodeToString(raw))
	out, e := cmd.CombinedOutput()
	if e != nil {
		t.Fatalf("fresh reopen: %s: %v", out, e)
	}
	t.Logf("fresh reopen: %s", out)
	after = artifactHashes(t, ref.Path)
	if !reflect.DeepEqual(before, after) {
		t.Fatal("fresh Open mutated segment")
	}
	bad := ref
	bad.Digest = "invalid"
	if v, e := b.Open(ctx, bad.Path, bad); e == nil {
		v.Close()
		t.Fatal("bad ref accepted")
	}
}
func TestFreshProcessHelper(t *testing.T) {
	value := os.Getenv("GX_KV_CHILD")
	if value == "" {
		t.Skip("subprocess helper")
	}
	raw, e := base64.RawURLEncoding.DecodeString(value)
	if e != nil {
		t.Fatal(e)
	}
	var ref adapter.SegmentRef
	if e = json.Unmarshal(raw, &ref); e != nil {
		t.Fatal(e)
	}
	s, e := (backend{}).Open(context.Background(), ref.Path, ref)
	if e != nil {
		t.Fatal(e)
	}
	defer func() {
		if e := s.Close(); e != nil {
			t.Error(e)
		}
	}()
	n, e := s.NodeByID(context.Background(), "a")
	if e != nil || n.Name != "Alpha" {
		t.Fatal(n, e)
	}
	hits, e := s.SearchSymbols(context.Background(), "graph", 10)
	if e != nil || len(hits) != 1 || hits[0].ID != "a" {
		t.Fatal(hits, e)
	}
}
func TestDeltaAndConcurrentReaders(t *testing.T) {
	ctx := context.Background()
	b := backend{}
	d := conformance.Fixture()
	dir := t.TempDir()
	old, _, e := b.Build(ctx, d, filepath.Join(dir, "old"))
	if e != nil {
		t.Fatal(e)
	}
	replacement := dataset.Dataset{Nodes: []dataset.Node{{ID: "new", Name: "deltaprobe", Kind: "function", FilePath: "f.go"}}, Symbols: []dataset.Symbol{{NodeID: "new", Tokens: "deltaprobe"}}, Content: []dataset.Content{{ChunkID: "new#0", NodeID: "new", FilePath: "f.go", Body: "deltaprobe"}}, Vectors: []dataset.Vector{{NodeID: "new", Values: d.Vectors[0].Values}}, Edges: []dataset.Edge{{From: "new", To: "outside", Kind: "calls", FilePath: "f.go"}}}
	next, stats, e := b.BuildDelta(ctx, old, adapter.FileChange{Path: "f.go", Replacement: replacement}, filepath.Join(dir, "next"))
	if e != nil {
		t.Fatal(e)
	}
	if stats.Mode != "full" {
		t.Fatal(stats.Mode)
	}
	s, e := b.Open(ctx, next.Path, next)
	if e != nil {
		t.Fatal(e)
	}
	defer s.Close()
	oldSegment, e := b.Open(ctx, old.Path, old)
	if e != nil {
		t.Fatal(e)
	}
	defer oldSegment.Close()
	if _, e := oldSegment.NodeByID(ctx, "a"); e != nil {
		t.Fatal("old segment lost", e)
	}
	for _, id := range []string{"a", "b"} {
		if _, e = s.NodeByID(ctx, id); !errors.Is(e, adapter.ErrNotFound) {
			t.Fatal("removed node survived", id, e)
		}
	}
	hits, e := s.SearchSymbols(ctx, "deltaprobe", 10)
	if e != nil || len(hits) != 1 || hits[0].ID != "new" {
		t.Fatal(hits, e)
	}
	hits, e = s.SearchSymbols(ctx, "Alpha", 10)
	if e != nil || len(hits) != 0 {
		t.Fatal("old name searchable", hits, e)
	}
	incoming, e := s.InEdges(ctx, "b", "calls")
	if e != nil || len(incoming) != 1 || incoming[0].From != "outside" {
		t.Fatal("dangling incoming edge lost", incoming, e)
	}
	var wg sync.WaitGroup
	for range 8 {
		wg.Go(func() {
			for range 5 {
				if _, e := s.NodeByID(ctx, "new"); e != nil {
					t.Error(e)
				}
				if h, e := s.SearchSymbols(ctx, "deltaprobe", 1); e != nil || len(h) != 1 || h[0].ID != "new" {
					t.Error(h, e)
				}
				if h, e := s.VectorTopK(ctx, replacement.Vectors[0].Values, 1); e != nil || len(h) != 1 || h[0].ID != "new" {
					t.Error(h, e)
				}
			}
		})
	}
	wg.Wait()
}
