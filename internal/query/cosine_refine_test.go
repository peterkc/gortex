package query

import (
	"context"
	"errors"
	"testing"

	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/search"
	"github.com/zzet/gortex/internal/search/rerank"
)

// fakeEmbedder is a minimal embedding.Provider for the refinement
// tests. Embed returns the configured query vector; the batch / dim /
// close methods are present only to satisfy the interface.
type fakeEmbedder struct {
	queryVec []float32
	err      error
}

func (f *fakeEmbedder) Embed(_ context.Context, _ string) ([]float32, error) {
	if f.err != nil {
		return nil, f.err
	}
	return f.queryVec, nil
}

func (f *fakeEmbedder) EmbedBatch(_ context.Context, texts []string) ([][]float32, error) {
	if f.err != nil {
		return nil, f.err
	}
	out := make([][]float32, len(texts))
	for i := range out {
		out[i] = f.queryVec
	}
	return out, nil
}

func (f *fakeEmbedder) Dimensions() int { return len(f.queryVec) }
func (f *fakeEmbedder) Close() error    { return nil }

// fakeVectorSearcher returns stored vectors from a fixed map and tracks
// the IDs GetEmbeddings was asked for, so a test can assert the stage
// only fetched the bounded head window.
type fakeVectorSearcher struct {
	hits       []graph.VectorHit
	vecs       map[string][]float32
	askedFor   []string
	getCalled  bool
	returnNone bool // simulate "store has no vectors at all"
}

func (f *fakeVectorSearcher) UpsertEmbedding(string, []float32) error       { return nil }
func (f *fakeVectorSearcher) BulkUpsertEmbeddings([]graph.VectorItem) error { return nil }
func (f *fakeVectorSearcher) BuildVectorIndex(int) error                    { return nil }
func (f *fakeVectorSearcher) SimilarTo([]float32, int) ([]graph.VectorHit, error) {
	return f.hits, nil
}

func (f *fakeVectorSearcher) GetEmbeddings(ids []string) map[string][]float32 {
	f.getCalled = true
	f.askedFor = append(f.askedFor, ids...)
	if f.returnNone {
		return map[string][]float32{}
	}
	out := make(map[string][]float32, len(ids))
	for _, id := range ids {
		if v, ok := f.vecs[id]; ok {
			out[id] = v
		}
	}
	return out
}

// cand builds a candidate at a given incoming rerank position.
func cand(id string, textRank int) *rerank.Candidate {
	return &rerank.Candidate{
		Node:       &graph.Node{ID: id, Name: id},
		TextRank:   textRank,
		VectorRank: -1,
	}
}

func ids(cands []*rerank.Candidate) []string {
	out := make([]string, len(cands))
	for i, c := range cands {
		out[i] = c.Node.ID
	}
	return out
}

// TestRefineByCosine_ReordersByCosine asserts the stage reorders the
// head by exact cosine: the candidate whose stored vector points most
// nearly the same direction as the query embedding rises to the top,
// even though it started last in the rerank order.
func TestRefineByCosine_ReordersByCosine(t *testing.T) {
	query := []float32{1, 0, 0}
	// "c" is the closest to the query direction, then "b", then "a";
	// the incoming rerank order is the reverse (a, b, c), so a correct
	// cosine refinement must invert it.
	vs := &fakeVectorSearcher{vecs: map[string][]float32{
		"a": {0, 1, 0},    // orthogonal — cosine 0
		"b": {1, 1, 0},    // 45° — cosine ~0.707
		"c": {10, 0.1, 0}, // almost parallel — cosine ~1
	}}
	emb := &fakeEmbedder{queryVec: query}

	in := []*rerank.Candidate{cand("a", 0), cand("b", 1), cand("c", 2)}
	out := refineByCosine("q", in, emb, vs, 10)

	got := ids(out)
	want := []string{"c", "b", "a"}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("cosine refinement order = %v, want %v", got, want)
		}
	}
}

