package search

import (
	"context"
	"sort"
	"time"

	"github.com/zzet/gortex/internal/embedding"
	"github.com/zzet/gortex/internal/search/rerank"
)

// HybridBackend combines text search (the store-native FTS adapter)
// with durable vector search using query-adaptive, α-weighted
// Reciprocal Rank Fusion (RRF). Identifier-shaped queries lean toward the
// text channel, where exact-token matches are most reliable; natural-language
// queries give semantic similarity more weight so synonymous wording can surface.
type HybridBackend struct {
	text     Backend
	vector   *VectorBackend
	embedder embedding.Provider
	k        int // RRF constant (default 60)
}

// NewHybrid creates a hybrid search backend with adaptive α fusion.
func NewHybrid(text Backend, vector *VectorBackend, embedder embedding.Provider) *HybridBackend {
	return &HybridBackend{
		text:     text,
		vector:   vector,
		embedder: embedder,
		k:        60,
	}
}

// Add forwards a symbol update to the text backend. Vector corpora are
// prepared and published atomically by the indexer.
func (h *HybridBackend) Add(id string, fields ...string) {
	h.text.Add(id, fields...)
}

// Remove removes a symbol from the text backend. Vector corpus changes are
// published through the durable store, not this adapter.
func (h *HybridBackend) Remove(id string) {
	h.text.Remove(id)
}

// Search runs both text and vector search and fuses them with adaptive
// α-weighted RRF. Identifier queries lean toward text search; natural-language
// queries give semantic similarity more weight.
func (h *HybridBackend) Search(query string, limit int) []SearchResult {
	textResults, vecIDs, _ := h.searchChannels(query, limit)
	if len(vecIDs) == 0 {
		if len(textResults) > limit {
			return textResults[:limit]
		}
		return textResults
	}
	return alphaFuse(textResults, vecIDs, rerank.AlphaFor(query), h.k, limit)
}

// SearchChannels returns the raw per-channel results — text ranks
// (with scores) and the parallel vector-search ID list — without
// RRF fusion. The rerank pipeline calls this so each channel can
// contribute as a separate Signal instead of being collapsed into a
// single RRF score upstream of the rerank.
func (h *HybridBackend) SearchChannels(query string, limit int) (textResults []SearchResult, vectorIDs []string) {
	textResults, vectorIDs, _ = h.searchChannels(query, limit)
	return textResults, vectorIDs
}

// ChannelTimings carries per-phase wall-clock numbers from one
// SearchChannelsTimed call. Zero fields = phase didn't run (e.g.
// VectorSearchMS=0 when the vector index is empty).
type ChannelTimings struct {
	TextMS         int64
	EmbedMS        int64
	VectorSearchMS int64
}

