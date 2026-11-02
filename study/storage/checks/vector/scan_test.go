package main

import (
	"context"
	"math"
	"reflect"
	"slices"
	"testing"

	"gxstudy/adapter"
	"gxstudy/conformance"
	"gxstudy/dataset"
	"gxstudy/oracle"
)

func mustMatrix(t *testing.T, rows []dataset.Vector, dimensions int) *matrix {
	t.Helper()
	m, err := newMatrix(rows, dimensions)
	if err != nil {
		t.Fatal(err)
	}
	return m
}
func TestHarnessVectorContract(t *testing.T) {
	d := conformance.Fixture()
	m := mustMatrix(t, d.Vectors, dataset.Dimensions)
	query := slices.Clone(d.Vectors[0].Values)
	want, err := oracle.New(d).VectorTopK(context.Background(), query, 2)
	if err != nil {
		t.Fatal(err)
	}
	for workers := 1; workers <= 4; workers++ {
		got, err := m.search(context.Background(), query, 2, workers, 0, "f32")
		if err != nil || exactResults(want, got) != nil {
			t.Fatal(workers, got, want, err)
		}
	}
	if len(want) != 2 || want[0].ID != "a" || want[0].Score != 1 {
		t.Fatal("fixture must exercise exact cosine", want)
	}
}
func TestExactTiesAndInputOrder(t *testing.T) {
	rows := []dataset.Vector{{NodeID: "z", Values: []float32{1, 0}}, {NodeID: "a", Values: []float32{1, 0}}, {NodeID: "m", Values: []float32{1, 0}}}
	copied := mustMatrix(t, rows, 2)
	rows[0].Values[0] = 0
	if copied.raw[0] != 1 {
		t.Fatal("matrix aliases caller values")
	}
	rows[0].Values[0] = 1
	want := []adapter.Hit{{ID: "a", Score: 1}, {ID: "m", Score: 1}}
	for _, order := range []string{"forward", "reversed"} {
		input := slices.Clone(rows)
		if order == "reversed" {
			slices.Reverse(input)
		}
		for _, mode := range []string{"f32", "int8"} {
			t.Run(order+"-"+mode, func(t *testing.T) {
				m := mustMatrix(t, input, 2)
				if mode == "int8" {
					if err := m.buildInt8(context.Background()); err != nil {
						t.Fatal(err)
					}
				}
				for workers := 1; workers <= 4; workers++ {
					// Three tied rows compete for two slots, including the int8 pool.
					got, err := m.search(context.Background(), []float32{1, 0}, 2, workers, 2, mode)
					if err != nil || exactResults(want, got) != nil {
						t.Fatal(workers, got, err)
					}
				}
			})
		}
	}
}
func TestInt8PruningMustNotMasqueradeAsExact(t *testing.T) {
	rows := []dataset.Vector{{NodeID: "a-far", Values: []float32{1, .003}}, {NodeID: "b-near", Values: []float32{1, .001}}}
	m := mustMatrix(t, rows, 2)
	if err := m.buildInt8(context.Background()); err != nil {
		t.Fatal(err)
	}
	query := []float32{1, .001}
	want, err := reference(context.Background(), m, query, 1, make([]adapter.Hit, len(m.ids)))
	if err != nil {
		t.Fatal(err)
	}
	if len(want) != 1 || want[0].ID != "b-near" {
		t.Fatal("adversarial truth is wrong", want)
	}
	for _, workers := range []int{1, 4} {
		pruned, err := m.search(context.Background(), query, 1, workers, 1, "int8")
		if err != nil {
			t.Fatal(err)
		}
		if exactResults(want, pruned) == nil {
			t.Fatal("quantized tie hid an omitted original-F32 neighbor", pruned)
		}
		full, err := m.search(context.Background(), query, 1, workers, 2, "int8")
		if err != nil || exactResults(want, full) != nil {
			t.Fatal("original-F32 rerank lost exact neighbor", full, err)
		}
	}
}
func TestExactnessRejectsOneWrongResult(t *testing.T) {
	want := []adapter.Hit{{ID: "a", Score: 1}, {ID: "b", Score: .9}}
	for name, got := range map[string][]adapter.Hit{
		"omitted": want[:1], "wrong-id": {{ID: "a", Score: 1}, {ID: "c", Score: .9}},
		"reordered": {want[1], want[0]}, "changed-score": {{ID: "a", Score: 1}, {ID: "b", Score: .900000001}},
	} {
		t.Run(name, func(t *testing.T) {
			if exactResults(want, got) == nil {
				t.Fatal("non-exact result accepted")
			}
		})
	}
}
func TestNearTiesUseNoEpsilon(t *testing.T) {
	m := mustMatrix(t, []dataset.Vector{{NodeID: "a", Values: []float32{1, .0011}}, {NodeID: "z", Values: []float32{1, .001}}}, 2)
	got, err := m.search(context.Background(), []float32{1, 0}, 1, 4, 0, "f32")
	if err != nil || len(got) != 1 || got[0].ID != "z" {
		t.Fatal("epsilon tie substitution", got, err)
	}
}
func TestScanBoundsAndCancellation(t *testing.T) {
	m := mustMatrix(t, []dataset.Vector{{NodeID: "a", Values: []float32{1, 0}}}, 2)
	for _, test := range []struct {
		workers, k, pool int
		mode             string
		q                []float32
	}{
		{0, 1, 1, "f32", []float32{1, 0}}, {5, 1, 1, "f32", []float32{1, 0}},
		{1, -1, 1, "f32", []float32{1, 0}}, {1, 2, 1, "f32", []float32{1, 0}},
		{1, 1, 1, "bad", []float32{1, 0}}, {1, 1, 1, "int8", []float32{1, 0}},
		{1, 1, 1, "f32", []float32{1}},
		{1, 1, 1, "f32", []float32{0, 0}}, {1, 1, 1, "f32", []float32{float32(math.NaN()), 0}},
	} {
		if _, err := m.search(context.Background(), test.q, test.k, test.workers, test.pool, test.mode); err == nil {
			t.Fatal("invalid scan accepted", test)
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := m.search(ctx, []float32{1, 0}, 1, 1, 0, "f32"); err == nil {
		t.Fatal("cancelled scan accepted")
	}
	got, err := m.search(context.Background(), []float32{1, 0}, 0, 1, 0, "f32")
	if err != nil || len(got) != 0 {
		t.Fatal("k=0 must be a no-change empty answer", got, err)
	}
	for _, rows := range [][]dataset.Vector{
		{{NodeID: "", Values: []float32{1, 0}}},
		{{NodeID: "a", Values: []float32{1, 0}}, {NodeID: "a", Values: []float32{0, 1}}},
		{{NodeID: "a", Values: []float32{float32(math.Inf(1)), 0}}},
	} {
		if _, err := newMatrix(rows, 2); err == nil {
			t.Fatal("invalid matrix accepted")
		}
	}
}
func TestNoMutation(t *testing.T) {
	query := []float32{1, 0}
	before := slices.Clone(query)
	m := mustMatrix(t, []dataset.Vector{{NodeID: "b", Values: []float32{0, 1}}, {NodeID: "a", Values: []float32{1, 0}}}, 2)
	if err := m.buildInt8(context.Background()); err != nil {
		t.Fatal(err)
	}
	for _, mode := range []string{"f32", "int8"} {
		got, err := m.search(context.Background(), query, 2, 2, 2, mode)
		want := []adapter.Hit{{ID: "a", Score: 1}, {ID: "b", Score: 0}}
		if err != nil || exactResults(want, got) != nil {
			t.Fatal(got, err)
		}
	}
	if !reflect.DeepEqual(query, before) {
		t.Fatal("search changed caller query")
	}
	if _, err := newMatrix([]dataset.Vector{{NodeID: "zero", Values: []float32{0, 0}}}, 2); err == nil {
		t.Fatal("zero vector accepted")
	}
}