// TestRefineByCosine_NoopWhenVectorsAbsent asserts the stage is a strict
// no-op (order preserved) when the store has no vectors for the
// candidates — the regression-safety contract.
func TestRefineByCosine_NoopWhenVectorsAbsent(t *testing.T) {
	emb := &fakeEmbedder{queryVec: []float32{1, 0, 0}}

	t.Run("store returns empty", func(t *testing.T) {
		vs := &fakeVectorSearcher{returnNone: true}
		in := []*rerank.Candidate{cand("a", 0), cand("b", 1), cand("c", 2)}
		out := refineByCosine("q", in, emb, vs, 10)
		if got := ids(out); got[0] != "a" || got[1] != "b" || got[2] != "c" {
			t.Fatalf("expected order preserved, got %v", got)
		}
	})

	t.Run("no matching ids", func(t *testing.T) {
		vs := &fakeVectorSearcher{vecs: map[string][]float32{"zzz": {1, 0, 0}}}
		in := []*rerank.Candidate{cand("a", 0), cand("b", 1)}
		out := refineByCosine("q", in, emb, vs, 10)
		if got := ids(out); got[0] != "a" || got[1] != "b" {
			t.Fatalf("expected order preserved, got %v", got)
		}
	})

	t.Run("nil vector searcher", func(t *testing.T) {
		in := []*rerank.Candidate{cand("a", 0), cand("b", 1)}
		out := refineByCosine("q", in, emb, nil, 10)
		if got := ids(out); got[0] != "a" || got[1] != "b" {
			t.Fatalf("expected order preserved, got %v", got)
		}
	})

	t.Run("nil embedder", func(t *testing.T) {
		vs := &fakeVectorSearcher{vecs: map[string][]float32{"a": {1, 0, 0}}}
		in := []*rerank.Candidate{cand("a", 0), cand("b", 1)}
		out := refineByCosine("q", in, nil, vs, 10)
		if got := ids(out); got[0] != "a" || got[1] != "b" {
			t.Fatalf("expected order preserved, got %v", got)
		}
	})
}

// TestRefineByCosine_NoopWhenQueryEmbedFails asserts a query embed
// failure leaves the order untouched rather than erroring out.
func TestRefineByCosine_NoopWhenQueryEmbedFails(t *testing.T) {
	vs := &fakeVectorSearcher{vecs: map[string][]float32{
		"a": {1, 0, 0}, "b": {0, 1, 0},
	}}
	emb := &fakeEmbedder{err: errors.New("embed boom")}
	in := []*rerank.Candidate{cand("a", 0), cand("b", 1)}
	out := refineByCosine("q", in, emb, vs, 10)
	if got := ids(out); got[0] != "a" || got[1] != "b" {
		t.Fatalf("expected order preserved on embed failure, got %v", got)
	}
}

// TestRefineByCosine_UnscoredCandidatesKeepTailOrder asserts that a
// candidate with no stored vector is never promoted above a scored one
// and that unscored candidates keep their relative incoming order.
func TestRefineByCosine_UnscoredCandidatesKeepTailOrder(t *testing.T) {
	query := []float32{1, 0, 0}
	// Only "b" and "d" have stored vectors; "a" and "c" do not. "d" is
	// closer to the query than "b". The scored pair must sort to the
	// front by cosine (d, b); the unscored pair must follow in their
	// incoming order (a, c).
	vs := &fakeVectorSearcher{vecs: map[string][]float32{
		"b": {1, 1, 0},    // ~0.707
		"d": {1, 0.05, 0}, // ~1.0
	}}
	emb := &fakeEmbedder{queryVec: query}

	in := []*rerank.Candidate{cand("a", 0), cand("b", 1), cand("c", 2), cand("d", 3)}
	out := refineByCosine("q", in, emb, vs, 10)

	got := ids(out)
	want := []string{"d", "b", "a", "c"}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("order = %v, want %v", got, want)
		}
	}
}

