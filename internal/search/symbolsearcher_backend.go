package search

import (
	"strings"

	"github.com/zzet/gortex/internal/graph"
)

// SymbolSearcherBackend adapts a graph.SymbolSearcher into the
// search.Backend the daemon's search-symbols path consumes.
// Engine.gatherBackendCandidates and the rerank pipeline don't need
// to know where the ranking comes from — they see a plain
// search.Backend and call Search on it.
//
// Production wiring: when the indexer detects that the backing
// graph.Store also implements graph.SymbolSearcher, it constructs
// this adapter as the initial search.Backend wrapped by
// search.NewSwappable. A store without that capability gets
// NullBackend instead, and the engine falls back to its substring
// scan — no text index is ever built in this process.
//
// Add / Remove are no-ops on the adapter because the indexer
// already drives the SymbolSearcher writes directly:
//
//   - cold-load: BulkUpsertSymbolFTS at shadow-drain commit (see
//     internal/indexer.go IndexCtx defer)
//   - incremental: UpsertSymbolFTS alongside the parallel
//     idx.search.Add in the per-file path
//
// The adapter therefore only carries the read side. Callers that
// invoke Add / Remove still get the right behaviour because the
// indexer is the only entity that ever creates this adapter, and
// it doesn't rely on Add / Remove updating the FTS — those calls
// happen through the direct SymbolSearcher surface.
type SymbolSearcherBackend struct {
	s graph.SymbolSearcher
}

// NewSymbolSearcherBackend wraps a SymbolSearcher in the
// search.Backend contract. The caller is responsible for keeping
// the underlying SymbolSearcher alive — Close on this adapter is
// a no-op and never touches the wrapped store.
func NewSymbolSearcherBackend(s graph.SymbolSearcher) *SymbolSearcherBackend {
	return &SymbolSearcherBackend{s: s}
}

// SymbolBundle re-exports graph.SymbolBundle so callers (the query
// engine, the rerank seed path) can construct + consume bundles
// without re-importing the graph package next to the search
// package import — symmetric with how SearchResult sits in
// search/.
type SymbolBundle = graph.SymbolBundle

// SearchSymbolBundles is the bundled-search hot path: it forwards
// to the wrapped graph.SymbolBundleSearcher when the underlying
// store implements that capability, returning the matched node +
// score + in/out edges in one engine round-trip. When the store
// only implements SymbolSearcher (no Bundle support), this method
// returns nil — callers MUST check the result and fall back to the
// per-call Search → GetNodesByIDs → GetIn/OutEdgesByNodeIDs path.
//
// Exposed on SymbolSearcherBackend (the production search.Backend
// adapter used in production) so the engine can type-assert through
// the search.Backend chain via SymbolBundleSearcherBackend without
// touching the daemon's wiring.
func (b *SymbolSearcherBackend) SearchSymbolBundles(query string, limit int) []SymbolBundle {
	if b == nil || b.s == nil || strings.TrimSpace(query) == "" {
		return nil
	}
	bs, ok := b.s.(graph.SymbolBundleSearcher)
	if !ok {
		return nil
	}
	bundles, err := bs.SearchSymbolBundles(query, limit)
	if err != nil {
		return nil
	}
	return bundles
}

// SymbolBundleSearcherBackend is the interface the engine type-asserts
// on a search.Backend to detect bundle support. Both
// *SymbolSearcherBackend and *HybridBackend implement this; Swappable
// forwards.
type SymbolBundleSearcherBackend interface {
	SearchSymbolBundles(query string, limit int) []SymbolBundle
}

