package query

import (
	"context"
	"errors"
	"testing"

	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/search"
)

type contextPlainBackend struct {
	calls   int
	results []search.SearchResult
}

func (*contextPlainBackend) Add(string, ...string) {}
func (*contextPlainBackend) Remove(string)         {}
func (b *contextPlainBackend) Search(string, int) []search.SearchResult {
	b.calls++
	return b.results
}
func (*contextPlainBackend) Count() int { return 1 }
func (*contextPlainBackend) Close()     {}

type cancelingTextBackend struct {
	*contextPlainBackend
	cancel       context.CancelFunc
	contextCalls int
}

func (b *cancelingTextBackend) SearchContext(context.Context, string, int) []search.SearchResult {
	b.contextCalls++
	b.cancel()
	return []search.SearchResult{{ID: "discard"}}
}

type emptyBundleBackend struct {
	*contextPlainBackend
	bundleCalls int
}

func (b *emptyBundleBackend) SearchSymbolBundlesContext(context.Context, string, int) []search.SymbolBundle {
	b.bundleCalls++
	return []search.SymbolBundle{}
}

type nilBundleBackend struct{ *contextPlainBackend }

func (*nilBundleBackend) SearchSymbolBundlesContext(context.Context, string, int) []search.SymbolBundle {
	return nil
}

type bundleChannelBackend struct {
	*contextPlainBackend
	bundleCalls, channelCalls int
}

func (b *bundleChannelBackend) SearchSymbolBundles(string, int) []search.SymbolBundle {
	b.bundleCalls++
	return []search.SymbolBundle{{Node: &graph.Node{ID: "bundle"}}}
}
func (b *bundleChannelBackend) SearchChannels(string, int) ([]search.SearchResult, []string) {
	b.channelCalls++
	return []search.SearchResult{{ID: "text"}}, []string{"vector"}
}

type bundleVectorBackend struct {
	*contextPlainBackend
	bundleCalls, vectorCalls int
}

func (b *bundleVectorBackend) SearchSymbolBundles(string, int) []search.SymbolBundle {
	b.bundleCalls++
	return []search.SymbolBundle{{Node: &graph.Node{ID: "bundle"}}}
}
func (b *bundleVectorBackend) VectorChannelOnly(string, int) ([]string, search.ChannelTimings) {
	b.vectorCalls++
	return []string{"vector"}, search.ChannelTimings{VectorSearchMS: 7}
}

type timedOnlyBackend struct {
	*contextPlainBackend
	timedCalls int
}

func (b *timedOnlyBackend) SearchChannelsTimed(string, int) ([]search.SearchResult, []string, search.ChannelTimings) {
	b.timedCalls++
	return []search.SearchResult{{ID: "timed"}}, []string{"vector"}, search.ChannelTimings{TextMS: 3}
}

func TestRequestContextPreservesSwappableFallbackAndCancellation(t *testing.T) {
	plain := &contextPlainBackend{results: []search.SearchResult{{ID: "plain"}}}
	swappable := search.NewSwappable(plain)
	refill := viewBaseTextRefillContext(context.Background(), swappable, "needle", []string{"repo"})
	if got := refill(4); len(got) != 1 || got[0].ID != "plain" || plain.calls != 1 {
		t.Fatalf("live plain fallback got=%v calls=%d", got, plain.calls)
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if got := viewBaseTextRefillContext(ctx, swappable, "needle", []string{"repo"})(4); got != nil {
		t.Fatalf("canceled refill=%v", got)
	}
	if plain.calls != 1 {
		t.Fatalf("canceled request invoked inner: calls=%d", plain.calls)
	}

	nilContext := &nilBundleBackend{contextPlainBackend: plain}
	if got := viewBaseTextRefillContext(context.Background(), nilContext, "needle", nil)(4); len(got) != 1 || got[0].ID != "plain" {
		t.Fatalf("live contextual nil did not fall through: %v", got)
	}

	empty := &emptyBundleBackend{contextPlainBackend: plain}
	if got := viewBaseTextRefillContext(context.Background(), empty, "needle", nil)(4); len(got) != 0 {
		t.Fatalf("authoritative empty=%v", got)
	}
	if plain.calls != 2 {
		t.Fatalf("authoritative empty fell through: calls=%d", plain.calls)
	}
}

func TestRequestContextKeepsPartialOptionalCapabilities(t *testing.T) {
	bc := &bundleChannelBackend{contextPlainBackend: &contextPlainBackend{}}
	if a := requestSymbolBundles(context.Background(), bc, "q", 3); !a.authoritative || len(a.bundles) != 1 {
		t.Fatalf("bundle answer=%+v", a)
	}
	text, vector, _ := requestSearchChannels(context.Background(), bc, "q", 3)
	if len(text) != 1 || len(vector) != 1 || bc.bundleCalls != 1 || bc.channelCalls != 1 {
		t.Fatalf("bundle+channel text=%v vector=%v calls=%d/%d", text, vector, bc.bundleCalls, bc.channelCalls)
	}

	bv := &bundleVectorBackend{contextPlainBackend: &contextPlainBackend{}}
	ids, timing := requestVectorChannel(context.Background(), bv, "q", 3)
	if len(ids) != 1 || timing.VectorSearchMS != 7 || bv.vectorCalls != 1 {
		t.Fatalf("bundle+vector ids=%v timing=%+v calls=%d", ids, timing, bv.vectorCalls)
	}

	timed := &timedOnlyBackend{contextPlainBackend: &contextPlainBackend{}}
	text, vector, timing = requestSearchChannels(context.Background(), timed, "q", 3)
	if len(text) != 1 || len(vector) != 1 || timing.TextMS != 3 || timed.timedCalls != 1 {
		t.Fatalf("timed text=%v vector=%v timing=%+v calls=%d", text, vector, timing, timed.timedCalls)
	}
}

func TestViewContextStopsLayersAndRefillAfterCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	firstPlain := &contextPlainBackend{}
	first := &cancelingTextBackend{contextPlainBackend: firstPlain, cancel: cancel}
	second := &contextPlainBackend{results: []search.SearchResult{{ID: "second"}}}
	engine := &Engine{viewLayers: []ViewLayerSource{
		{Search: first, Layer: graph.NewOverlayLayer()},
		{Search: second, Layer: graph.NewOverlayLayer()},
	}}
	refills := 0
	got := engine.viewTextCandidatesContext(ctx, "q", 2, nil, func(int) []search.SearchResult {
		refills++
		return []search.SearchResult{{ID: "base"}}
	})
	if got != nil || first.contextCalls != 1 || ctx.Err() != context.Canceled || firstPlain.calls != 0 || second.calls != 0 || refills != 0 {
		t.Fatalf("got=%v firstContext=%d ctx=%v firstLegacy=%d second=%d refills=%d", got, first.contextCalls, ctx.Err(), firstPlain.calls, second.calls, refills)
	}
}

