package quality

import (
	"fmt"
	"math"
	"slices"
)

// InverseRatio implements Eq.(3), not the arithmetic mean of inverse ratios.
// Both lists are ranked by original cosine distance. A missing result scores0.
// A matching zero-distance pair contributes1; zero exact / positive returned
// makes the approximation ratio infinite and its inverse0.
func InverseRatio(exact, returned []float64) (float64, error) {
	if len(exact) == 0 || len(returned) > len(exact) {
		return 0, fmt.Errorf("invalid ratio lengths")
	}
	if len(returned) < len(exact) {
		return 0, nil
	}
	a := slices.Clone(exact)
	b := slices.Clone(returned)
	slices.Sort(a)
	slices.Sort(b)
	var sum float64
	for i, d := range a {
		r := b[i]
		if math.IsNaN(d) || math.IsNaN(r) || math.IsInf(d, 0) || math.IsInf(r, 0) || d < 0 || r < 0 {
			return 0, fmt.Errorf("invalid original distance")
		}
		if d == 0 {
			if r > 0 {
				return 0, nil
			}
			sum++
			continue
		}
		sum += r / d
	}
	if sum == 0 {
		return 0, fmt.Errorf("zero approximation-ratio sum")
	}
	value := float64(len(a)) / sum
	if value > 1+1e-9 {
		return 0, fmt.Errorf("returned distances precede exact reference")
	}
	return min(1, value), nil
}

// ParentTop10 preserves retrieval order and collapses chunk records to symbols.
func ParentTop10(positions []int, parents []string) ([]string, error) {
	out := make([]string, 0, 10)
	seen := map[string]bool{}
	for _, position := range positions {
		if position < 0 || position >= len(parents) {
			return nil, fmt.Errorf("candidate position outside corpus")
		}
		parent := parents[position]
		if !seen[parent] {
			out = append(out, parent)
			seen[parent] = true
		}
		if len(out) == 10 {
			break
		}
	}
	return out, nil
}
