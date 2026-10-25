package main

import (
	"os"
	"path/filepath"
	"slices"
	"testing"

	"hybridfollowup/internal/quantindex"
)

func TestGeneratedLatencyInput(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "input")
	if err := generateInputs(dir, 10); err != nil {
		t.Fatal(err)
	}
	d, err := loadMetadata(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(d.Documents) != 10 || len(d.Queries) != 256 {
		t.Fatal("wrong generated shape")
	}
	if err = verifyInputs(d); err != nil {
		t.Fatal(err)
	}
	for _, item := range []struct {
		name         string
		rows, offset int
	}{{"document", 10, 0}, {"query", 256, 1000000000}} {
		matrix, err := openMatrix(filepath.Join(dir, item.name+".f32"), item.rows)
		if err != nil {
			t.Fatal(err)
		}
		if !slices.Equal(matrix.Vector(0), quantindex.SyntheticVector(20261023, item.offset, dimensions)) {
			t.Fatal("not timing's generator", item.name)
		}
		if err = matrix.Close(); err != nil {
			t.Fatal(err)
		}
	}
	if err = generateInputs(dir, 10); err == nil {
		t.Fatal("existing output accepted")
	}
	bad := filepath.Join(t.TempDir(), "invalid")
	if err = generateInputs(bad, 0); err == nil {
		t.Fatal("invalid rows accepted")
	}
	if _, err = os.Stat(bad); !os.IsNotExist(err) {
		t.Fatal("invalid rows wrote output")
	}
	path := filepath.Join(dir, "document.f32")
	if err = os.Chmod(path, 0600); err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(path, []byte("corrupt"), 0600); err != nil {
		t.Fatal(err)
	}
	if err = verifyInputs(d); err == nil {
		t.Fatal("generated input drift accepted")
	}
}
