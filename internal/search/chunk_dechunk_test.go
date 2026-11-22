package search

import (
	"context"
	"slices"
	"sort"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/zzet/gortex/internal/graph"
)

// mapText is a map-backed text Backend for the hybrid fixtures. The
// production text side is the store-native FTS, which lives behind a
// graph.SymbolSearcher this package must not reach for; these tests
// only need *a* text channel next to the vector one, so the double
// keeps the corpus in a map and ranks by how many query tokens a
// document's own tokens cover, ties broken by ID for determinism.
type mapText struct{ docs map[string][]string }

func newMapText() *mapText { return &mapText{docs: make(map[string][]string)} }

func (m *mapText) Add(id string, fields ...string) {
	m.docs[id] = Tokenize(strings.Join(fields, " "))
}

func (m *mapText) Remove(id string) { delete(m.docs, id) }
func (m *mapText) Count() int       { return len(m.docs) }
func (m *mapText) Close()           {}

func (m *mapText) Search(query string, limit int) []SearchResult {
	terms := TokenizeQuery(query)
	var out []SearchResult
	for id, tokens := range m.docs {
		matched := 0
		for _, tok := range tokens {
			if slices.Contains(terms, tok) {
				matched++
			}
		}
		if matched > 0 {
			out = append(out, SearchResult{ID: id, Score: float64(matched)})
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Score != out[j].Score {
			return out[i].Score > out[j].Score
		}
		return out[i].ID < out[j].ID
	})
	if limit > 0 && len(out) > limit {
		out = out[:limit]
	}
	return out
}

// fixedEmbedder is a deterministic test embedding provider: every query
// embeds to the same constant vector, so HybridBackend.Search exercises
// the vector channel without any model.
type fixedEmbedder struct{ dims int }

func (f fixedEmbedder) Embed(_ context.Context, _ string) ([]float32, error) {
	v := make([]float32, f.dims)
	if f.dims > 0 {
		v[0] = 1
	}
	return v, nil
}

func (f fixedEmbedder) EmbedBatch(ctx context.Context, texts []string) ([][]float32, error) {
	out := make([][]float32, len(texts))
	for i := range texts {
		out[i], _ = f.Embed(ctx, texts[i])
	}
	return out, nil
}

func (f fixedEmbedder) Dimensions() int { return f.dims }
func (f fixedEmbedder) Close() error    { return nil }

// TestHybridSearch_DeChunksToParent proves the query-time de-chunk
// contract: when the vector index holds chunk vectors, a chunk hit is
// mapped back to its parent symbol, the symbol appears at most once,
// and no synthetic "#chunk" ID ever leaks into the results.
func TestHybridSearch_DeChunksToParent(t *testing.T) {
	const dims = 3
	vec := NewDelegatedVector(dims, &publicationTestDelegate{hits: []graph.VectorHit{
		{NodeID: "big.go::Big#chunk0", ParentID: "big.go::Big"},
		{NodeID: "big.go::Big#chunk1", ParentID: "big.go::Big"},
		{NodeID: "big.go::Big#chunk2", ParentID: "big.go::Big"},
		{NodeID: "small.go::Small"},
	}}, 4, 3)

	text := newMapText()
	text.Add("big.go::Big", "Big", "big.go", "")
	text.Add("small.go::Small", "Small", "small.go", "")

	h := NewHybrid(text, vec, fixedEmbedder{dims: dims})
	results := h.Search("anything", 10)
	require.NotEmpty(t, results)

	seen := map[string]int{}
	for _, r := range results {
		assert.NotContains(t, r.ID, "#chunk",
			"a synthetic chunk ID must never leak into search results")
		seen[r.ID]++
	}
	assert.Equal(t, 1, seen["big.go::Big"],
		"the sub-chunked symbol must appear exactly once despite owning three chunk vectors")
	assert.Equal(t, 1, seen["small.go::Small"])
}

// TestHybridSearch_DeChunkPreservesOrder asserts the de-chunk step
// keeps a symbol at the rank of its best-scoring chunk.
func TestHybridSearch_DeChunkPreservesOrder(t *testing.T) {
	const dims = 3
	vec := NewDelegatedVector(dims, &publicationTestDelegate{hits: []graph.VectorHit{
		{NodeID: "a.go::A#chunk0", ParentID: "a.go::A"},
		{NodeID: "b.go::B#chunk0", ParentID: "b.go::B"},
		{NodeID: "c.go::C"},
	}}, 3, 2)

	// Empty text backend so only the vector channel decides ordering.
	h := NewHybrid(newMapText(), vec, fixedEmbedder{dims: dims})
	got, _ := h.VectorChannelOnly("anything", 1)
	require.Len(t, got, 2)
	assert.Equal(t, "a.go::A", got[0], "the symbol with the nearest chunk must rank first")
	assert.Equal(t, "b.go::B", got[1])
}

// TestHybridSearch_UnchunkedIDsPassThrough checks the plain-symbol route.
func TestHybridSearch_UnchunkedIDsPassThrough(t *testing.T) {
	const dims = 3
	vec := NewDelegatedVector(dims, &publicationTestDelegate{hits: []graph.VectorHit{
		{NodeID: "a.go::A"}, {NodeID: "b.go::B"},
	}}, 2, 0)
	require.False(t, vec.HasChunks())

	h := NewHybrid(newMapText(), vec, fixedEmbedder{dims: dims})
	got, _ := h.VectorChannelOnly("anything", 1)
	require.Equal(t, []string{"a.go::A", "b.go::B"}, got)
}
