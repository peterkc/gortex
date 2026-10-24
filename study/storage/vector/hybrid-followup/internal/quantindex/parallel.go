package quantindex

import (
	"container/heap"
	"context"
	"encoding/binary"
	"fmt"
	"hybridfollowup/internal/vector"
	"math"
	"sync"
)

// SearchWorkers scans in parallel, merges global quantized top candidates, then
// reranks only that global pool in original float32 cosine. It joins all workers.
func (idx *Index) SearchWorkers(ctx context.Context, q []float32, k, workers int) ([]vector.Hit, error) {
	if err := validateDimensions(q, idx.dimensions); err != nil {
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if idx.Mode != "int8" {
		return nil, fmt.Errorf("parallel experiment supports int8 only")
	}
	if workers != 1 && workers != 4 && workers != 8 {
		return nil, fmt.Errorf("workers must be 1,4,8")
	}
	if k == 0 {
		return nil, nil
	}
	if k < 0 {
		k = len(idx.IDs)
	}
	limit := max(k, idx.Candidates)
	quant := quantize(q)
	var qnorm int64
	for _, x := range quant {
		v := int64(int8(x))
		qnorm += v * v
	}
	workers = min(workers, max(1, len(idx.IDs)))
	local := make([][]candidate, workers)
	errs := make([]error, workers)
	if workers == 1 {
		local[0], errs[0] = idx.scanRange(ctx, quant, qnorm, 0, len(idx.IDs), limit)
	} else {
		var group sync.WaitGroup
		for worker := range workers {
			begin := len(idx.IDs) * worker / workers
			end := len(idx.IDs) * (worker + 1) / workers
			group.Add(1)
			go func() {
				defer group.Done()
				local[worker], errs[worker] = idx.scanRange(ctx, quant, qnorm, begin, end, limit)
			}()
		}
		group.Wait()
	}
	global := &candidateHeap{}
	heap.Init(global)
	for worker, values := range local {
		if errs[worker] != nil {
			return nil, errs[worker]
		}
		for _, value := range values {
			keepCandidate(global, value, limit)
		}
	}
	out := make([]vector.Hit, 0, global.Len())
	var qn float64
	for _, x := range q {
		qn += float64(x) * float64(x)
	}
	for _, h := range *global {
		raw := idx.raw[h.position*idx.dimensions*4 : (h.position+1)*idx.dimensions*4]
		var dot, vn float64
		for j, x := range q {
			v := float64(math.Float32frombits(binary.LittleEndian.Uint32(raw[j*4:])))
			dot += float64(x) * v
			vn += v * v
		}
		out = append(out, vector.Hit{ID: idx.IDs[h.position], Score: dot / math.Sqrt(qn*vn)})
	}
	return vector.Top(out, k), nil
}
func keepCandidate(values *candidateHeap, value candidate, limit int) {
	if values.Len() < limit {
		heap.Push(values, value)
	} else if compare(value, (*values)[0]) < 0 {
		(*values)[0] = value
		heap.Fix(values, 0)
	}
}
func (idx *Index) scanRange(ctx context.Context, q []byte, qnorm int64, begin, end, limit int) ([]candidate, error) {
	values := &candidateHeap{}
	heap.Init(values)
	for i := begin; i < end; i++ {
		if (i-begin)%4096 == 0 {
			if err := ctx.Err(); err != nil {
				return nil, err
			}
		}
		v := idx.quant[i*idx.dimensions : (i+1)*idx.dimensions]
		var dot int32
		for j, x := range q {
			dot += int32(int8(x)) * int32(int8(v[j]))
		}
		score := float64(dot) / math.Sqrt(float64(qnorm*idx.norms[i]))
		keepCandidate(values, candidate{position: i, score: score, id: idx.IDs[i]}, limit)
	}
	return *values, nil
}
