package main

import (
	"container/heap"
	"context"
	"fmt"
	"math"
	"slices"
	"strings"
	"sync"

	"gxstudy/adapter"
	"gxstudy/dataset"
)

// matrix owns its immutable F32 copy. Quantized bytes never replace the originals.
type matrix struct {
	ids        []string
	raw        []float32
	norms      []float64
	quant      []int8
	quantNorms []int64
	dim        int
}

func newMatrix(rows []dataset.Vector, dimensions int) (*matrix, error) {
	if dimensions < 1 || dimensions > 768 || len(rows) < 1 {
		return nil, fmt.Errorf("invalid matrix dimensions or count")
	}
	m := &matrix{dim: dimensions}
	seen := map[string]bool{}
	for _, row := range rows {
		if row.NodeID == "" || seen[row.NodeID] {
			return nil, fmt.Errorf("empty or duplicate vector ID %q", row.NodeID)
		}
		norm, err := vectorNorm(row.Values, dimensions)
		if err != nil {
			return nil, err
		}
		seen[row.NodeID] = true
		m.ids = append(m.ids, row.NodeID)
		m.raw = append(m.raw, row.Values...)
		m.norms = append(m.norms, norm)
	}
	return m, nil
}

func vectorNorm(v []float32, dimensions int) (float64, error) {
	if len(v) != dimensions {
		return 0, fmt.Errorf("vector dimensions: got %d, want %d", len(v), dimensions)
	}
	var norm float64
	for _, x := range v {
		if math.IsNaN(float64(x)) || math.IsInf(float64(x), 0) {
			return 0, fmt.Errorf("nonfinite vector")
		}
		norm += float64(x) * float64(x)
	}
	if norm == 0 {
		return 0, fmt.Errorf("zero vector")
	}
	return norm, nil
}

// Per-vector symmetric max-absolute quantization follows hybrid-followup/quantindex.
func quantize(v []float32) ([]int8, int64) {
	var largest float32
	for _, x := range v {
		largest = max(largest, float32(math.Abs(float64(x))))
	}
	out := make([]int8, len(v))
	var norm int64
	if largest == 0 {
		return out, norm
	}
	for i, x := range v {
		out[i] = int8(math.Round(float64(x / largest * 127)))
		norm += int64(out[i]) * int64(out[i])
	}
	return out, norm
}

func (m *matrix) buildInt8(ctx context.Context) error {
	m.quant = make([]int8, len(m.raw))
	m.quantNorms = make([]int64, len(m.ids))
	for i := range m.ids {
		if err := ctx.Err(); err != nil {
			return err
		}
		q, norm := quantize(m.raw[i*m.dim : (i+1)*m.dim])
		copy(m.quant[i*m.dim:], q)
		m.quantNorms[i] = norm
	}
	return nil
}

func (m *matrix) cosine(query []float32, norm float64, position int) float64 {
	row := m.raw[position*m.dim : (position+1)*m.dim]
	var dot float64
	for j, x := range query {
		dot += float64(x) * float64(row[j])
	}
	return dot / math.Sqrt(norm*m.norms[position])
}

type candidate struct {
	position int
	id       string
	score    float64
}

func compare(a, b candidate) int {
	if a.score > b.score {
		return -1
	}
	if a.score < b.score {
		return 1
	}
	return strings.Compare(a.id, b.id)
}

type candidates []candidate

func (h candidates) Len() int           { return len(h) }
func (h candidates) Less(i, j int) bool { return compare(h[i], h[j]) > 0 }
func (h candidates) Swap(i, j int)      { h[i], h[j] = h[j], h[i] }
func (h *candidates) Push(x any)        { *h = append(*h, x.(candidate)) }
func (h *candidates) Pop() any          { a := *h; x := a[len(a)-1]; *h = a[:len(a)-1]; return x }
func keep(h *candidates, value candidate, limit int) {
	if h.Len() < limit {
		heap.Push(h, value)
		return
	}
	if compare(value, (*h)[0]) < 0 {
		(*h)[0] = value
		heap.Fix(h, 0)
	}
}

