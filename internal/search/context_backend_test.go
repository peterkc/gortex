package search

import (
	"context"
	"errors"
	"reflect"
	"testing"

	"github.com/zzet/gortex/internal/graph"
)

type contextGraphSearcherStub struct {
	graph.SymbolSearcher

	legacyCalls       int
	contextCalls      int
	bundleCalls       int
	scopedBundleCalls int
	contextErr        error
	hits              []graph.SymbolHit
	bundles           []graph.SymbolBundle
}

func (s *contextGraphSearcherStub) SearchSymbols(string, int) ([]graph.SymbolHit, error) {
	s.legacyCalls++
	return s.hits, nil
}

func (s *contextGraphSearcherStub) SearchSymbolsContext(context.Context, string, int) ([]graph.SymbolHit, error) {
	s.contextCalls++
	return s.hits, s.contextErr
}

func (s *contextGraphSearcherStub) SearchSymbolBundlesContext(context.Context, string, int) ([]graph.SymbolBundle, error) {
	s.bundleCalls++
	return s.bundles, s.contextErr
}

func (s *contextGraphSearcherStub) SearchSymbolBundlesRepoScopedContext(context.Context, string, []string, int) ([]graph.SymbolBundle, error) {
	s.scopedBundleCalls++
	return s.bundles, s.contextErr
}

type legacyGraphSearcherStub struct {
	graph.SymbolSearcher
	calls int
	hits  []graph.SymbolHit
}

func (s *legacyGraphSearcherStub) SearchSymbols(string, int) ([]graph.SymbolHit, error) {
	s.calls++
	return s.hits, nil
}

type contextBackendStub struct {
	contextCalls int
	legacyCalls  int
	timedCalls   int
	results      []SearchResult
}

func (*contextBackendStub) Add(string, ...string) {}
func (*contextBackendStub) Remove(string)         {}
func (s *contextBackendStub) Search(string, int) []SearchResult {
	s.legacyCalls++
	return s.results
}
func (*contextBackendStub) Count() int { return 1 }
func (*contextBackendStub) Close()     {}
func (s *contextBackendStub) SearchContext(context.Context, string, int) []SearchResult {
	s.contextCalls++
	return s.results
}
func (s *contextBackendStub) SearchChannelsTimedContext(context.Context, string, int) ([]SearchResult, []string, ChannelTimings) {
	s.timedCalls++
	return s.results, nil, ChannelTimings{TextMS: 1}
}

