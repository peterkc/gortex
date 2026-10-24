package dataset

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

func fixture() Dataset {
	return Dataset{Repo: "tiny", Meta: Manifest{Seed: 7, SourceRevision: "fixture"}, Nodes: []Node{{ID: "a", Kind: "function", Name: "Alpha", FilePath: "a.go", StartLine: 1, EndLine: 2}, {ID: "b", Kind: "method", Name: "Beta", FilePath: "b.go", StartLine: 1, EndLine: 2}}, Edges: []Edge{{From: "a", To: "b", Kind: "calls", FilePath: "a.go", Line: 2}}, Files: []File{{Path: "a.go", ContentHash: "sha-a", Size: 10}, {Path: "b.go", ContentHash: "sha-b", Size: 20}}, Symbols: []Symbol{{NodeID: "a", Tokens: "alpha graph"}}, Content: []Content{{ChunkID: "a#0", NodeID: "a", FilePath: "a.go", Body: "graph node"}}, Vectors: []Vector{MakeVector("a", 7), MakeVector("b", 7)}}
}
func TestRoundTrip(t *testing.T) {
	d := fixture()
	dir := t.TempDir()
	m, e := Write(dir, d)
	if e != nil {
		t.Fatal(e)
	}
	got, e := Load(dir)
	if e != nil {
		t.Fatal(e)
	}
	d.Meta = m
	if !reflect.DeepEqual(d, got) {
		t.Fatal("roundtrip changed records")
	}
	if e = os.WriteFile(filepath.Join(dir, "nodes.jsonl"), []byte("{}\n"), 0644); e != nil {
		t.Fatal(e)
	}
	if _, e = Load(dir); e == nil {
		t.Fatal("corrupt file accepted")
	}
}
func TestGeneratorDeterminism(t *testing.T) {
	a := Derive(fixture(), "r", .5, 42)
	b := Derive(fixture(), "r", .5, 42)
	ma, e := Write(t.TempDir(), a)
	if e != nil {
		t.Fatal(e)
	}
	mb, e := Write(t.TempDir(), b)
	if e != nil {
		t.Fatal(e)
	}
	if !reflect.DeepEqual(ma, mb) {
		t.Fatal("same seed gave different file sha256")
	}
	if len(a.Files) != 1 || len(a.Nodes) != 1 {
		t.Fatal("file subset not applied")
	}
	if len(Derive(fixture(), "x", 1, 42).Edges) != 1 {
		t.Fatal("full tier lost edges")
	}
}
func TestVectorNorm(t *testing.T) {
	v := MakeVector("node", 9)
	sum := 0.
	for _, x := range v.Values {
		sum += float64(x) * float64(x)
	}
	if sum < .999999 || sum > 1.000001 {
		t.Fatalf("norm squared %g", sum)
	}
}
