package storage

import (
	"math"
	"time"
)

// DurationHistogram is a snapshot of a duration histogram. Counts[i] holds
// the observations at most DurationHistogramBounds[i] and above the edge
// before it; the last count holds those above every edge. The fields are read
// one by one, so a snapshot taken mid-record may hold a Total or Max that
// includes a sample its Counts do not yet.
type DurationHistogram struct {
	Counts [len(DurationHistogramBounds) + 1]int64
	Total  time.Duration
	Max    time.Duration
}

// Count is the number of observations.
func (h DurationHistogram) Count() int64 {
	var n int64
	for _, c := range h.Counts {
		n += c
	}
	return n
}

// Quantile returns an upper bound on the q-quantile (0 < q <= 1): the edge of
// the bucket holding that observation, or Max when Max is smaller or the
// observation lies above every edge. It is zero for an empty histogram.
func (h DurationHistogram) Quantile(q float64) time.Duration {
	n := h.Count()
	if n == 0 {
		return 0
	}
	bounds := DurationHistogramBounds
	rank := max(int64(math.Ceil(q*float64(n))), 1)
	var seen int64
	for i, c := range h.Counts {
		seen += c
		if seen >= rank && i < len(bounds) {
			return min(bounds[i], h.Max)
		}
	}
	return h.Max
}
