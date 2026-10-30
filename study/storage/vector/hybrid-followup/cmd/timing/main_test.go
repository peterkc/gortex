package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"os"
	"path/filepath"
	"testing"

	"hybridfollowup/internal/quantindex"
)

func TestFreshOutputNamespacePreservesInputsAndExclusiveResults(t *testing.T) {
	data := t.TempDir()
	index := filepath.Join(data, "int8-n32-d384.bin")
	if err := quantindex.BuildGenerated(
		context.Background(), index, 32, 384, 20261023,
	); err != nil {
		t.Fatal(err)
	}
	input, err := os.ReadFile(index)
	if err != nil {
		t.Fatal(err)
	}
	args := []string{
		"-phase", "query", "-n", "32",
		"-dimensions", "384", "-workers", "1",
		"-queries", "2", "-repeat", "1", "-data-dir", data,
	}
	invoke := func(extra ...string) error {
		savedFlags, savedArgs := flag.CommandLine, os.Args
		defer func() {
			flag.CommandLine, os.Args = savedFlags, savedArgs
		}()
		flag.CommandLine = flag.NewFlagSet(
			"timing-test", flag.ContinueOnError,
		)
		os.Args = append([]string{"timing-test"}, args...)
		os.Args = append(os.Args, extra...)
		return run()
	}
	first := filepath.Join(t.TempDir(), "sample.json")
	if err := invoke("-output", first); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(first)
	if err != nil {
		t.Fatal(err)
	}
	var result struct {
		N       int `json:"n"`
		Queries int `json:"queries"`
		Workers int `json:"workers"`
	}
	if err := json.Unmarshal(raw, &result); err != nil {
		t.Fatal(err)
	}
	if result.N != 32 || result.Queries != 2 || result.Workers != 1 {
		t.Fatalf("wrong bounded sample: %+v", result)
	}
	legacy := filepath.Join(data, "n32-d384-w1-rep1.json")
	if _, err := os.Stat(legacy); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("custom output leaked into input directory: %v", err)
	}
	if err := invoke("-output", first); !errors.Is(err, os.ErrExist) {
		t.Fatalf("existing sample was not refused: %v", err)
	}
	retained, err := os.ReadFile(first)
	if err != nil || !bytes.Equal(raw, retained) {
		t.Fatalf("existing raw sample changed: %v", err)
	}
	second := filepath.Join(t.TempDir(), "sample.json")
	if err := invoke("-output", second); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(second); err != nil {
		t.Fatal(err)
	}
	if err := invoke(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(legacy); err != nil {
		t.Fatal(err)
	}
	after, err := os.ReadFile(index)
	if err != nil || !bytes.Equal(input, after) {
		t.Fatalf("immutable generated input changed: %v", err)
	}
}
