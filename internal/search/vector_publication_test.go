package search

import (
	"context"
	"reflect"
	"sync"
	"testing"
	"time"

	"github.com/zzet/gortex/internal/graph"
)

type publicationTestDelegate struct {
	mu     sync.Mutex
	hits   []graph.VectorHit
	limits []int
}

func (d *publicationTestDelegate) SimilarTo(_ []float32, limit int) ([]graph.VectorHit, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.limits = append(d.limits, limit)
	if limit < len(d.hits) {
		return append([]graph.VectorHit(nil), d.hits[:limit]...), nil
	}
	return append([]graph.VectorHit(nil), d.hits...), nil
}

type publicationTestEmbedder struct {
	started chan struct{}
	release chan struct{}
	once    sync.Once
}

func (e *publicationTestEmbedder) Embed(_ context.Context, _ string) ([]float32, error) {
	if e.started != nil {
		e.once.Do(func() { close(e.started) })
		<-e.release
	}
	return []float32{1, 0}, nil
}

func (e *publicationTestEmbedder) EmbedBatch(_ context.Context, texts []string) ([][]float32, error) {
	vectors := make([][]float32, len(texts))
	for i := range vectors {
		vectors[i] = []float32{1, 0}
	}
	return vectors, nil
}

func (*publicationTestEmbedder) Dimensions() int { return 2 }
func (*publicationTestEmbedder) Close() error    { return nil }

type publicationTestTextBackend struct {
	mu         sync.Mutex
	closeCount int
}

func (*publicationTestTextBackend) Add(string, ...string)             {}
func (*publicationTestTextBackend) Remove(string)                     {}
func (*publicationTestTextBackend) Search(string, int) []SearchResult { return nil }
func (*publicationTestTextBackend) Count() int                        { return 1 }

func (b *publicationTestTextBackend) Close() {
	b.mu.Lock()
	b.closeCount++
	b.mu.Unlock()
}

func (b *publicationTestTextBackend) closes() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.closeCount
}

func TestNewDelegatedVectorHasNoHeapIndexAndUsesDurableStats(t *testing.T) {
	delegate := &publicationTestDelegate{hits: []graph.VectorHit{
		{NodeID: "symbol-a#chunk0", ParentID: "symbol-a"},
		{NodeID: "symbol-a#chunk1", ParentID: "symbol-a"},
		{NodeID: "symbol-b"},
	}}
	backend := NewDelegatedVector(384, delegate, 17, 2)

	if got := backend.Count(); got != 17 {
		t.Fatalf("Count() = %d, want complete durable count 17", got)
	}
	if !backend.HasChunks() {
		t.Fatal("HasChunks() = false, want durable chunk metadata")
	}
	if got := backend.SizeBytes(); got != 0 {
		t.Fatalf("SizeBytes() = %d, want zero process-local vector bytes", got)
	}
	if got, want := backend.Search([]float32{1, 0}, 8), []string{"symbol-a", "symbol-b"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("Search() = %v, want parent-deduplicated %v", got, want)
	}
}

func TestDelegatedVectorSearchOverfetchesForUniqueParents(t *testing.T) {
	delegate := &publicationTestDelegate{hits: []graph.VectorHit{
		{NodeID: "symbol-a#chunk0", ParentID: "symbol-a"},
		{NodeID: "symbol-a#chunk1", ParentID: "symbol-a"},
		{NodeID: "symbol-b"},
		{NodeID: "symbol-c"},
	}}
	backend := NewDelegatedVector(2, delegate, 4, 2)

	if got, want := backend.Search([]float32{1, 0}, 3), []string{"symbol-a", "symbol-b", "symbol-c"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("Search() = %v, want k unique parent IDs %v", got, want)
	}

	delegate.mu.Lock()
	limits := append([]int(nil), delegate.limits...)
	delegate.mu.Unlock()
	if got, want := limits, []int{4}; !reflect.DeepEqual(got, want) {
		t.Fatalf("delegate limits = %v, want min(delegateCount, k+chunkCount) = %v", got, want)
	}
}

func TestDelegatedVectorSearchRejectsNonPositiveLimitWithoutDelegateCall(t *testing.T) {
	delegate := &publicationTestDelegate{hits: []graph.VectorHit{{NodeID: "symbol-a"}}}
	backend := NewDelegatedVector(2, delegate, 1, 0)

	for _, k := range []int{0, -1} {
		if got := backend.Search([]float32{1, 0}, k); got != nil {
			t.Fatalf("Search(k=%d) = %v, want nil", k, got)
		}
	}
	delegate.mu.Lock()
	limits := append([]int(nil), delegate.limits...)
	delegate.mu.Unlock()
	if len(limits) != 0 {
		t.Fatalf("delegate called with limits %v for non-positive k", limits)
	}
}

