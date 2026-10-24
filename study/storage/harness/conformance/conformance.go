// Package conformance supplies a tiny shared contract test for adapter authors.
package conformance

import (
	"context"
	"encoding/json"
	"errors"
	"gxstudy/adapter"
	"gxstudy/dataset"
	"path/filepath"
	"testing"
)

func Fixture() dataset.Dataset {
	v := make([]float32, 384)
	v[0] = 1
	return dataset.Dataset{Repo: "fixture", Nodes: []dataset.Node{{ID: "a", Name: "Alpha", Kind: "function", FilePath: "f.go", StartLine: 1, EndLine: 2, Language: "go", Payload: map[string]json.RawMessage{"doc": json.RawMessage(`"documentation"`), "signature": json.RawMessage(`"func Alpha()"`), "meta": json.RawMessage(`"AQID"`)}}, {ID: "b", Name: "Beta", Kind: "function", FilePath: "f.go", StartLine: 3, EndLine: 4, Language: "go"}, {ID: "outside", Name: "Outside", Kind: "function", FilePath: "g.go", StartLine: 1, EndLine: 2, Language: "go"}}, Edges: []dataset.Edge{{From: "b", To: "a", Kind: "calls", FilePath: "f.go", Line: 3, Confidence: .75, Payload: map[string]json.RawMessage{"origin": json.RawMessage(`"parser"`)}}, {From: "a", To: "b", Kind: "references", FilePath: "f.go", Line: 1}, {From: "outside", To: "b", Kind: "calls", FilePath: "g.go", Line: 1}}, Files: []dataset.File{{Path: "f.go", ContentHash: "f", Size: 40}, {Path: "g.go", ContentHash: "g", Size: 20}}, Symbols: []dataset.Symbol{{NodeID: "a", Tokens: "Alpha graph"}, {NodeID: "b", Tokens: "Beta node"}}, Content: []dataset.Content{{ChunkID: "a#0", NodeID: "a", FilePath: "f.go", Body: "Alpha graph"}, {ChunkID: "b#0", NodeID: "b", FilePath: "f.go", Body: "Beta node"}}, Vectors: []dataset.Vector{{NodeID: "a", Values: v}, {NodeID: "b", Values: dataset.MakeVector("b", 7).Values}}}
}

// CheckBackend tests shared absence and payload semantics. Run it in the
// candidate module's own tests. Unsupported list operations are not called.
func CheckBackend(t *testing.T, b adapter.Backend) {
	t.Helper()
	ctx := context.Background()
	d := Fixture()
	ref, _, e := b.Build(ctx, d, filepath.Join(t.TempDir(), "segment"))
	if e != nil {
		t.Fatal(e)
	}
	s, e := b.Open(ctx, ref.Path, ref)
	if e != nil {
		t.Fatal(e)
	}
	defer s.Close()
	if _, e = s.NodeByID(ctx, "missing"); !errors.Is(e, adapter.ErrNotFound) {
		t.Fatal("missing ID must return ErrNotFound", e)
	}
	n, e := s.NodeByID(ctx, "a")
	if e != nil {
		t.Fatal(e)
	}
	got, _ := json.Marshal(n)
	want, _ := json.Marshal(d.Nodes[0])
	if string(got) != string(want) {
		t.Fatal("full node payload did not round-trip")
	}
	caps := b.Capabilities()
	if caps.Unsupported["name"] == "" {
		a, e := s.NodesByName(ctx, "missing", 10)
		if e != nil || len(a) != 0 {
			t.Fatal("missing name must return empty,nil", e)
		}
	}
	if caps.Unsupported["file"] == "" {
		a, e := s.FileNodes(ctx, "missing")
		if e != nil || len(a) != 0 {
			t.Fatal("missing file must return empty,nil", e)
		}
	}
	if caps.Unsupported["in"] == "" {
		a, e := s.InEdges(ctx, "missing", "calls")
		if e != nil || len(a) != 0 {
			t.Fatal("missing incoming key must return empty,nil", e)
		}
	}
	if caps.Unsupported["out"] == "" {
		a, e := s.OutEdges(ctx, "missing", "calls")
		if e != nil || len(a) != 0 {
			t.Fatal("missing outgoing key must return empty,nil", e)
		}
	}
}

// CheckCatalog checks CAS absence and complete-ref semantics against sealed refs.
func CheckCatalog(t *testing.T, b adapter.Backend, factory adapter.CatalogFactory) {
	t.Helper()
	ctx := context.Background()
	dir := t.TempDir()
	d := Fixture()
	old, _, err := b.Build(ctx, d, filepath.Join(dir, "old"))
	if err != nil {
		t.Fatal(err)
	}
	d.Nodes = append([]dataset.Node(nil), d.Nodes...)
	d.Nodes[0].Name = "changed"
	next, _, err := b.Build(ctx, d, filepath.Join(dir, "next"))
	if err != nil {
		t.Fatal(err)
	}
	cat, err := factory(filepath.Join(dir, "catalog"))
	if err != nil {
		t.Fatal(err)
	}
	defer cat.Close()
	if err = cat.Publish(ctx, d.Repo, old, nil); err != nil {
		t.Fatal(err)
	}
	if err = cat.Publish(ctx, d.Repo, next, nil); !errors.Is(err, adapter.ErrConflict) {
		t.Fatal("nil old must require absence", err)
	}
	if err = cat.PublishSet(ctx, adapter.View{d.Repo: next}, adapter.View{d.Repo: old, "ignored-extra-key": next}); err != nil {
		t.Fatal("old entries outside set must be ignored", err)
	}
	if err = cat.PublishSet(ctx, adapter.View{d.Repo: old}, adapter.View{}); !errors.Is(err, adapter.ErrConflict) {
		t.Fatal("missing expected key must mean absent", err)
	}
	view, release, err := cat.Pin(ctx, []string{d.Repo})
	if err != nil {
		t.Fatal(err)
	}
	release()
	release()
	if view[d.Repo] != next {
		t.Fatal("failed CAS changed current view")
	}
	previous, err := cat.Previous(ctx, d.Repo)
	if err != nil || previous != old {
		t.Fatal("previous ref lost", previous, err)
	}
}
