package quantindex

import (
	"context"
	"fmt"
	"hybridfollowup/internal/vector"
	"math/rand"
	"path/filepath"
	"reflect"
	"testing"
)

func TestParallelMatchesGlobalSerial(t *testing.T) {
	for _, dimensions := range []int{384, 1024} {
		rows := make([]vector.Vector, 273)
		rng := rand.New(rand.NewSource(20261023))
		for i := range rows {
			v := make([]float32, dimensions)
			for j := range v {
				v[j] = rng.Float32() - .5
			}
			if i%13 == 0 {
				v = make([]float32, dimensions)
				v[0] = 1
			}
			rows[i] = vector.Vector{NodeID: fmt.Sprintf("id%04d", i), Values: v}
		}
		path := filepath.Join(t.TempDir(), "quant.bin")
		if err := BuildDimensions(context.Background(), path, "int8", rows, dimensions); err != nil {
			t.Fatal(err)
		}
		index, err := Open(path, "int8", 128)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { index.Close() })
		for _, query := range [][]float32{rows[0].Values, rows[1].Values, rows[26].Values} {
			for _, k := range []int{10, 128} {
				want, err := index.Search(context.Background(), query, k)
				if err != nil {
					t.Fatal(err)
				}
				for _, workers := range []int{1, 4, 8} {
					got, err := index.SearchWorkers(context.Background(), query, k, workers)
					if err != nil {
						t.Fatal(err)
					}
					if !reflect.DeepEqual(got, want) {
						t.Fatalf("dimensions%d workers%d unequal global candidate result", dimensions, workers)
					}
				}
			}
		}
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		if _, err = index.SearchWorkers(ctx, rows[0].Values, 10, 8); err != context.Canceled {
			t.Fatal(err)
		}
		if _, err = index.SearchWorkers(context.Background(), rows[0].Values, 10, 3); err == nil {
			t.Fatal("accepted invalid worker count")
		}
	}
}
