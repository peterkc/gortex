package main

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"gxstudy/adapter"
	"gxstudy/dataset"
	"gxstudy/oracle"
	"gxstudy/runner"
	"gxstudy/workload"
)

func TestTinyFixtureIsCompleteDeterministicAndSupportsDelta(t *testing.T) {
	revision := strings.Repeat("a", 40)
	out := filepath.Join(t.TempDir(), "tiny")
	if err := run([]string{"-out", out, "-source-revision", revision}, io.Discard); err != nil {
		t.Fatal(err)
	}
	d, err := dataset.OpenSource(filepath.Join(out, "fixture"))
	if err != nil {
		t.Fatal(err)
	}
	if d.Meta.SourceRevision != revision || d.Meta.Origin != "synthetic" || d.Meta.Counts.Nodes != 3 || d.Meta.Counts.Edges != 3 || d.Meta.Counts.Vectors != 2 {
		t.Fatal("fixture provenance or counts differ", d.Meta)
	}
	if _, err := runner.MakeChange(d); err != nil {
		t.Fatal("tiny fixture does not support full delta protocol", err)
	}
	var meta dataset.Manifest
	if err := dataset.ReadJSON(filepath.Join(out, "manifest.json"), &meta); err != nil {
		t.Fatal(err)
	}
	for name, want := range meta.SHA256 {
		got, err := dataset.Hash(filepath.Join(out, name))
		if err != nil || got != want {
			t.Fatal("unbound fixture artifact", name, err)
		}
	}
	var queries []workload.Query
	var expected []workload.Expected
	if err := dataset.ReadJSON(filepath.Join(out, "queries.json"), &queries); err != nil {
		t.Fatal(err)
	}
	if err := dataset.ReadJSON(filepath.Join(out, "expected.json"), &expected); err != nil {
		t.Fatal(err)
	}
	if len(queries) == 0 || len(queries) != len(expected) {
		t.Fatal("missing fixture workload")
	}
	segments := map[string]adapter.Segment{d.Repo: &oracle.DiskSegment{Data: d}}
	for i, query := range queries {
		if query.ID != expected[i].QueryID {
			t.Fatal("query identity differs")
		}
		answer, err := workload.Evaluate(context.Background(), segments, nil, query)
		if err != nil {
			t.Fatal(err)
		}
		if check := workload.Compare(query, expected[i].Answer, answer); check.Mismatch {
			t.Fatal("disk-backed checker differs", query.ID, check)
		}
	}
	second := filepath.Join(t.TempDir(), "tiny")
	if err := run([]string{"-out", second, "-source-revision", revision}, io.Discard); err != nil {
		t.Fatal(err)
	}
	firstHash, err := dataset.Hash(filepath.Join(out, "manifest.json"))
	if err != nil {
		t.Fatal(err)
	}
	secondHash, err := dataset.Hash(filepath.Join(second, "manifest.json"))
	if err != nil || firstHash != secondHash {
		t.Fatal("fixture generation is not deterministic", err)
	}
	if err := run([]string{"-out", out, "-source-revision", revision}, io.Discard); err == nil {
		t.Fatal("existing output was adopted")
	}
	if after, _ := dataset.Hash(filepath.Join(out, "manifest.json")); after != firstHash {
		t.Fatal("rejected overwrite changed fixture")
	}
}

func TestTinyFixtureRejectsMissingInputsBeforeWriting(t *testing.T) {
	for _, revision := range []string{"", "short", strings.Repeat("z", 40)} {
		out := filepath.Join(t.TempDir(), "invalid")
		if err := run([]string{"-out", out, "-source-revision", revision}, io.Discard); err == nil {
			t.Fatal("invalid source revision accepted")
		}
		if _, err := os.Stat(out); !os.IsNotExist(err) {
			t.Fatal("invalid request created output", err)
		}
	}
	if err := run([]string{"-source-revision", strings.Repeat("a", 40)}, io.Discard); err == nil {
		t.Fatal("missing output accepted")
	}
}