// SearchSymbolBundlesScoped is the repo-narrowed bundle path — the
// narrowing runs inside the wrapped store's FTS query (see
// graph.ScopedSymbolBundleSearcher for why a post-fetch filter can't
// substitute). Returns nil when the store has no scoped support;
// callers fall back to the unscoped bundle path.
func (b *SymbolSearcherBackend) SearchSymbolBundlesScoped(query string, repoAllow []string, limit int) []SymbolBundle {
	if b == nil || b.s == nil || strings.TrimSpace(query) == "" {
		return nil
	}
	bs, ok := b.s.(graph.ScopedSymbolBundleSearcher)
	if !ok {
		return nil
	}
	bundles, err := bs.SearchSymbolBundlesRepoScoped(query, repoAllow, limit)
	if err != nil {
		return nil
	}
	if bundles == nil {
		// Non-nil empty = "scoped path answered: nothing in scope".
		// Callers fall back to the unscoped fetch only on nil — a
		// genuine zero must not trigger a doomed cross-repo flood
		// query whose head ScopeAllows would discard wholesale.
		return []SymbolBundle{}
	}
	return bundles
}

// ScopedSymbolBundleSearcherBackend is the engine's detection
// interface for the repo-narrowed bundle path. *SymbolSearcherBackend
// implements it; Swappable and HybridBackend forward.
type ScopedSymbolBundleSearcherBackend interface {
	SearchSymbolBundlesScoped(query string, repoAllow []string, limit int) []SymbolBundle
}

// Search forwards to SymbolSearcher.SearchSymbols and translates
// the per-hit (NodeID, Score) into search.SearchResult so callers
// don't see the graph package at all.
//
// An error from the backend is downgraded to an empty result — the
// daemon's search_symbols path already tolerates an empty primary
// hit set (it falls through to the exact-name / substring tiers in
// query.Engine.gatherBackendCandidates), so returning an error
// surface here would force every caller to grow its own fallback. Context-free
// callers retain this best-effort contract; request-aware callers can observe
// actual failures with WithSearchFailureObserver without losing other tiers.
func (b *SymbolSearcherBackend) Search(query string, limit int) []SearchResult {
	if b == nil || b.s == nil || strings.TrimSpace(query) == "" {
		return nil
	}
	hits, err := b.s.SearchSymbols(query, limit)
	if err != nil || len(hits) == 0 {
		return nil
	}
	out := make([]SearchResult, len(hits))
	for i, h := range hits {
		out[i] = SearchResult{ID: h.NodeID, Score: h.Score}
	}
	return out
}

// Add is a no-op — the indexer drives UpsertSymbolFTS on the wrapped
// SymbolSearcher directly.
func (b *SymbolSearcherBackend) Add(string, ...string) {}

// Remove is a no-op for the same reason as Add — the removal path routes
// through SymbolSearcher directly, not through the search.Backend contract.
func (b *SymbolSearcherBackend) Remove(string) {}

// Count returns the authoritative persisted corpus size when the wrapped store
// exposes it. Add and Remove deliberately do not maintain a second local count:
// their corresponding FTS writes happen directly on the store, so applying the
// same deltas here would double-count them.
func (b *SymbolSearcherBackend) Count() int {
	count, ok := b.DocCount()
	if !ok {
		return 0
	}
	return count
}

// DocCounter is the capability interface for the authoritative corpus size.
// The engine's has-corpus gate asserts this; Swappable and HybridBackend
// forward it.
type DocCounter interface {
	DocCount() (int, bool)
}

// DocCount returns the authoritative number of indexed documents, straight
// from the underlying index, and reports whether it could be obtained.
//
// Count delegates to this method as well, so readiness checks and user-facing
// status observe the same store-owned value.
func (b *SymbolSearcherBackend) DocCount() (int, bool) {
	if b == nil || b.s == nil {
		return 0, false
	}
	counter, ok := b.s.(graph.SymbolFTSCounter)
	if !ok {
		return 0, false
	}
	count, err := counter.SymbolFTSCount()
	if err != nil || count < 0 {
		return 0, false
	}
	return count, true
}

// Close is a no-op. The wrapped SymbolSearcher is owned by the
// graph.Store; closing it from the search adapter would race the
// indexer's own lifecycle.
func (b *SymbolSearcherBackend) Close() {}
