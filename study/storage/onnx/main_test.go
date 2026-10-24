package main

import (
	"math"
	"slices"
	"testing"
)

func TestCLSPoolingPreservesInputs(t *testing.T) {
	hidden := make([]float32, 2*768)
	hidden[0], hidden[1], hidden[768] = 3, 4, 900
	original := slices.Clone(hidden)
	vector := pool(hidden, []int64{1, 1}, []int64{1, 2}, "coderank")[0]
	if math.Abs(float64(vector[0])-0.6) > 1e-7 || math.Abs(float64(vector[1])-0.8) > 1e-7 {
		t.Fatalf("wrong CLS vector: %v", vector[:2])
	}
	if !slices.Equal(hidden, original) {
		t.Fatal("pooling changed hidden state")
	}
}

func TestMaskedMeanPooling(t *testing.T) {
	hidden := make([]float32, 2*768)
	hidden[0], hidden[1], hidden[768] = 3, 4, 900
	vector := pool(hidden, []int64{1, 0}, []int64{1, 2}, "nomic")[0]
	mean, norm := 0.0, 0.0
	for _, value := range vector {
		mean += float64(value)
		norm += float64(value) * float64(value)
	}
	if math.Abs(mean) > 1e-6 || math.Abs(norm-1) > 1e-6 || vector[1] <= vector[0] {
		t.Fatalf("wrong masked normalized vector: mean=%g norm=%g first=%v", mean, norm, vector[:2])
	}
}
