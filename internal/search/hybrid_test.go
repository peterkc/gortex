package search

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type hybridCountTextBackend struct {
	count int
}

func (*hybridCountTextBackend) Add(string, ...string)             {}
func (*hybridCountTextBackend) Remove(string)                     {}
func (*hybridCountTextBackend) Search(string, int) []SearchResult { return nil }
func (b *hybridCountTextBackend) Count() int                      { return b.count }
func (*hybridCountTextBackend) Close()                            {}

func TestHybridCountUsesVectorWhenTextIsEmpty(t *testing.T) {
	vector := NewDelegatedVector(2, &publicationTestDelegate{}, 1, 0)

	hybrid := NewHybrid(NewNull(), vector, nil)
	require.Equal(t, 1, hybrid.Count(), "a populated vector channel is a searchable corpus")

	textAuthoritative := NewHybrid(&hybridCountTextBackend{count: 3}, vector, nil)
	assert.Equal(t, 3, textAuthoritative.Count(), "text and vector counts describe the same corpus and must not be summed")

	empty := NewHybrid(NewNull(), nil, nil)
	assert.Zero(t, empty.Count(), "an empty hybrid must report no corpus")

	var nilHybrid *HybridBackend
	assert.Zero(t, nilHybrid.Count(), "a nil hybrid must report no corpus")
}

func TestAlphaFuse_EqualWeights(t *testing.T) {
	textResults := []SearchResult{
		{ID: "a", Score: 10},
		{ID: "b", Score: 8},
		{ID: "c", Score: 5},
	}
	vecIDs := []string{"b", "d", "a"}

	results := alphaFuse(textResults, vecIDs, 0.5, 60, 10)
	require.GreaterOrEqual(t, len(results), 3)

	// "a" and "b" appear in both lists → highest RRF scores.
	// "b" is rank 1 in text, rank 0 in vec → should score high.
	topIDs := make([]string, len(results))
	for i, r := range results {
		topIDs[i] = r.ID
	}
	assert.Contains(t, topIDs[:2], "a", "a should be in top 2 (in both lists)")
	assert.Contains(t, topIDs[:2], "b", "b should be in top 2 (in both lists)")

	// All results should have non-zero scores.
	for _, r := range results {
		assert.Greater(t, r.Score, float64(0))
	}
}

func TestAlphaFuse_EmptyVec(t *testing.T) {
	textResults := []SearchResult{
		{ID: "a", Score: 10},
		{ID: "b", Score: 8},
	}
	results := alphaFuse(textResults, nil, 0.5, 60, 10)
	// With no vec results, only text results contribute.
	assert.Len(t, results, 2)
	assert.Equal(t, "a", results[0].ID)
}

func TestAlphaFuse_Limit(t *testing.T) {
	textResults := []SearchResult{
		{ID: "a"}, {ID: "b"}, {ID: "c"}, {ID: "d"}, {ID: "e"},
	}
	vecIDs := []string{"f", "g", "h", "i", "j"}

	results := alphaFuse(textResults, vecIDs, 0.5, 60, 3)
	assert.Len(t, results, 3)
}

// --- adaptive alphaFuse coverage -------------------------------------

func TestAlphaFuse_SmallAlphaFavorsText(t *testing.T) {
	// Text-only candidate "t" and vector-only candidate "v" at the
	// same rank in their channels. With α=0.1 (BM25-heavy), "t" must
	// outrank "v" — symbol queries are supposed to favor text.
	textResults := []SearchResult{{ID: "t"}}
	vecIDs := []string{"v"}

	res := alphaFuse(textResults, vecIDs, 0.1, 60, 10)
	require.Len(t, res, 2)
	if res[0].ID != "t" {
		t.Fatalf("with α=0.1 expected text-only hit first, got %v", res[0].ID)
	}
}

func TestAlphaFuse_LargeAlphaFavorsVector(t *testing.T) {
	textResults := []SearchResult{{ID: "t"}}
	vecIDs := []string{"v"}

	res := alphaFuse(textResults, vecIDs, 0.9, 60, 10)
	require.Len(t, res, 2)
	if res[0].ID != "v" {
		t.Fatalf("with α=0.9 expected vector-only hit first, got %v", res[0].ID)
	}
}

func TestAlphaFuse_ClampsAlpha(t *testing.T) {
	// α below 0 → 0 (text-only); above 1 → 1 (vector-only). Out-of-
	// range values must not break sort order.
	textResults := []SearchResult{{ID: "t"}}
	vecIDs := []string{"v"}

	res := alphaFuse(textResults, vecIDs, -5.0, 60, 10)
	require.Len(t, res, 2)
	if res[0].ID != "t" {
		t.Fatalf("α=-5 should clamp to 0 (text wins), got %v", res[0].ID)
	}
	res = alphaFuse(textResults, vecIDs, 5.0, 60, 10)
	require.Len(t, res, 2)
	if res[0].ID != "v" {
		t.Fatalf("α=5 should clamp to 1 (vector wins), got %v", res[0].ID)
	}
}

func TestAlphaFuse_DeterministicTieBreak(t *testing.T) {
	textResults := []SearchResult{{ID: "alpha"}, {ID: "beta"}}
	vecIDs := []string{"alpha", "gamma"}

	for range 5 {
		res := alphaFuse(textResults, vecIDs, 0.5, 60, 10)
		require.GreaterOrEqual(t, len(res), 3)
		if res[0].ID != "alpha" {
			t.Fatalf("alphaFuse ordering not stable; got %v", res[0].ID)
		}
	}
}
