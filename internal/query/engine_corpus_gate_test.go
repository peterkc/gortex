package query

import (
	"testing"

	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/search"
)

// warmStoreBackend models a backend over a pre-populated disk FTS
// right after a daemon restart: the Add/Remove delta counter is zero
// (nothing indexed THIS process) but the disk corpus is fully loaded.
type warmStoreBackend struct {
	hits          []search.SearchResult
	docs          int
	docCountCalls int
}

func (b *warmStoreBackend) Add(string, ...string) {}
func (b *warmStoreBackend) Remove(string)         {}
func (b *warmStoreBackend) Count() int            { return 0 }
func (b *warmStoreBackend) Close()                {}
func (b *warmStoreBackend) DocCount() (int, bool) {
	b.docCountCalls++
	return b.docs, true
}
func (b *warmStoreBackend) Search(string, int) []search.SearchResult {
	return append([]search.SearchResult(nil), b.hits...)
}

// TestGatherSymbolCandidates_WarmStoreUsesBackend: a zero delta
// counter must not divert the engine to the substring fallback when
// the backend's authoritative DocCount proves the corpus exists —
// otherwise every daemon restart over an existing store silently
// degrades search until the first file edit bumps the counter.
func TestGatherSymbolCandidates_WarmStoreUsesBackend(t *testing.T) {
	g := graph.New()
	n := &graph.Node{ID: "app/w.go::WidgetExtensions", Name: "WidgetExtensions", Kind: graph.KindType, RepoPrefix: "app"}
	g.AddNode(n)
	backend := &warmStoreBackend{hits: []search.SearchResult{{ID: n.ID, Score: 5}}, docs: 12345}
	engine := NewEngine(g)
	engine.SetSearch(backend)

	// A two-word concept query: the substring fallback cannot match it
	// against "WidgetExtensions", so a result can only come from the
	// backend — the discriminator between the two paths.
	got := engine.GatherSymbolCandidates("widget extensions", 5, QueryOptions{SkipInnerRerank: true, SkipVectorChannel: true}, nil)
	if len(got) != 1 || got[0].Node.ID != n.ID {
		t.Fatalf("warm-store backend was bypassed (substring fallback?); got %#v", got)
	}
	if got[0].TextRank != 0 {
		t.Fatalf("backend hit must carry its text rank; got %#v", got[0])
	}
}

// TestGatherSymbolCandidates_CorpusGateMemoized: DocCount is a real
// COUNT(*) on the disk store, and the warm-restart window makes the
// gate fire on every gather — a positive answer is monotone for the
// backend's lifetime and must be asked at most once.
func TestGatherSymbolCandidates_CorpusGateMemoized(t *testing.T) {
	g := graph.New()
	n := &graph.Node{ID: "app/w.go::WidgetExtensions", Name: "WidgetExtensions", Kind: graph.KindType, RepoPrefix: "app"}
	g.AddNode(n)
	backend := &warmStoreBackend{hits: []search.SearchResult{{ID: n.ID, Score: 5}}, docs: 12345}
	engine := NewEngine(g)
	engine.SetSearch(backend)

	opts := QueryOptions{SkipInnerRerank: true, SkipVectorChannel: true}
	for i := 0; i < 5; i++ {
		engine.GatherSymbolCandidates("widget extensions", 5, opts, nil)
	}
	if backend.docCountCalls > 1 {
		t.Fatalf("corpus gate asked DocCount %d times across 5 gathers; a positive answer must be memoized", backend.docCountCalls)
	}
}

// TestGatherSymbolCandidates_ExplicitDeepLimitPiercesScopeCap: the
// scoped over-fetch cap (200) exists to bound implicit slack, not to
// flatten an explicit deep request — the handler's post-filter-wipeout
// escalation asks for limit 625 precisely to dig past the head, and a
// cap below the ask turns its deeper iterations into byte-identical
// re-queries.
func TestGatherSymbolCandidates_ExplicitDeepLimitPiercesScopeCap(t *testing.T) {
	g := graph.New()
	backend := &depthRecordingBackend{}
	engine := NewEngine(g)
	engine.SetSearch(backend)

	opts := QueryOptions{RepoAllow: map[string]bool{"app": true}, SkipInnerRerank: true, SkipVectorChannel: true}
	engine.GatherSymbolCandidates("widget extensions", 625, opts, nil)
	if backend.maxLimit < 625 {
		t.Fatalf("explicit limit 625 was clamped to %d — escalation depth flattened", backend.maxLimit)
	}

	backend.maxLimit = 0
	engine.GatherSymbolCandidates("widget extensions", 20, opts, nil)
	if backend.maxLimit > 200 {
		t.Fatalf("small-page scoped over-fetch must stay capped; backend saw %d", backend.maxLimit)
	}
}

