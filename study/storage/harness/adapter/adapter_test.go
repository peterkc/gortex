package adapter

import (
	"gxstudy/dataset"
	"reflect"
	"testing"
)

func TestApplyChangeHandComputed(t *testing.T) {
	v := make([]float32, 384)
	v[0] = 1
	base := dataset.Dataset{Repo: "r", Nodes: []dataset.Node{{ID: "gone", FilePath: "a.go"}, {ID: "outside", FilePath: "b.go"}}, Edges: []dataset.Edge{{From: "gone", To: "outside", Kind: "calls", FilePath: "a.go"}, {From: "outside", To: "gone", Kind: "calls", FilePath: "b.go"}}, Files: []dataset.File{{Path: "a.go"}, {Path: "b.go"}}, Symbols: []dataset.Symbol{{NodeID: "gone", Tokens: "old"}, {NodeID: "outside", Tokens: "keep"}}, Content: []dataset.Content{{ChunkID: "gone#0", NodeID: "gone", FilePath: "a.go", Body: "old"}}, Vectors: []dataset.Vector{{NodeID: "gone", Values: v}}}
	replacement := dataset.Dataset{Nodes: []dataset.Node{{ID: "added", FilePath: "a.go", Name: "new"}}, Edges: []dataset.Edge{{From: "added", To: "outside", Kind: "references", FilePath: "a.go"}}, Files: []dataset.File{{Path: "a.go", Size: 42}}, Symbols: []dataset.Symbol{{NodeID: "added", Tokens: "new"}}}
	got := ApplyChange(base, FileChange{Path: "a.go", Replacement: replacement})
	expected := dataset.Dataset{Repo: "r", Nodes: []dataset.Node{{ID: "outside", FilePath: "b.go"}, {ID: "added", FilePath: "a.go", Name: "new"}}, Edges: []dataset.Edge{{From: "outside", To: "gone", Kind: "calls", FilePath: "b.go"}, {From: "added", To: "outside", Kind: "references", FilePath: "a.go"}}, Files: []dataset.File{{Path: "b.go"}, {Path: "a.go", Size: 42}}, Symbols: []dataset.Symbol{{NodeID: "outside", Tokens: "keep"}, {NodeID: "added", Tokens: "new"}}}
	if !reflect.DeepEqual(got, expected) {
		t.Fatalf("hand-computed change differs\ngot=%+v\nwant=%+v", got, expected)
	}
	if len(base.Nodes) != 2 || base.Nodes[0].ID != "gone" || len(base.Edges) != 2 {
		t.Fatal("base mutated")
	}
}