func TestGatherAndRankedReturnBeforeBackendWorkWhenCanceled(t *testing.T) {
	backend := &contextPlainBackend{results: []search.SearchResult{{ID: "missing"}}}
	engine := NewEngine(graph.New())
	engine.SetSearch(backend)
	engine.SetRerank(nil)

	engine.GatherSymbolCandidatesContext(context.Background(), "q", 5, QueryOptions{}, nil)
	if backend.calls == 0 {
		t.Fatal("live gather did not exercise backend")
	}
	backend.calls = 0

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if got := engine.GatherSymbolCandidatesContext(ctx, "q", 5, QueryOptions{}, nil); got != nil {
		t.Fatalf("canceled gather=%v", got)
	}
	if got := engine.SearchSymbolsRankedContext(ctx, "q", 5, QueryOptions{}, nil); got != nil {
		t.Fatalf("canceled ranked=%v", got)
	}
	if backend.calls != 0 {
		t.Fatalf("canceled gather/ranked invoked backend %d times", backend.calls)
	}
}

type requestFailureSymbolStore struct {
	graph.SymbolSearcher
	calls, searches int
	err             error
}

func (*requestFailureSymbolStore) SymbolFTSCount() (int, error) { return 1, nil }
func (s *requestFailureSymbolStore) SearchSymbolBundlesPathScopedContext(context.Context, string, []string, []string, int) ([]graph.SymbolBundle, error) {
	s.calls++
	return nil, s.err
}
func (s *requestFailureSymbolStore) SearchSymbolBundlesContext(context.Context, string, int) ([]graph.SymbolBundle, error) {
	s.calls++
	return nil, s.err
}
func (s *requestFailureSymbolStore) SearchSymbols(string, int) ([]graph.SymbolHit, error) {
	s.searches++
	return nil, s.err
}

type requestFailureVectorBackend struct {
	*search.SymbolSearcherBackend
	vectorCalls int
}

func (b *requestFailureVectorBackend) VectorChannelOnlyContext(context.Context, string, int) ([]string, search.ChannelTimings) {
	b.vectorCalls++
	return []string{"vector"}, search.ChannelTimings{}
}
func TestFailedBundleRetainsVectorAndExactWithoutTextReplay(t *testing.T) {
	g := graph.New()
	g.AddBatch([]*graph.Node{{ID: "exact", Name: "needle", Kind: graph.KindFunction, FilePath: "r/a.go"}, {ID: "vector", Name: "other", Kind: graph.KindFunction, FilePath: "r/b.go"}}, nil)
	source := &requestFailureSymbolStore{err: errors.New("bundle failed")}
	backend := &requestFailureVectorBackend{SymbolSearcherBackend: search.NewSymbolSearcherBackend(source)}
	engine := NewEngine(g)
	engine.SetSearch(search.NewSwappable(backend))
	engine.SetRerank(nil)
	observed := 0
	ctx := search.WithSearchFailureObserver(context.Background(), func(channel string, err error) {
		if channel == "text" && errors.Is(err, source.err) {
			observed++
		}
	})
	// Given a failed primary text bundle, retain independent vector and exact candidates without replay.
	got := engine.SearchSymbolsRankedContext(ctx, "needle", 5, QueryOptions{}, nil)
	seen := map[string]bool{}
	for _, candidate := range got {
		seen[candidate.Node.ID] = true
	}
	if !seen["exact"] || !seen["vector"] || source.calls != 1 || source.searches != 0 || backend.vectorCalls != 1 || observed != 1 {
		t.Fatalf("candidates=%v bundle=%d replay=%d vector=%d failures=%d", seen, source.calls, source.searches, backend.vectorCalls, observed)
	}
}
