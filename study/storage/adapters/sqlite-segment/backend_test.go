package main

import (
	"context"
	"encoding/json"
	"errors"
	"gxstudy/adapter"
	"gxstudy/conformance"
	"gxstudy/dataset"
	"gxstudy/runner"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"sync"
	"testing"
)

func TestConformance(t *testing.T) {
	b := Backend{}
	for _, op := range []string{"symbols", "content"} {
		if b.Capabilities().Ranking[op].Mode != "native" {
			t.Fatalf("%s must declare native scoring", op)
		}
	}
	conformance.CheckBackend(t, b)
}
func TestCatalog(t *testing.T) {
	b := Backend{}
	conformance.CheckCatalog(t, b, runner.ReferenceCatalog(b))
}
func TestServingAndDelta(t *testing.T) {
	ctx := context.Background()
	b := Backend{}
	d := conformance.Fixture()
	d.Edges = append(d.Edges, d.Edges[0])
	ref, stats, err := b.Build(ctx, d, filepath.Join(t.TempDir(), "old"))
	if err != nil {
		t.Fatal(err)
	}
	if stats.Mode != "full" {
		t.Fatal(stats)
	}
	seg, err := b.Open(ctx, ref.Path, ref)
	if err != nil {
		t.Fatal(err)
	}
	defer seg.Close()
	s := seg.(*Segment)
	var version string
	if err = s.db.QueryRowContext(ctx, "select sqlite_version()").Scan(&version); err != nil {
		t.Fatal(err)
	}
	t.Log("engine", version)
	for _, pragma := range []string{"page_size", "mmap_size", "query_only", "cache_size", "journal_mode"} {
		var value any
		if err = s.db.QueryRowContext(ctx, "PRAGMA "+pragma).Scan(&value); err != nil {
			t.Fatal(err)
		}
		t.Log("serving setting", pragma, value)
	}
	rows, err := s.db.QueryContext(ctx, "EXPLAIN QUERY PLAN SELECT raw FROM edges WHERE dst='a' AND kind='calls'")
	if err != nil {
		t.Fatal(err)
	}
	for rows.Next() {
		var id, parent, unused int
		var detail string
		if err = rows.Scan(&id, &parent, &unused, &detail); err != nil {
			t.Fatal(err)
		}
		t.Log("incoming edge plan", detail)
	}
	if err = rows.Err(); err != nil {
		t.Fatal(err)
	}
	if err = rows.Close(); err != nil {
		t.Fatal(err)
	}

	for _, k := range []int{0, 1, -1} {
		nodes, e := s.NodesByName(ctx, "Alpha", k)
		if e != nil {
			t.Fatal(e)
		}
		want := 1
		if k == 0 {
			want = 0
		}
		if len(nodes) != want {
			t.Fatal(k, nodes)
		}
	}
	edges, err := s.OutEdges(ctx, "b", "calls")
	if err != nil || !reflect.DeepEqual(edges, []dataset.Edge{d.Edges[0], d.Edges[0]}) {
		t.Fatal("duplicate/full edge lost", edges, err)
	}
	for _, search := range []func(context.Context, string, int) ([]adapter.Hit, error){s.SearchSymbols, s.SearchContent} {
		hits, e := search(ctx, "graph", 10)
		if e != nil || len(hits) != 1 || hits[0].ID != "a" && hits[0].ID != "a#0" || hits[0].Score <= 0 {
			t.Fatal(hits, e)
		}
		hits, e = search(ctx, "notpresent", 10)
		if e != nil || len(hits) != 0 {
			t.Fatal(hits, e)
		}
		hits, e = search(ctx, "\" OR graph --", 10)
		if e != nil || len(hits) != 1 {
			t.Fatal("untrusted query must be tokenized/quoted", hits, e)
		}
	}
	hits, err := s.VectorTopK(ctx, d.Vectors[0].Values, 2)
	if err != nil || len(hits) != 2 || hits[0].ID != "a" || hits[0].Score != 1 {
		t.Fatal(hits, err)
	}
	if _, err = s.VectorTopK(ctx, []float32{1}, 1); err == nil {
		t.Fatal("accepted wrong vector dimensions")
	}
	if _, err = s.VectorTopK(ctx, make([]float32, 384), 1); err == nil {
		t.Fatal("accepted zero vector")
	}
	if _, err = s.db.ExecContext(ctx, "DELETE FROM nodes"); err == nil {
		t.Fatal("serving connection accepted write")
	}
	before, err := dataset.Hash(filepath.Join(ref.Path, "segment.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Go(func() {
			for j := 0; j < 3; j++ {
				if _, e := s.NodeByID(ctx, "a"); e != nil {
					t.Error(e)
				}
				if _, e := s.SearchContent(ctx, "graph", 1); e != nil {
					t.Error(e)
				}
				if _, e := s.VectorTopK(ctx, d.Vectors[0].Values, 1); e != nil {
					t.Error(e)
				}
			}
		})
	}
	wg.Wait()
	after, err := dataset.Hash(filepath.Join(ref.Path, "segment.sqlite"))
	if err != nil || before != after {
		t.Fatal("Open/read modified segment", err)
	}
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	encoded, err := json.Marshal(ref)
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.CommandContext(ctx, exe, "-test.run=^TestFreshProcessOpen$")
	cmd.Env = append(os.Environ(), "GX_FRESH_REF="+string(encoded))
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("fresh process: %v %s", err, out)
	}
	t.Log("fresh process", string(out))
	replacement := conformance.Fixture()
	replacement.Nodes = []dataset.Node{{ID: "new", Name: "Changed", FilePath: "f.go", Kind: "function"}}
	replacement.Edges = nil
	replacement.Symbols = []dataset.Symbol{{NodeID: "new", Tokens: "deltaprobe"}}
	replacement.Content = []dataset.Content{{ChunkID: "new#0", NodeID: "new", FilePath: "f.go", Body: "deltaprobe"}}
	replacement.Vectors = []dataset.Vector{{NodeID: "new", Values: d.Vectors[0].Values}}
	replacement.Files = replacement.Files[:1]
	next, _, err := b.BuildDelta(ctx, ref, adapter.FileChange{Path: "f.go", Replacement: replacement}, filepath.Join(t.TempDir(), "next"))
	if err != nil {
		t.Fatal(err)
	}
	newer, err := b.Open(ctx, next.Path, next)
	if err != nil {
		t.Fatal(err)
	}
	defer newer.Close()
	if _, err = newer.NodeByID(ctx, "a"); !errors.Is(err, adapter.ErrNotFound) {
		t.Fatal("deleted ID survived", err)
	}
	if _, err = s.NodeByID(ctx, "a"); err != nil {
		t.Fatal("old generation changed", err)
	}
	retained, err := newer.InEdges(ctx, "b", "calls")
	if err != nil || len(retained) != 1 || retained[0].From != "outside" {
		t.Fatal("dangling incoming edge not retained", retained, err)
	}
	hits, err = newer.SearchSymbols(ctx, "deltaprobe", 10)
	if err != nil || len(hits) != 1 || hits[0].ID != "new" {
		t.Fatal(hits, err)
	}
	wrong := ref
	wrong.Digest = "bad"
	if _, err = b.Open(ctx, ref.Path, wrong); err == nil {
		t.Fatal("accepted wrong digest")
	}
	if _, _, err = b.Build(ctx, d, ref.Path); err == nil {
		t.Fatal("overwrote sealed generation")
	}
	entries, err := os.ReadDir(ref.Path)
	if err != nil || len(entries) != 2 {
		t.Fatal("unexpected sidecars", entries, err)
	}
	t.Log("sealed bytes", stats.Bytes, "files", len(entries))
}
func TestFreshProcessOpen(t *testing.T) {
	raw := os.Getenv("GX_FRESH_REF")
	if raw == "" {
		t.Skip("subprocess-only helper")
	}
	var ref adapter.SegmentRef
	if err := json.Unmarshal([]byte(raw), &ref); err != nil {
		t.Fatal(err)
	}
	s, err := (Backend{}).Open(context.Background(), ref.Path, ref)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	n, err := s.NodeByID(context.Background(), "a")
	if err != nil || n.Name != "Alpha" {
		t.Fatal(n, err)
	}
}
