package dataset

import (
	"encoding/json"
	"math"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"testing"
)

func diskFixture() Dataset {
	return Dataset{Repo: "r", Nodes: []Node{{ID: "a", Name: "Alpha", FilePath: "x.go", Payload: map[string]json.RawMessage{"doc": json.RawMessage(`"opaque"`)}}, {ID: "b", Name: "Beta"}}, Edges: []Edge{{From: "a", To: "b", Kind: "calls"}, {From: "a", To: "b", Kind: "calls"}}, Files: []File{{Path: "x.go"}}, Symbols: []Symbol{{NodeID: "a", Tokens: "graph"}}, Content: []Content{{ChunkID: "a#0", NodeID: "a", Body: "graph"}}, Vectors: []Vector{MakeVector("a", 7)}}
}
func TestDiskSourceRoundTripAndOrder(t *testing.T) {
	for _, reverse := range []bool{false, true} {
		t.Run(map[bool]string{false: "original", true: "reordered"}[reverse], func(t *testing.T) {
			d := diskFixture()
			if reverse {
				slices.Reverse(d.Nodes)
				slices.Reverse(d.Edges)
			}
			root := t.TempDir()
			if _, err := Write(root, d); err != nil {
				t.Fatal(err)
			}
			source, err := OpenSource(root)
			if err != nil {
				t.Fatal(err)
			}
			if source.Source == nil || len(source.Nodes)+len(source.Edges)+len(source.Vectors) != 0 {
				t.Fatal("source retained records")
			}
			copyDir := filepath.Join(t.TempDir(), "copy")
			if _, err = Write(copyDir, source); err != nil {
				t.Fatal(err)
			}
			got, err := Load(copyDir)
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(got.Nodes, d.Nodes) || !reflect.DeepEqual(got.Edges, d.Edges) || !reflect.DeepEqual(got.Vectors, d.Vectors) {
				t.Fatal("disk iteration changed payload, order or multiplicity")
			}
		})
	}
}
func TestDiskSourceRejectsMalformedAndAmbiguousInputs(t *testing.T) {
	for _, kind := range []string{"duplicate-node", "duplicate-symbol", "duplicate-content", "duplicate-vector", "nonfinite", "truncated-json", "truncated-vector", "digest", "symlink", "manifest-symlink"} {
		t.Run(kind, func(t *testing.T) {
			root := t.TempDir()
			d := diskFixture()
			switch kind {
			case "duplicate-node":
				d.Nodes = append(d.Nodes, d.Nodes[0])
			case "duplicate-symbol":
				d.Symbols = append(d.Symbols, d.Symbols[0])
			case "duplicate-content":
				d.Content = append(d.Content, d.Content[0])
			case "duplicate-vector":
				d.Vectors = append(d.Vectors, d.Vectors[0])
			case "nonfinite":
				d.Vectors[0].Values[0] = float32(math.NaN())
			}
			meta, err := Write(root, d)
			if err != nil {
				t.Fatal(err)
			}
			name := "nodes.jsonl"
			switch kind {
			case "truncated-json":
				if err = os.WriteFile(filepath.Join(root, name), []byte(`{"id":"a"}`), 0600); err != nil {
					t.Fatal(err)
				}
			case "truncated-vector":
				name = "vectors.bin"
				if err = os.WriteFile(filepath.Join(root, name), []byte{1, 0, 0}, 0600); err != nil {
					t.Fatal(err)
				}
			case "digest":
				if err = os.WriteFile(filepath.Join(root, name), []byte("{}\n"), 0600); err != nil {
					t.Fatal(err)
				}
			case "symlink", "manifest-symlink":
				if kind == "manifest-symlink" {
					name = "manifest.json"
				}
				external := filepath.Join(t.TempDir(), "external")
				encoded, err := os.ReadFile(filepath.Join(root, name))
				if err != nil {
					t.Fatal(err)
				}
				if err = os.WriteFile(external, encoded, 0600); err != nil {
					t.Fatal(err)
				}
				if err = os.Remove(filepath.Join(root, name)); err != nil {
					t.Fatal(err)
				}
				if err = os.Symlink(external, filepath.Join(root, name)); err != nil {
					t.Fatal(err)
				}
			}
			if kind == "truncated-json" || kind == "truncated-vector" {
				meta.SHA256[name], err = Hash(filepath.Join(root, name))
				if err != nil {
					t.Fatal(err)
				}
				if err = WriteJSON(filepath.Join(root, "manifest.json"), meta); err != nil {
					t.Fatal(err)
				}
			}
			if _, err = OpenSource(root); err == nil {
				t.Fatal("accepted", kind)
			}
		})
	}
}
func TestSafeFileRejectsAliases(t *testing.T) {
	for _, name := range []string{"../escape", "a/../nodes.jsonl", "./nodes.jsonl", "/absolute", "."} {
		if _, err := SafeFile(t.TempDir(), name); err == nil {
			t.Fatal("accepted alias", name)
		}
	}
}
