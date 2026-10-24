package quality

import (
	"math"
	"reflect"
	"testing"
)

func TestInverseRatioEquation3(t *testing.T) {
	a := []float64{1, 1}
	b := []float64{2, 1}
	got, err := InverseRatio(a, b)
	if err != nil || math.Abs(got-2./3) > 1e-12 {
		t.Fatal(got, err)
	}
	if !reflect.DeepEqual(b, []float64{2, 1}) {
		t.Fatal("mutated returned distances")
	}
	got, err = InverseRatio([]float64{0, 1}, []float64{0, 1})
	if err != nil || got != 1 {
		t.Fatal(got, err)
	}
	got, err = InverseRatio([]float64{0, 1}, []float64{.1, 1})
	if err != nil || got != 0 {
		t.Fatal(got, err)
	}
	if _, err = InverseRatio([]float64{1}, []float64{-1}); err == nil {
		t.Fatal("accepted negative distance")
	}
}
func TestParentCollapse(t *testing.T) {
	got, err := ParentTop10([]int{1, 0, 2}, []string{"symbol-a", "symbol-a", "symbol-b"})
	if err != nil || !reflect.DeepEqual(got, []string{"symbol-a", "symbol-b"}) {
		t.Fatal(got, err)
	}
	if _, err = ParentTop10([]int{9}, []string{"one"}); err == nil {
		t.Fatal("accepted unknown candidate")
	}
}
