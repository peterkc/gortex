package search

import (
	"context"
	"errors"
	"github.com/zzet/gortex/internal/graph"
	"testing"
)

type sequentialOnlyBackend struct{ searches int }

func (b *sequentialOnlyBackend) Add(string, ...string) {}
func (b *sequentialOnlyBackend) Remove(string)         {}
func (b *sequentialOnlyBackend) Search(string, int) []SearchResult {
	b.searches++
	return []SearchResult{{ID: "sequential"}}
}
func (b *sequentialOnlyBackend) Count() int { return 0 }
func (b *sequentialOnlyBackend) Close()     {}
func (b *sequentialOnlyBackend) SearchContext(context.Context, string, int) []SearchResult {
	b.searches++
	return []SearchResult{{ID: "sequential-context"}}
}

func TestHybridAndSwappableKeepSequentialViewFallback(t *testing.T) {
	hybridInner := &sequentialOnlyBackend{}
	hybrid := &HybridBackend{text: hybridInner}
	if _, ok := Backend(hybrid).(ContextViewBatchSearcherBackend); ok {
		t.Fatal("HybridBackend unexpectedly exposes view batching")
	}
	if got := hybrid.SearchContext(context.Background(), "query", 2); len(got) != 1 || got[0].ID != "sequential-context" {
		t.Fatalf("hybrid sequential result = %#v", got)
	}
	if hybridInner.searches != 1 {
		t.Fatalf("hybrid sequential calls = %d, want 1", hybridInner.searches)
	}

	swappableInner := &sequentialOnlyBackend{}
	swappable := &Swappable{inner: swappableInner}
	if _, ok := Backend(swappable).(ContextViewBatchSearcherBackend); ok {
		t.Fatal("Swappable unexpectedly exposes view batching")
	}
	if got := swappable.SearchContext(context.Background(), "query", 2); len(got) != 1 || got[0].ID != "sequential-context" {
		t.Fatalf("swappable sequential result = %#v", got)
	}
	if swappableInner.searches != 1 {
		t.Fatalf("swappable sequential calls = %d, want 1", swappableInner.searches)
	}
}

type failingViewGraphSearcher struct {
	contextGraphSearcherStub
	batchCalls int
}

func (*failingViewGraphSearcher) SymbolSearchViewGeneration() int64 { return 2 }
func (*failingViewGraphSearcher) SharesSymbolSearchCore(other any) bool {
	_, ok := other.(*failingViewGraphSearcher)
	return ok
}
func (s *failingViewGraphSearcher) SearchSymbolsViewGenerationsRepoScopedContext(context.Context, string, []string, []int64, int) (map[int64][]graph.SymbolHit, error) {
	s.batchCalls++
	return nil, s.contextErr
}
func TestFailedViewBatchObservesTextAndRemainsHandled(t *testing.T) {
	source := &failingViewGraphSearcher{contextGraphSearcherStub: contextGraphSearcherStub{contextErr: errors.New("batch failure")}}
	adapter := NewSymbolSearcherBackend(source)
	calls := 0
	ctx := WithSearchFailureObserver(context.Background(), func(channel string, err error) {
		if channel == "text" && errors.Is(err, source.contextErr) {
			calls++
		}
	})
	// Given a handled batch failure, do not replay it as sequential symbol queries.
	results, handled := adapter.SearchViewBatchContext(ctx, "needle", []Backend{adapter, adapter}, 3)
	if !handled || len(results) != 2 || len(results[0]) != 0 || len(results[1]) != 0 || calls != 1 || source.batchCalls != 1 || source.contextCalls != 0 || source.legacyCalls != 0 {
		t.Fatalf("batch=%v/%t observations=%d source=%+v", results, handled, calls, source)
	}
}