func TestSymbolSearcherBackendSearchContextIsAuthoritative(t *testing.T) {
	source := &contextGraphSearcherStub{
		hits: []graph.SymbolHit{{NodeID: "symbol", Score: 7}},
	}
	backend := NewSymbolSearcherBackend(source)

	got := backend.SearchContext(context.Background(), "symbol query", 5)
	if len(got) != 1 || got[0].ID != "symbol" || got[0].Score != 7 {
		t.Fatalf("context results = %#v", got)
	}
	if source.contextCalls != 1 || source.legacyCalls != 0 {
		t.Fatalf("calls after context search = context:%d legacy:%d", source.contextCalls, source.legacyCalls)
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if got := backend.SearchContext(ctx, "symbol query", 5); got != nil {
		t.Fatalf("pre-canceled results = %#v", got)
	}
	if source.contextCalls != 1 || source.legacyCalls != 0 {
		t.Fatalf("pre-canceled search invoked source: context:%d legacy:%d", source.contextCalls, source.legacyCalls)
	}

	source.contextErr = errors.New("context search failed")
	if got := backend.SearchContext(context.Background(), "symbol query", 5); got != nil {
		t.Fatalf("failed context results = %#v", got)
	}
	if source.contextCalls != 2 || source.legacyCalls != 0 {
		t.Fatalf("context error fell back: context:%d legacy:%d", source.contextCalls, source.legacyCalls)
	}
}

func TestSymbolSearcherBackendSearchContextUsesLiveLegacyFallbackOnly(t *testing.T) {
	source := &legacyGraphSearcherStub{hits: []graph.SymbolHit{{NodeID: "legacy", Score: 3}}}
	backend := NewSymbolSearcherBackend(source)

	got := backend.SearchContext(context.Background(), "legacy query", 5)
	if len(got) != 1 || got[0].ID != "legacy" {
		t.Fatalf("legacy fallback results = %#v", got)
	}
	if source.calls != 1 {
		t.Fatalf("legacy calls = %d, want 1", source.calls)
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if got := backend.SearchContext(ctx, "legacy query", 5); got != nil {
		t.Fatalf("pre-canceled legacy results = %#v", got)
	}
	if source.calls != 1 {
		t.Fatalf("pre-canceled request invoked legacy source; calls = %d", source.calls)
	}
}

func TestSymbolSearcherBackendContextBundlePathsDoNotFallback(t *testing.T) {
	source := &contextGraphSearcherStub{}
	backend := NewSymbolSearcherBackend(source)

	if got := backend.SearchSymbolBundlesScopedContext(context.Background(), "query", []string{"repo"}, 5); got == nil || len(got) != 0 {
		t.Fatalf("scoped empty result = %#v, want non-nil empty", got)
	}
	if source.scopedBundleCalls != 1 {
		t.Fatalf("scoped context bundle calls = %d, want 1", source.scopedBundleCalls)
	}

	source.contextErr = context.Canceled
	if got := backend.SearchSymbolBundlesContext(context.Background(), "query", 5); got != nil {
		t.Fatalf("canceled bundle results = %#v", got)
	}
	if source.bundleCalls != 1 || source.legacyCalls != 0 {
		t.Fatalf("bundle error fell back: bundle:%d legacy:%d", source.bundleCalls, source.legacyCalls)
	}
}

func TestSwappableContextForwardersPreferContextCapabilities(t *testing.T) {
	inner := &contextBackendStub{results: []SearchResult{{ID: "context", Score: 1}}}
	swappable := NewSwappable(inner)

	got := swappable.SearchContext(context.Background(), "query", 5)
	if len(got) != 1 || got[0].ID != "context" {
		t.Fatalf("context results = %#v", got)
	}
	if inner.contextCalls != 1 || inner.legacyCalls != 0 {
		t.Fatalf("search calls = context:%d legacy:%d", inner.contextCalls, inner.legacyCalls)
	}

	text, vector, timings := swappable.SearchChannelsTimedContext(context.Background(), "query", 5)
	if len(text) != 1 || len(vector) != 0 || timings.TextMS != 1 {
		t.Fatalf("timed channels = text:%#v vector:%#v timings:%+v", text, vector, timings)
	}
	if inner.timedCalls != 1 {
		t.Fatalf("timed context calls = %d, want 1", inner.timedCalls)
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if got := swappable.SearchContext(ctx, "query", 5); got != nil {
		t.Fatalf("pre-canceled swappable results = %#v", got)
	}
	if inner.contextCalls != 1 || inner.legacyCalls != 0 {
		t.Fatalf("pre-canceled swappable invoked inner: context:%d legacy:%d", inner.contextCalls, inner.legacyCalls)
	}
}

func TestSearchFailureObserversComposeAndDoNotWarnForWorkingFallback(t *testing.T) {
	cause := errors.New("search failure")
	source := &contextGraphSearcherStub{contextErr: cause}
	backend := NewSymbolSearcherBackend(source)
	outer, inner := 0, 0
	ctx := WithSearchFailureObserver(context.Background(), func(channel string, err error) {
		if channel == "text" && errors.Is(err, cause) {
			outer++
		}
	})
	ctx = WithSearchFailureObserver(ctx, func(channel string, err error) {
		if channel == "text" && errors.Is(err, cause) {
			inner++
		}
	})
	// Given three actual store failures, every composed observer sees the original error once per call.
	backend.SearchContext(ctx, "needle", 3)
	backend.SearchSymbolBundlesContext(ctx, "needle", 3)
	backend.SearchSymbolBundlesScopedContext(ctx, "needle", []string{"r"}, 3)
	if outer != 3 || inner != 3 || source.contextCalls != 1 || source.bundleCalls != 1 || source.scopedBundleCalls != 1 || source.legacyCalls != 0 {
		t.Fatalf("observations=%d/%d source=%+v", outer, inner, source)
	}
	source.contextErr = nil
	backend.SearchContext(ctx, "needle", 3)
	backend.SearchSymbolBundlesContext(ctx, "needle", 3)
	backend.SearchSymbolBundlesScopedContext(ctx, "needle", nil, 3)
	legacy := &legacyGraphSearcherStub{hits: []graph.SymbolHit{{NodeID: "legacy", Score: 1}}}
	fallback := NewSwappable(NewHybrid(NewSymbolSearcherBackend(legacy), nil, nil))
	hits := fallback.SearchContext(ctx, "needle", 3)
	fallback.SearchSymbolBundlesContext(ctx, "needle", 3)
	fallback.SearchSymbolBundlesScopedContext(ctx, "needle", nil, 3)
	_, handled := fallback.SearchSymbolBundlesPathScopedContext(ctx, "needle", nil, []string{"src"}, 3)
	if len(hits) != 1 || hits[0].ID != "legacy" || handled || outer != 3 || inner != 3 || legacy.calls != 1 {
		t.Fatalf("working optional fallback=%v/%t observations=%d/%d calls%d", hits, handled, outer, inner, legacy.calls)
	}
	canceled, cancel := context.WithCancel(ctx)
	cancel()
	backend.SearchContext(canceled, "needle", 3)
	if source.contextCalls != 2 || outer != 3 || inner != 3 {
		t.Fatal("pre-canceled request started work or emitted a backend failure")
	}
}

type searchFailureEmbedder struct {
	fixedEmbedder
	err error
}

func (e searchFailureEmbedder) Embed(context.Context, string) ([]float32, error) { return nil, e.err }
func TestHybridFailureObserverNamesVectorAndRetainsText(t *testing.T) {
	vector := NewVector(2)
	vector.Add("vector", []float32{1, 0})
	cause := errors.New("embedding failed")
	text := &contextBackendStub{results: []SearchResult{{ID: "text", Score: 2}}}
	backend := NewSwappable(NewHybrid(text, vector, searchFailureEmbedder{fixedEmbedder{dims: 2}, cause}))
	var channels []string
	ctx := WithSearchFailureObserver(context.Background(), func(channel string, err error) {
		if errors.Is(err, cause) {
			channels = append(channels, channel)
		}
	})
	// Given failed vector embedding, the independent text channel stays useful.
	hits := backend.SearchContext(ctx, "needle", 3)
	if len(hits) != 1 || hits[0].ID != "text" || len(channels) != 1 || channels[0] != "vector" {
		t.Fatalf("hits=%v failures=%v", hits, channels)
	}
}

type legacyFailureSymbolSearcher struct {
	graph.SymbolSearcher
	err   error
	calls int
}

func (s *legacyFailureSymbolSearcher) SearchSymbols(string, int) ([]graph.SymbolHit, error) {
	s.calls++
	return nil, s.err
}
func (s *legacyFailureSymbolSearcher) SearchSymbolBundles(string, int) ([]graph.SymbolBundle, error) {
	s.calls++
	return nil, s.err
}
func (s *legacyFailureSymbolSearcher) SearchSymbolBundlesRepoScoped(string, []string, int) ([]graph.SymbolBundle, error) {
	s.calls++
	return nil, s.err
}
func TestLegacyStoreFailuresAreObservedOnlyForContextualCalls(t *testing.T) {
	source := &legacyFailureSymbolSearcher{err: errors.New("legacy failure")}
	backend := NewSymbolSearcherBackend(source)
	observations := 0
	ctx := WithSearchFailureObserver(context.Background(), func(channel string, err error) {
		if channel == "text" && errors.Is(err, source.err) {
			observations++
		}
	})
	// Given legacy store errors, the request-aware fallback observes them without issuing a second query.
	backend.SearchContext(ctx, "needle", 3)
	backend.SearchSymbolBundlesContext(ctx, "needle", 3)
	backend.SearchSymbolBundlesScopedContext(ctx, "needle", nil, 3)
	if observations != 3 || source.calls != 3 {
		t.Fatalf("legacy observations=%d calls=%d", observations, source.calls)
	}
	backend.Search("needle", 3)
	backend.SearchSymbolBundles("needle", 3)
	backend.SearchSymbolBundlesScoped("needle", nil, 3)
	if observations != 3 || source.calls != 6 {
		t.Fatal("context-free best-effort contract changed")
	}
}

type isolatedFailureSymbolSearcher struct{ graph.SymbolSearcher }

func (*isolatedFailureSymbolSearcher) SearchSymbolsContext(_ context.Context, q string, _ int) ([]graph.SymbolHit, error) {
	if q == "fail" {
		return nil, errors.New("request failure")
	}
	return []graph.SymbolHit{{NodeID: "healthy"}}, nil
}
func TestSearchFailureObserversRemainRequestLocalOnSharedBackend(t *testing.T) {
	backend := NewSymbolSearcherBackend(&isolatedFailureSymbolSearcher{})
	type outcome struct {
		query    string
		hits     []SearchResult
		channels []string
	}
	done := make(chan outcome, 2)
	// Given simultaneous requests on one backend, only the failing request owns its diagnostic.
	for _, q := range []string{"fail", "healthy"} {
		go func(q string) {
			var channels []string
			ctx := WithSearchFailureObserver(context.Background(), func(channel string, _ error) { channels = append(channels, channel) })
			hits := backend.SearchContext(ctx, q, 3)
			done <- outcome{q, hits, channels}
		}(q)
	}
	for range 2 {
		got := <-done
		if got.query == "fail" {
			if len(got.hits) != 0 || !reflect.DeepEqual(got.channels, []string{"text"}) {
				t.Fatalf("failed outcome=%+v", got)
			}
		} else if len(got.hits) != 1 || got.hits[0].ID != "healthy" || len(got.channels) != 0 {
			t.Fatalf("healthy outcome=%+v", got)
		}
	}
}
