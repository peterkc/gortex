// Package vector contains the two data records and stable ranking contract
// copied from the accepted harness, keeping this portable runner self-contained.
package vector

import (
	"slices"
	"strings"
)

const Dimensions = 384

type Vector struct {
	NodeID string
	Values []float32
}
type Hit struct {
	ID    string
	Score float64
}

func Top(hits []Hit, k int) []Hit {
	out := slices.Clone(hits)
	slices.SortFunc(out, func(a, b Hit) int {
		if a.Score > b.Score {
			return -1
		}
		if a.Score < b.Score {
			return 1
		}
		return strings.Compare(a.ID, b.ID)
	})
	if k >= 0 && len(out) > k {
		out = out[:k]
	}
	return out
}
