package main

import (
	"flag"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"gxstudy/dataset"
)

func TestSyntheticCrossRepoFlag(t *testing.T) {
	inputs := []dataset.Dataset{}
	var sources []string
	owners := map[string]string{}
	for _, repo := range []string{"a", "b"} {
		d := dataset.Dataset{Repo: repo, Meta: dataset.Manifest{Namespace: repo}}
		for _, name := range []string{"First", "Second"} {
			id := repo + "::" + name
			d.Nodes = append(d.Nodes, dataset.Node{ID: id, Kind: "function", Name: name, FilePath: "fixture.go", Language: "go", StartLine: 1, EndLine: 1})
			owners[id] = repo
		}
		for range 200 {
			d.Edges = append(d.Edges, dataset.Edge{From: repo + "::First", To: repo + "::Second", Kind: "calls", FilePath: "fixture.go", Confidence: .75})
		}
		other := "a"
		if repo == "a" {
			other = "b"
		}
		d.Edges = append(d.Edges, dataset.Edge{From: repo + "::First", To: other + "::Second", Kind: "calls", CrossRepo: true, Confidence: .9})
		dir := filepath.Join(t.TempDir(), repo)
		if _, err := dataset.Write(dir, d); err != nil {
			t.Fatal(err)
		}
		inputs = append(inputs, d)
		sources = append(sources, dir)
	}
	countCrossings := func(ds []dataset.Dataset) int {
		count := 0
		for _, d := range ds {
			for _, edge := range d.Edges {
				if owners[edge.From] != owners[edge.To] {
					count++
				}
			}
		}
		return count
	}
	sourceCrossings := countCrossings(inputs)
	if sourceCrossings != 2 {
		t.Fatalf("fixture has %d owned crossings, want 2", sourceCrossings)
	}
	generate := func(t *testing.T, args ...string) ([]dataset.Dataset, dataset.Manifest) {
		t.Helper()
		oldFlags, oldArgs := flag.CommandLine, os.Args
		defer func() { flag.CommandLine, os.Args = oldFlags, oldArgs }()
		flag.CommandLine = flag.NewFlagSet("gen", flag.ContinueOnError)
		flag.CommandLine.SetOutput(io.Discard)
		out := filepath.Join(t.TempDir(), "tier")
		os.Args = append([]string{"gen", "-sources", strings.Join(sources, ","), "-out", out, "-repos", "2"}, args...)
		if err := run(); err != nil {
			t.Fatal(err)
		}
		var manifest dataset.Manifest
		if err := dataset.ReadJSON(filepath.Join(out, "manifest.json"), &manifest); err != nil {
			t.Fatal(err)
		}
		var generated []dataset.Dataset
		for _, input := range inputs {
			d, err := dataset.Load(filepath.Join(out, input.Repo))
			if err != nil {
				t.Fatal(err)
			}
			generated = append(generated, d)
		}
		return generated, manifest
	}
	t.Run("source-only", func(t *testing.T) {
		ds, manifest := generate(t, "-synthetic-cross-repo=false")
		if got := countCrossings(ds); got != sourceCrossings {
			t.Fatalf("owned crossings = %d, want source count %d", got, sourceCrossings)
		}
		if manifest.Counts.CrossEdges != sourceCrossings || manifest.CrossRepoPolicy != "" {
			t.Fatalf("source-only tier advertises synthetic links: %+v", manifest)
		}
		for i, d := range ds {
			if !reflect.DeepEqual(d.Edges, inputs[i].Edges) || d.Meta.CrossRepoPolicy != "" {
				t.Fatalf("source-only repository %s changed its edges or synthetic policy", d.Repo)
			}
		}
	})
	t.Run("default-and-explicit-on", func(t *testing.T) {
		defaultDS, defaultManifest := generate(t)
		onDS, onManifest := generate(t, "-synthetic-cross-repo=true")
		if got := countCrossings(onDS); got != sourceCrossings+4 {
			t.Fatalf("enabled crossings = %d, want %d", got, sourceCrossings+4)
		}
		if onManifest.Counts.CrossEdges != sourceCrossings+4 || onManifest.CrossRepoPolicy != dataset.CrossRepoPolicy || !reflect.DeepEqual(defaultManifest, onManifest) {
			t.Fatal("default and explicit-on synthetic manifests differ or lack synthetic policy")
		}
		for i, d := range onDS {
			if !reflect.DeepEqual(d.Edges, defaultDS[i].Edges) || d.Meta.CrossRepoPolicy != dataset.CrossRepoPolicy {
				t.Fatalf("default and explicit-on edges or policy differ for %s", d.Repo)
			}
		}
	})
}