// TestRefineByCosine_OnlyTouchesTopN asserts the stage bounds its work
// to the top-N head: candidates beyond the bound keep their position
// and their vectors are never fetched.
func TestRefineByCosine_OnlyTouchesTopN(t *testing.T) {
	query := []float32{1, 0, 0}
	vs := &fakeVectorSearcher{vecs: map[string][]float32{
		"a": {0, 1, 0},    // orthogonal
		"b": {10, 0.1, 0}, // near-parallel
		"c": {5, 0, 0},    // parallel but outside the window
	}}
	emb := &fakeEmbedder{queryVec: query}

	in := []*rerank.Candidate{cand("a", 0), cand("b", 1), cand("c", 2)}
	// topN = 2 → only "a" and "b" participate; "c" stays put.
	out := refineByCosine("q", in, emb, vs, 2)

	got := ids(out)
	want := []string{"b", "a", "c"}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("order = %v, want %v", got, want)
		}
	}
	// The store must only have been asked for the head window IDs.
	for _, asked := range vs.askedFor {
		if asked == "c" {
			t.Fatalf("GetEmbeddings was asked for an out-of-window id %q", asked)
		}
	}
}

// TestRefineByCosine_NoopBelowTwoCandidates asserts the stage does not
// run (and never even embeds the query) for a trivial candidate set.
func TestRefineByCosine_NoopBelowTwoCandidates(t *testing.T) {
	vs := &fakeVectorSearcher{vecs: map[string][]float32{"a": {1, 0, 0}}}
	emb := &fakeEmbedder{queryVec: []float32{1, 0, 0}}
	in := []*rerank.Candidate{cand("a", 0)}
	out := refineByCosine("q", in, emb, vs, 10)
	if len(out) != 1 || out[0].Node.ID != "a" {
		t.Fatalf("single-candidate set must be returned unchanged")
	}
	if vs.getCalled {
		t.Fatalf("GetEmbeddings must not be called for a sub-2 candidate set")
	}
}

// emptyTextBackend is a no-op search.Backend used only to construct a
// HybridBackend in the embedder-unwrap test.
type emptyTextBackend struct{}

func (emptyTextBackend) Add(string, ...string)                    {}
func (emptyTextBackend) Remove(string)                            {}
func (emptyTextBackend) Search(string, int) []search.SearchResult { return nil }
func (emptyTextBackend) Count() int                               { return 0 }
func (emptyTextBackend) Close()                                   {}

// TestBackendEmbedder_UnwrapsSwappable asserts the embedder resolver
// finds the query embedder through the production backend chain
// (Swappable wrapping a HybridBackend) — the wiring the handler relies
// on — and returns nil for a plain text backend that carries none.
func TestBackendEmbedder_UnwrapsSwappable(t *testing.T) {
	emb := &fakeEmbedder{queryVec: []float32{1, 0, 0}}
	hybrid := search.NewHybrid(emptyTextBackend{}, search.NewDelegatedVector(3, &fakeVectorSearcher{}, 0, 0), emb)
	sw := search.NewSwappable(hybrid)

	if got := backendEmbedder(sw); got != emb {
		t.Fatalf("backendEmbedder must unwrap Swappable->Hybrid to the embedder, got %v", got)
	}
	if got := backendEmbedder(hybrid); got != emb {
		t.Fatalf("backendEmbedder must read the embedder off a bare Hybrid, got %v", got)
	}
	if got := backendEmbedder(search.NewSwappable(emptyTextBackend{})); got != nil {
		t.Fatalf("backendEmbedder must return nil for a text-only backend, got %v", got)
	}
	if got := backendEmbedder(nil); got != nil {
		t.Fatalf("backendEmbedder(nil) must be nil, got %v", got)
	}
}

// TestEngineRefineByCosine_NoopWhenStoreLacksVectors asserts the engine
// method no-ops cleanly when the underlying graph reader does not
// implement graph.VectorSearcher (the in-memory store) — proving the
// production wiring can never panic on a non-vector backend.
func TestEngineRefineByCosine_NoopWhenStoreLacksVectors(t *testing.T) {
	e := NewEngine(graph.New()) // *graph.Graph does NOT implement VectorSearcher
	in := []*rerank.Candidate{cand("a", 0), cand("b", 1)}
	out := e.RefineByCosine("q", in, 0)
	if got := ids(out); got[0] != "a" || got[1] != "b" {
		t.Fatalf("engine refine must be a no-op without a vector store, got %v", got)
	}
}