func TestSerializeVectorUpdateSerializesConcurrentCallbacks(t *testing.T) {
	swappable := NewSwappable(&publicationTestTextBackend{})
	firstStarted := make(chan struct{})
	releaseFirst := make(chan struct{})
	firstDone := make(chan struct{})
	go func() {
		_ = swappable.SerializeVectorUpdate(func() error {
			close(firstStarted)
			<-releaseFirst
			return nil
		})
		close(firstDone)
	}()
	awaitPublicationSignal(t, "first callback start", firstStarted)

	secondAttempted := make(chan struct{})
	secondStarted := make(chan struct{})
	secondDone := make(chan struct{})
	go func() {
		close(secondAttempted)
		_ = swappable.SerializeVectorUpdate(func() error {
			close(secondStarted)
			return nil
		})
		close(secondDone)
	}()
	awaitPublicationSignal(t, "second callback attempt", secondAttempted)

	select {
	case <-secondStarted:
		t.Fatal("second vector-update callback started while the first was active")
	case <-time.After(50 * time.Millisecond):
	}

	close(releaseFirst)
	awaitPublicationSignal(t, "first callback completion", firstDone)
	awaitPublicationSignal(t, "second callback start", secondStarted)
	awaitPublicationSignal(t, "second callback completion", secondDone)
	swappable.Close()
}

func TestCloseWaitsForActiveVectorUpdate(t *testing.T) {
	text := &publicationTestTextBackend{}
	swappable := NewSwappable(text)
	updateStarted := make(chan struct{})
	releaseUpdate := make(chan struct{})
	updateDone := make(chan struct{})
	go func() {
		_ = swappable.SerializeVectorUpdate(func() error {
			close(updateStarted)
			<-releaseUpdate
			return nil
		})
		close(updateDone)
	}()
	awaitPublicationSignal(t, "vector-update callback start", updateStarted)

	closeAttempted := make(chan struct{})
	closeDone := make(chan struct{})
	go func() {
		close(closeAttempted)
		swappable.Close()
		close(closeDone)
	}()
	awaitPublicationSignal(t, "Close attempt", closeAttempted)

	select {
	case <-closeDone:
		t.Fatal("Close completed while a vector-update callback was active")
	case <-time.After(50 * time.Millisecond):
	}
	if got := text.closes(); got != 0 {
		t.Fatalf("backend close count = %d while vector update active, want 0", got)
	}

	close(releaseUpdate)
	awaitPublicationSignal(t, "vector-update callback completion", updateDone)
	awaitPublicationSignal(t, "Close completion", closeDone)
	if got := text.closes(); got != 1 {
		t.Fatalf("backend close count = %d after vector update drained, want 1", got)
	}
}

func TestAcquireBackendPinsReplacementUntilRelease(t *testing.T) {
	text := &publicationTestTextBackend{}
	embedder := &publicationTestEmbedder{}
	oldVector := NewDelegatedVector(2, &publicationTestDelegate{hits: []graph.VectorHit{{NodeID: "old-symbol"}}}, 1, 0)
	swappable := NewSwappable(NewHybrid(text, oldVector, embedder))

	pinned, release := swappable.AcquireBackend()
	if _, ok := pinned.(*HybridBackend); !ok {
		t.Fatalf("AcquireBackend() returned %T, want *HybridBackend", pinned)
	}

	newVector := NewDelegatedVector(2, &publicationTestDelegate{}, 1, 0)
	replaceStarted := make(chan struct{})
	replaceDone := make(chan struct{})
	go func() {
		close(replaceStarted)
		swappable.ReplaceHybridVector(newVector, embedder)
		close(replaceDone)
	}()
	<-replaceStarted

	select {
	case <-replaceDone:
		t.Fatal("replacement completed while AcquireBackend pin was held")
	case <-time.After(50 * time.Millisecond):
	}
	if oldVector.Count() != 1 {
		t.Fatal("pinned backend was retired before release")
	}

	release()
	release() // The release contract is intentionally idempotent.
	select {
	case <-replaceDone:
	case <-time.After(2 * time.Second):
		t.Fatal("replacement did not complete after AcquireBackend release")
	}
	if got := oldVector.Count(); got != 0 {
		t.Fatalf("retired vector still reports %d rows after release", got)
	}

	swappable.Close()
	if got := text.closes(); got != 1 {
		t.Fatalf("text backend close count = %d, want 1", got)
	}
}