// VectorChannelOnly returns the vector-channel IDs (embedder + vector
// search) WITHOUT re-running the text search. Used by the engine
// when the text channel has already been satisfied via the bundle
// path — the bundle returns Nodes + edges + scores already, so
// re-running text Search would double-pay the FTS cost. Returns
// nil and a zero ChannelTimings when the vector index is empty.
func (h *HybridBackend) VectorChannelOnly(query string, limit int) ([]string, ChannelTimings) {
	var stats ChannelTimings
	if h == nil || h.vector == nil || h.vector.Count() == 0 {
		return nil, stats
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	embedStart := time.Now()
	queryVec, err := h.embedder.Embed(ctx, query)
	stats.EmbedMS = time.Since(embedStart).Milliseconds()
	if err != nil || queryVec == nil {
		return nil, stats
	}
	fetch := limit * 2
	if h.vector.HasChunks() {
		fetch = limit * 8
	}
	vecStart := time.Now()
	rawVecIDs := h.vector.Search(queryVec, fetch)
	stats.VectorSearchMS = time.Since(vecStart).Milliseconds()
	return limitVectorIDs(rawVecIDs, limit*2), stats
}

// SearchChannelsTimed is SearchChannels with a per-phase timing
// breakdown so callers can prove which sub-step (text FTS vs
// vector embed vs vector search) actually cost wall-clock time.
// Used by the MCP search_symbols handler's debug-log
// instrumentation; production callers that don't care just use
// SearchChannels.
func (h *HybridBackend) SearchChannelsTimed(query string, limit int) ([]SearchResult, []string, ChannelTimings) {
	return h.searchChannels(query, limit)
}

// SearchSymbolBundles forwards to the text backend's bundle path when
// it implements SymbolBundleSearcherBackend. The vector channel does
// not participate — its IDs ride out through SearchChannels/Timed as
// before and the engine merges them with the bundle set. Returns nil
// when the text backend has no bundle support (no-op for the
// fallback path).
//
// HybridBackend wires both channels together in production, so the
// engine's bundle-detection step type-asserts on the outer
// HybridBackend through Swappable; this is what makes the bundle
// path available when the daemon's search is the FTS + vector
// stack instead of a bare SymbolSearcherBackend.
func (h *HybridBackend) SearchSymbolBundles(query string, limit int) []SymbolBundle {
	if h == nil || h.text == nil {
		return nil
	}
	if bs, ok := h.text.(SymbolBundleSearcherBackend); ok {
		return bs.SearchSymbolBundles(query, limit)
	}
	return nil
}

// DocCount forwards the text backend's authoritative corpus size for
// the engine's has-corpus gate, mirroring the Swappable forward.
func (h *HybridBackend) DocCount() (int, bool) {
	if h == nil || h.text == nil {
		return 0, false
	}
	if dc, ok := h.text.(DocCounter); ok {
		return dc.DocCount()
	}
	return 0, false
}

// SearchSymbolBundlesScoped forwards the repo-narrowed bundle path to
// the text backend; the vector channel does not participate, exactly
// as in SearchSymbolBundles.
func (h *HybridBackend) SearchSymbolBundlesScoped(query string, repoAllow []string, limit int) []SymbolBundle {
	if h == nil || h.text == nil {
		return nil
	}
	if bs, ok := h.text.(ScopedSymbolBundleSearcherBackend); ok {
		return bs.SearchSymbolBundlesScoped(query, repoAllow, limit)
	}
	return nil
}

func (h *HybridBackend) searchChannels(query string, limit int) ([]SearchResult, []string, ChannelTimings) {
	var stats ChannelTimings
	tStart := time.Now()
	textResults := h.text.Search(query, limit*2)
	stats.TextMS = time.Since(tStart).Milliseconds()

	var vecIDs []string
	if h.vector.Count() > 0 {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		embedStart := time.Now()
		queryVec, err := h.embedder.Embed(ctx, query)
		stats.EmbedMS = time.Since(embedStart).Milliseconds()
		if err == nil && queryVec != nil {
			// When symbols are sub-chunked, one symbol owns several
			// vectors, so a fixed top-k under-counts distinct symbols.
			// Over-fetch, then retain up to limit*2 distinct symbols.
			fetch := limit * 2
			if h.vector.HasChunks() {
				fetch = limit * 8
			}
			vecStart := time.Now()
			rawVecIDs := h.vector.Search(queryVec, fetch)
			stats.VectorSearchMS = time.Since(vecStart).Milliseconds()
			vecIDs = limitVectorIDs(rawVecIDs, limit*2)
		}
	}
	return textResults, vecIDs, stats
}

// limitVectorIDs caps the ordered, deduplicated IDs from the vector backend.
func limitVectorIDs(ids []string, want int) []string {
	if ids == nil {
		return []string{}
	}
	return ids[:min(len(ids), max(want, 0))]
}

// Count reports the corpus visible to hybrid retrieval. A positive text count
// remains authoritative; vector-only hybrids fall back to their vector count.
// The channel counts are alternatives, not additive views of the same symbols.
func (h *HybridBackend) Count() int {
	if h == nil {
		return 0
	}
	if h.text != nil {
		if count := h.text.Count(); count > 0 {
			return count
		}
	}
	if h.vector != nil {
		return h.vector.Count()
	}
	return 0
}

// Close releases resources owned by the hybrid. The embedding provider and a
// delegated vector searcher are externally owned; VectorBackend.Close only
// detaches the delegate.
func (h *HybridBackend) Close() {
	if h == nil {
		return
	}
	text := h.text
	vector := h.vector
	h.text = nil
	h.vector = nil
	h.embedder = nil
	if vector != nil {
		vector.Close()
	}
	if text != nil {
		text.Close()
	}
}

// detachTextBackend transfers text-backend ownership to a replacement hybrid.
// It is intentionally private and may only be called after all users of h have
// drained (Swappable.ReplaceHybridVector holds the write lock when calling it).
func (h *HybridBackend) detachTextBackend() Backend {
	if h == nil {
		return nil
	}
	text := h.text
	h.text = nil
	return text
}

// TextBackend returns the underlying text search backend.
func (h *HybridBackend) TextBackend() Backend { return h.text }

// VectorBackend returns the underlying vector search backend.
func (h *HybridBackend) VectorIndex() *VectorBackend { return h.vector }

// Embedder returns the embedding provider.
func (h *HybridBackend) Embedder() embedding.Provider { return h.embedder }

// SizeBytes returns the sum of text and vector backend sizes.
func (h *HybridBackend) SizeBytes() uint64 {
	return BackendSize(h.text) + h.vector.SizeBytes()
}

// VectorSizeBytes returns just the vector backend's size.
func (h *HybridBackend) VectorSizeBytes() uint64 { return h.vector.SizeBytes() }

// alphaFuse combines text and vector results with an α-weighted blend
// of their reciprocal-rank contributions. Higher α gives the vector
// channel more weight (good for natural-language queries where
// semantic similarity catches synonyms); lower α gives the text channel
// more weight (good for identifier queries where exact-token matches
// are the most reliable signal).
//
// Formula:
//
//	score(doc) = (1-α) × 1/(k+rank_text+1) + α × 1/(k+rank_vector+1)
//
// α=0 reduces to text-only; α=1 reduces to vector-only; α=0.5 is
// equal-weight RRF with each channel halved, so absolute scores differ
// from the unscaled formula but relative ordering is unchanged.
func alphaFuse(textResults []SearchResult, vecIDs []string, alpha float64, k, limit int) []SearchResult {
	if alpha < 0 {
		alpha = 0
	}
	if alpha > 1 {
		alpha = 1
	}
	textWeight := 1.0 - alpha
	vecWeight := alpha
	scores := make(map[string]float64)

	for rank, r := range textResults {
		scores[r.ID] += textWeight / float64(k+rank+1)
	}
	for rank, id := range vecIDs {
		scores[id] += vecWeight / float64(k+rank+1)
	}

	type scored struct {
		id    string
		score float64
	}
	results := make([]scored, 0, len(scores))
	for id, score := range scores {
		results = append(results, scored{id: id, score: score})
	}
	sort.Slice(results, func(i, j int) bool {
		if results[i].score != results[j].score {
			return results[i].score > results[j].score
		}
		// Stable secondary key: id ascending so identical-score
		// runs ship in a deterministic order across calls.
		return results[i].id < results[j].id
	})

	if len(results) > limit {
		results = results[:limit]
	}
	out := make([]SearchResult, len(results))
	for i, r := range results {
		out[i] = SearchResult{ID: r.id, Score: r.score}
	}
	return out
}
