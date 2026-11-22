package search

import (
	"sync"

	"github.com/zzet/gortex/internal/graph"
)

// VectorDelegate searches the durable vector corpus. Hits include parent IDs
// for chunk vectors.
type VectorDelegate interface {
	SimilarTo(vec []float32, limit int) ([]graph.VectorHit, error)
}

// VectorBackend searches a durable corpus without retaining any of its rows.
type VectorBackend struct {
	dims               int
	mu                 sync.RWMutex
	delegate           VectorDelegate
	delegateCount      int
	delegateChunkCount int
}

// NewDelegatedVector creates a vector backend that routes searches to a
// durable store. vectorCount and chunkCount describe the complete durable
// corpus, so warm-started hybrid search is enabled immediately.
func NewDelegatedVector(dims int, delegate VectorDelegate, vectorCount, chunkCount int) *VectorBackend {
	if delegate == nil {
		panic("search.NewDelegatedVector: nil delegate")
	}
	if dims <= 0 {
		panic("search.NewDelegatedVector: non-positive dimensions")
	}
	if vectorCount < 0 {
		panic("search.NewDelegatedVector: negative vector count")
	}
	if chunkCount < 0 || chunkCount > vectorCount {
		panic("search.NewDelegatedVector: invalid chunk count")
	}
	return &VectorBackend{
		dims:               dims,
		delegate:           delegate,
		delegateCount:      vectorCount,
		delegateChunkCount: chunkCount,
	}
}

// HasChunks reports whether any vector in the corpus is a chunk.
func (v *VectorBackend) HasChunks() bool {
	v.mu.RLock()
	defer v.mu.RUnlock()
	return v.delegateChunkCount > 0
}

// Search returns the k nearest neighbors to the query vector.
func (v *VectorBackend) Search(query []float32, k int) []string {
	if k <= 0 {
		return nil
	}
	v.mu.RLock()
	d := v.delegate
	delegateCount := v.delegateCount
	delegateChunks := v.delegateChunkCount
	v.mu.RUnlock()
	if d == nil {
		return nil
	}
	// Similarity rows are vector-ranked, but several top rows may be sibling
	// chunks of one parent. Overfetch by the complete chunk population (capped
	// by the corpus size), then stop after k unique symbols so de-duplication
	// cannot silently under-fill an otherwise sufficient result set.
	fetch := k
	if delegateChunks > 0 {
		if delegateCount-k <= delegateChunks {
			fetch = delegateCount
		} else {
			fetch = k + delegateChunks
		}
		if fetch < k {
			fetch = k
		}
	}
	hits, err := d.SimilarTo(query, fetch)
	if err != nil || len(hits) == 0 {
		return nil
	}
	ids := make([]string, 0, min(k, len(hits)))
	seen := make(map[string]struct{}, len(hits))
	for _, h := range hits {
		id := h.NodeID
		if h.ParentID != "" {
			id = h.ParentID
		}
		if id == "" {
			continue
		}
		if _, duplicate := seen[id]; duplicate {
			continue
		}
		seen[id] = struct{}{}
		ids = append(ids, id)
		if len(ids) == k {
			break
		}
	}
	return ids
}

// Count returns the number of indexed vectors.
func (v *VectorBackend) Count() int {
	v.mu.RLock()
	defer v.mu.RUnlock()
	return v.delegateCount
}

// Dims returns the embedding dimensionality.
func (v *VectorBackend) Dims() int { return v.dims }

// SizeBytes reports the resident vector corpus size. The corpus is durable,
// never held by this backend.
func (*VectorBackend) SizeBytes() uint64 { return 0 }

// Close detaches the externally owned delegate without closing its store.
func (v *VectorBackend) Close() {
	if v == nil {
		return
	}
	v.mu.Lock()
	defer v.mu.Unlock()
	v.delegate = nil
	v.delegateCount = 0
	v.delegateChunkCount = 0
}