// search joins at most four scan goroutines. Queries run serially; no hidden pool.
func (m *matrix) search(ctx context.Context, query []float32, k, workers, pool int, mode string) ([]adapter.Hit, error) {
	if workers < 1 || workers > 4 {
		return nil, fmt.Errorf("workers must be in 1..4")
	}
	if k < 0 || k > len(m.ids) {
		return nil, fmt.Errorf("k outside 0..corpus count")
	}
	if mode != "f32" && mode != "int8" {
		return nil, fmt.Errorf("unknown scan mode %q", mode)
	}
	if mode == "int8" && (len(m.quant) != len(m.raw) || pool < k || pool > len(m.ids)) {
		return nil, fmt.Errorf("int8 requires built quantization and k <= candidates <= corpus count")
	}
	norm, err := vectorNorm(query, m.dim)
	if err != nil {
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if k == 0 {
		return nil, nil
	}
	limit := k
	if mode == "int8" {
		limit = pool
	}
	var q []int8
	var qnorm int64
	if mode == "int8" {
		q, qnorm = quantize(query)
	}
	workers = min(workers, len(m.ids))
	local := make([]candidates, workers)
	errs := make([]error, workers)
	var group sync.WaitGroup
	for worker := range workers {
		begin, end := len(m.ids)*worker/workers, len(m.ids)*(worker+1)/workers
		group.Add(1)
		go func() {
			defer group.Done()
			h := &local[worker]
			for i := begin; i < end; i++ {
				if (i-begin)%256 == 0 {
					if e := ctx.Err(); e != nil {
						errs[worker] = e
						return
					}
				}
				var score float64
				if mode == "f32" {
					score = m.cosine(query, norm, i)
				} else {
					var dot int64
					row := m.quant[i*m.dim : (i+1)*m.dim]
					for j, x := range q {
						dot += int64(x) * int64(row[j])
					}
					score = float64(dot) / math.Sqrt(float64(qnorm)*float64(m.quantNorms[i]))
				}
				keep(h, candidate{i, m.ids[i], score}, limit)
			}
		}()
	}
	group.Wait()
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	global := &candidates{}
	for worker, values := range local {
		if errs[worker] != nil {
			return nil, errs[worker]
		}
		for _, value := range values {
			keep(global, value, limit)
		}
	}
	hits := make([]adapter.Hit, 0, global.Len())
	for _, h := range *global {
		score := h.score
		if mode == "int8" {
			score = m.cosine(query, norm, h.position)
		}
		hits = append(hits, adapter.Hit{ID: h.id, Score: score})
	}
	return adapter.Top(hits, k), nil
}

// reference overwrites and sorts its caller-owned scratch buffer. It returns a
// copied top-k, independent of the candidate heap and reusable on the next query.
func reference(ctx context.Context, m *matrix, query []float32, k int, hits []adapter.Hit) ([]adapter.Hit, error) {
	norm, err := vectorNorm(query, m.dim)
	if err != nil {
		return nil, err
	}
	if len(hits) != len(m.ids) {
		return nil, fmt.Errorf("reference scratch count mismatch")
	}
	for i, id := range m.ids {
		if i%256 == 0 {
			if err := ctx.Err(); err != nil {
				return nil, err
			}
		}
		var dot, vn float64
		for j, x := range query {
			value := float64(m.raw[i*m.dim+j])
			dot += float64(x) * value
			vn += value * value
		}
		score := dot / math.Sqrt(norm*vn)
		hits[i] = adapter.Hit{ID: id, Score: score}
	}
	slices.SortFunc(hits, func(a, b adapter.Hit) int {
		if a.Score > b.Score {
			return -1
		}
		if a.Score < b.Score {
			return 1
		}
		return strings.Compare(a.ID, b.ID)
	})
	return slices.Clone(hits[:min(k, len(hits))]), nil
}

func exactResults(want, got []adapter.Hit) error {
	if !slices.Equal(want, got) {
		return fmt.Errorf("ordered top-k IDs or original-F32 cosine scores differ")
	}
	return nil
}