func TestReplaceHybridVectorWaitsForReadersAndTransfersTextOwnership(t *testing.T) {
	text := &publicationTestTextBackend{}
	oldVector := NewDelegatedVector(2, &publicationTestDelegate{hits: []graph.VectorHit{{NodeID: "old-symbol"}}}, 1, 0)
	oldEmbedder := &publicationTestEmbedder{
		started: make(chan struct{}),
		release: make(chan struct{}),
	}
	swappable := NewSwappable(NewHybrid(text, oldVector, oldEmbedder))

	oldResults := make(chan []string, 1)
	go func() {
		ids, _ := swappable.VectorChannelOnly("old", 1)
		oldResults <- ids
	}()
	<-oldEmbedder.started

	newDelegate := &publicationTestDelegate{hits: []graph.VectorHit{{
		NodeID:   "new-symbol#chunk0",
		ParentID: "new-symbol",
	}}}
	newVector := NewDelegatedVector(2, newDelegate, 1, 1)
	newEmbedder := &publicationTestEmbedder{}
	replaceStarted := make(chan struct{})
	replaceDone := make(chan struct{})
	go func() {
		close(replaceStarted)
		swappable.ReplaceHybridVector(newVector, newEmbedder)
		close(replaceDone)
	}()
	<-replaceStarted

	select {
	case <-replaceDone:
		t.Fatal("replacement completed while an old-backend reader was active")
	case <-time.After(50 * time.Millisecond):
	}
	if oldVector.Count() != 1 {
		t.Fatal("old vector was retired before its active reader completed")
	}

	close(oldEmbedder.release)
	if got, want := <-oldResults, []string{"old-symbol"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("in-flight reader saw %v, want complete old result %v", got, want)
	}
	select {
	case <-replaceDone:
	case <-time.After(2 * time.Second):
		t.Fatal("replacement did not complete after the old reader drained")
	}

	if got := oldVector.Count(); got != 0 {
		t.Fatalf("retired vector still reports %d rows, want zero", got)
	}
	if got := text.closes(); got != 0 {
		t.Fatalf("transferred text backend closed %d times during replacement", got)
	}
	if got, want := vectorIDs(swappable, "new"), []string{"new-symbol"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("new reader saw %v, want complete new result %v", got, want)
	}

	func() {
		backend, release := swappable.AcquireBackend()
		defer release()
		current, ok := backend.(*HybridBackend)
		if !ok {
			t.Fatalf("active backend is %T, want *HybridBackend", backend)
		}
		if current.TextBackend() != text {
			t.Fatal("replacement did not retain the original text backend")
		}
		if _, nested := current.TextBackend().(*HybridBackend); nested {
			t.Fatal("replacement nested a HybridBackend inside another HybridBackend")
		}
	}()

	swappable.Close()
	if got := text.closes(); got != 1 {
		t.Fatalf("text backend close count = %d, want exactly one final close", got)
	}
	if got := newVector.Count(); got != 0 {
		t.Fatalf("final vector Count() = %d after close, want 0", got)
	}
}

func TestReplaceHybridVectorFlattensAndSupportsRepeatedReplacement(t *testing.T) {
	text := &publicationTestTextBackend{}
	embedder := &publicationTestEmbedder{}
	first := NewDelegatedVector(2, &publicationTestDelegate{}, 1, 0)
	second := NewDelegatedVector(2, &publicationTestDelegate{}, 2, 0)
	legacyNested := NewHybrid(NewHybrid(text, first, embedder), second, embedder)
	swappable := NewSwappable(legacyNested)

	third := NewDelegatedVector(2, &publicationTestDelegate{}, 3, 0)
	swappable.ReplaceHybridVector(third, embedder)
	assertSingleHybrid(t, swappable, text)
	if first.Count() != 0 || second.Count() != 0 {
		t.Fatalf("flattening did not retire both legacy vectors: first=%d second=%d", first.Count(), second.Count())
	}

	fourth := NewDelegatedVector(2, &publicationTestDelegate{}, 4, 1)
	swappable.ReplaceHybridVector(fourth, embedder)
	assertSingleHybrid(t, swappable, text)
	if got := third.Count(); got != 0 {
		t.Fatalf("repeated replacement retained prior delegated vector count %d", got)
	}
	if got := text.closes(); got != 0 {
		t.Fatalf("text backend closed %d times before final Swappable.Close", got)
	}

	swappable.Close()
	if got := text.closes(); got != 1 {
		t.Fatalf("text backend close count = %d, want 1", got)
	}
	if got := fourth.Count(); got != 0 {
		t.Fatalf("last vector Count() = %d after close, want 0", got)
	}
}

func awaitPublicationSignal(t *testing.T, name string, signal <-chan struct{}) {
	t.Helper()
	select {
	case <-signal:
	case <-time.After(2 * time.Second):
		t.Fatalf("timed out waiting for %s", name)
	}
}

func vectorIDs(swappable *Swappable, query string) []string {
	ids, _ := swappable.VectorChannelOnly(query, 1)
	return ids
}

func assertSingleHybrid(t *testing.T, swappable *Swappable, wantText Backend) {
	t.Helper()
	backend, release := swappable.AcquireBackend()
	defer release()
	hybrid, ok := backend.(*HybridBackend)
	if !ok {
		t.Fatalf("active backend is %T, want *HybridBackend", backend)
	}
	if hybrid.TextBackend() != wantText {
		t.Fatalf("hybrid text backend is %T, want original %T", hybrid.TextBackend(), wantText)
	}
	if _, nested := hybrid.TextBackend().(*HybridBackend); nested {
		t.Fatal("active HybridBackend contains a nested HybridBackend")
	}
}