type depthRecordingBackend struct {
	maxLimit int
}

func (b *depthRecordingBackend) Add(string, ...string) {}
func (b *depthRecordingBackend) Remove(string)         {}
func (b *depthRecordingBackend) Count() int            { return 1 }
func (b *depthRecordingBackend) Close()                {}
func (b *depthRecordingBackend) Search(_ string, limit int) []search.SearchResult {
	if limit > b.maxLimit {
		b.maxLimit = limit
	}
	return nil
}

// TestGatherSymbolCandidates_DocCountContractCorners: an
// empty-but-known store and an unknown count must both keep the
// fallback — only known-and-positive proves a corpus.
func TestGatherSymbolCandidates_DocCountContractCorners(t *testing.T) {
	g := graph.New()
	n := &graph.Node{ID: "app/w.go::WidgetExtensions", Name: "WidgetExtensions", Kind: graph.KindType, RepoPrefix: "app"}
	g.AddNode(n)
	opts := QueryOptions{SkipInnerRerank: true, SkipVectorChannel: true}

	for name, backend := range map[string]*warmStoreBackend{
		"empty-but-known": {hits: []search.SearchResult{{ID: n.ID, Score: 5}}, docs: 0},
		"unknown":         {hits: []search.SearchResult{{ID: n.ID, Score: 5}}, docs: -1},
	} {
		engine := NewEngine(g)
		if name == "unknown" {
			engine.SetSearch(&unknownCountBackend{warmStoreBackend: *backend})
		} else {
			engine.SetSearch(backend)
		}
		// Identifier query: the substring fallback CAN answer this, so
		// a result proves the fallback ran; the backend's two-word-only
		// hit list proves the backend did not.
		got := engine.GatherSymbolCandidates("WidgetExtensions", 5, opts, nil)
		if len(got) != 1 || got[0].Node.ID != n.ID {
			t.Fatalf("%s: fallback must still answer; got %#v", name, got)
		}
	}
}

type unknownCountBackend struct{ warmStoreBackend }

func (b *unknownCountBackend) DocCount() (int, bool) { return 12345, false }

// TestGatherSymbolCandidates_VectorOnlyHybridUsesBackend proves that the corpus
// gate recognizes a populated vector channel even when its text side is the
// deliberately empty NullBackend. The concept query shares no substring with
// the symbol name, so the candidate can only arrive through semantic search.
func TestGatherSymbolCandidates_VectorOnlyHybridUsesBackend(t *testing.T) {
	g := graph.New()
	n := &graph.Node{
		ID:         "app/snapshot.go::SnapshotCoordinator",
		Name:       "SnapshotCoordinator",
		Kind:       graph.KindType,
		RepoPrefix: "app",
	}
	g.AddNode(n)

	delegate := &fakeVectorSearcher{hits: []graph.VectorHit{{NodeID: n.ID}}}
	vector := search.NewDelegatedVector(2, delegate, 1, 0)
	backend := search.NewSwappable(search.NewNull())
	backend.ReplaceHybridVector(vector, &fakeEmbedder{queryVec: []float32{1, 0}})
	defer backend.Close()

	engine := NewEngine(g)
	engine.SetSearch(backend)
	got := engine.GatherSymbolCandidates(
		"durable state persistence",
		5,
		QueryOptions{SkipInnerRerank: true},
		nil,
	)
	if len(got) != 1 || got[0].Node.ID != n.ID {
		t.Fatalf("vector-only hybrid was bypassed (substring fallback?); got %#v", got)
	}
	if got[0].TextRank != -1 || got[0].VectorRank != 0 {
		t.Fatalf("vector-only hit must preserve channel ranks; got %#v", got[0])
	}
}

// TestGatherSymbolCandidates_EmptyBackendStillFallsBack: no delta
// count AND no doc count keeps the substring fallback for genuinely
// empty in-process backends.
func TestGatherSymbolCandidates_EmptyBackendStillFallsBack(t *testing.T) {
	g := graph.New()
	n := &graph.Node{ID: "app/w.go::WidgetExtensions", Name: "WidgetExtensions", Kind: graph.KindType, RepoPrefix: "app"}
	g.AddNode(n)
	engine := NewEngine(g)
	engine.SetSearch(search.NewNull()) // empty: Count 0, no corpus

	got := engine.GatherSymbolCandidates("WidgetExtensions", 5, QueryOptions{SkipInnerRerank: true, SkipVectorChannel: true}, nil)
	if len(got) != 1 || got[0].Node.ID != n.ID {
		t.Fatalf("substring fallback must still rescue empty backends; got %#v", got)
	}
}
