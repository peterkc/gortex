package indexer

import (
	"context"
	"fmt"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	"github.com/zzet/gortex/internal/config"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/parser"
	"github.com/zzet/gortex/internal/search"
)

// poolEmbedder is a fake API-style embedding provider for the pool
// tests. It reports Concurrent() == true so embedAllChunks picks the
// parallel path, records the peak number of EmbedBatch calls running
// at once, and embeds each text to a deterministic vector derived from
// the text so callers can assert results landed at the right index.
type poolEmbedder struct {
	mu       sync.Mutex
	inFlight int
	peak     int
	calls    int32
	// delay, when set, makes every EmbedBatch sleep so concurrent
	// calls genuinely overlap and the peak measurement is meaningful.
	delay time.Duration
	// failOnText, when non-empty, makes EmbedBatch return an error for
	// any batch containing that exact text — the error-injection hook.
	failOnText string
}

func (p *poolEmbedder) Concurrent() bool { return true }
func (p *poolEmbedder) Dimensions() int  { return 3 }
func (p *poolEmbedder) Close() error     { return nil }

func (p *poolEmbedder) Embed(ctx context.Context, text string) ([]float32, error) {
	out, err := p.EmbedBatch(ctx, []string{text})
	if err != nil {
		return nil, err
	}
	return out[0], nil
}

func (p *poolEmbedder) EmbedBatch(_ context.Context, texts []string) ([][]float32, error) {
	atomic.AddInt32(&p.calls, 1)
	p.mu.Lock()
	p.inFlight++
	if p.inFlight > p.peak {
		p.peak = p.inFlight
	}
	p.mu.Unlock()
	defer func() {
		p.mu.Lock()
		p.inFlight--
		p.mu.Unlock()
	}()

	if p.delay > 0 {
		time.Sleep(p.delay)
	}

	out := make([][]float32, len(texts))
	for i, txt := range texts {
		if p.failOnText != "" && txt == p.failOnText {
			return nil, fmt.Errorf("poolEmbedder: injected failure on %q", txt)
		}
		// Vector encodes the text's numeric suffix so a caller can
		// verify the result landed at the matching index.
		out[i] = []float32{textToScalar(txt), 0, 0}
	}
	return out, nil
}

// textToScalar maps "t<N>" to N as a float32 so each embedded text has
// a unique, position-revealing vector.
func textToScalar(txt string) float32 {
	if len(txt) > 1 && txt[0] == 't' {
		if n, err := strconv.Atoi(txt[1:]); err == nil {
			return float32(n)
		}
	}
	return -1
}

// newPoolTestIndexer builds a minimal Indexer wired with the given
// embedder and API concurrency cap, for direct embedAllChunks tests.
func newPoolTestIndexer(t *testing.T, emb interface {
	Embed(context.Context, string) ([]float32, error)
	EmbedBatch(context.Context, []string) ([][]float32, error)
	Dimensions() int
	Close() error
}, concurrency int) *Indexer {
	t.Helper()
	idx := New(graph.New(), parser.NewRegistry(), config.Default().Index, zap.NewNop())
	idx.SetEmbedder(emb)
	idx.SetEmbeddingAPIConcurrency(concurrency)
	return idx
}

// makeTexts builds n texts "t0".."t<n-1>".
func makeTexts(n int) []string {
	out := make([]string, n)
	for i := range out {
		out[i] = "t" + strconv.Itoa(i)
	}
	return out
}

// passthroughEmbedFn is the embedFn embedAllChunks expects — here it
// just forwards to the indexer's embedder with no retry wrapping, so
// the tests exercise the pool itself.
func passthroughEmbedFn(idx *Indexer) func(context.Context, []string) ([][]float32, error) {
	return func(ctx context.Context, items []string) ([][]float32, error) {
		return idx.embedder.EmbedBatch(ctx, items)
	}
}

// TestEmbedAllChunks_PoolRespectsConcurrencyCap asserts the worker
// pool never runs more EmbedBatch calls at once than the configured
// cap, embeds every chunk, and returns vectors in input order.
func TestEmbedAllChunks_PoolRespectsConcurrencyCap(t *testing.T) {
	emb := &poolEmbedder{delay: 15 * time.Millisecond}
	const cap = 3
	idx := newPoolTestIndexer(t, emb, cap)

	texts := makeTexts(60) // 60 texts, batch 5 → 12 batches > cap
	vectors, err := idx.embedAllChunks(context.Background(), texts, 5, passthroughEmbedFn(idx))
	require.NoError(t, err)

	require.Len(t, vectors, len(texts), "every text must be embedded")
	assert.LessOrEqual(t, emb.peak, cap,
		"peak in-flight EmbedBatch calls (%d) must not exceed the cap (%d)", emb.peak, cap)
	assert.Greater(t, emb.peak, 1, "the pool must actually run batches in parallel")

	// Order preserved: vectors[i] must encode texts[i].
	for i := range texts {
		require.Len(t, vectors[i], 3)
		assert.Equal(t, float32(i), vectors[i][0],
			"vector at index %d must correspond to text %q", i, texts[i])
	}
}

// TestEmbedAllChunks_AbortsOnError asserts that a single batch failure
// aborts the whole embed: embedAllChunks returns the error and no
// partial vector slice.
func TestEmbedAllChunks_AbortsOnError(t *testing.T) {
	emb := &poolEmbedder{delay: 5 * time.Millisecond, failOnText: "t37"}
	idx := newPoolTestIndexer(t, emb, 4)

	vectors, err := idx.embedAllChunks(context.Background(), makeTexts(80), 5, passthroughEmbedFn(idx))
	require.Error(t, err, "one failing chunk must fail the whole embed")
	assert.Nil(t, vectors, "a failed embed must return no partial result")
	assert.Contains(t, err.Error(), "t37")
}

// TestBuildSearchIndexCtx_AbortOnEmbedErrorKeepsTextOnly asserts the end-to-end
// abort contract: when embedding fails, vector publication leaves the search
// backend text-only — no HybridBackend is swapped in.
func TestBuildSearchIndexCtx_AbortOnEmbedErrorKeepsTextOnly(t *testing.T) {
	g := newVectorPlanTestStore()
	// Two function nodes; their embed metadata text is
	// "function <Name> ...". poolEmbedder fails on an exact text
	// match, so failing on a metadata string aborts the embed.
	g.AddNode(&graph.Node{ID: "a.go::Alpha", Name: "Alpha", Kind: graph.KindFunction, FilePath: "a.go", Language: "go"})
	g.AddNode(&graph.Node{ID: "a.go::Beta", Name: "Beta", Kind: graph.KindFunction, FilePath: "a.go", Language: "go"})

	idx := New(g, parser.NewRegistry(), config.Default().Index, zap.NewNop())
	emb := &poolEmbedder{failOnText: "function Alpha  a.go"}
	idx.SetEmbedder(emb)

	require.NoError(t, idx.buildSearchIndexCtx(context.Background()))
	require.ErrorContains(t, idx.LastVectorBuildError(), "poolEmbedder: injected failure")
	require.Equal(t, int32(1), emb.calls)
	require.Zero(t, g.replaceCalls)

	// The backend must NOT be a HybridBackend — embedding aborted, so
	// the search stays text-only.
	sw, ok := idx.Search().(*search.Swappable)
	require.True(t, ok)
	backend, release := sw.AcquireBackend()
	defer release()
	_, isHybrid := backend.(*search.HybridBackend)
	assert.False(t, isHybrid,
		"vector publication must not install a HybridBackend when embedding fails")
}

// TestEmbedAllChunks_DeterministicRegardlessOfOrder runs the pool many
// times with overlapping workers and asserts the vectors always land
// at the input-aligned indices — completion order must never reshuffle
// the result.
func TestEmbedAllChunks_DeterministicRegardlessOfOrder(t *testing.T) {
	for trial := 0; trial < 8; trial++ {
		emb := &poolEmbedder{delay: time.Millisecond}
		idx := newPoolTestIndexer(t, emb, 6)

		texts := makeTexts(50)
		vectors, err := idx.embedAllChunks(context.Background(), texts, 3, passthroughEmbedFn(idx))
		require.NoError(t, err)
		require.Len(t, vectors, len(texts))
		for i := range texts {
			assert.Equal(t, float32(i), vectors[i][0],
				"trial %d: index %d must hold the vector for %q regardless of completion order",
				trial, i, texts[i])
		}
	}
}

// TestEmbedAllChunks_SerialForNonConcurrentEmbedder asserts an
// embedder that does not implement Concurrent() is run serially — the
// peak in-flight count stays at 1.
func TestEmbedAllChunks_SerialForNonConcurrentEmbedder(t *testing.T) {
	emb := &serialOnlyEmbedder{delay: 5 * time.Millisecond}
	idx := New(graph.New(), parser.NewRegistry(), config.Default().Index, zap.NewNop())
	idx.SetEmbedder(emb)
	idx.SetEmbeddingAPIConcurrency(8)

	vectors, err := idx.embedAllChunks(context.Background(), makeTexts(30), 3, passthroughEmbedFn(idx))
	require.NoError(t, err)
	require.Len(t, vectors, 30)
	assert.Equal(t, 1, emb.peak,
		"an embedder without Concurrent() must be driven serially (peak in-flight 1)")
}

func TestEmbedAllChunks_SerialHonorsParentCancellation(t *testing.T) {
	emb := &serialOnlyEmbedder{}
	idx := newPoolTestIndexer(t, emb, 8)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	vectors, err := idx.embedAllChunks(ctx, makeTexts(3), 1, passthroughEmbedFn(idx))
	require.ErrorIs(t, err, context.Canceled)
	assert.Nil(t, vectors)
	assert.Equal(t, 0, emb.peak, "a cancelled owner must not start serial embedding")
}

func TestEmbedAllChunks_ParallelHonorsCancellationWhenProviderReturnsLate(t *testing.T) {
	idx := newPoolTestIndexer(t, &poolEmbedder{}, 2)
	ctx, cancel := context.WithCancel(context.Background())
	started := make(chan struct{})
	release := make(chan struct{})
	var startedOnce sync.Once

	done := make(chan struct {
		vectors [][]float32
		err     error
	}, 1)
	go func() {
		vectors, err := idx.embedAllChunks(ctx, makeTexts(4), 1, func(_ context.Context, items []string) ([][]float32, error) {
			startedOnce.Do(func() { close(started) })
			<-release // Model a provider that ignores cancellation and returns late.
			vectors := make([][]float32, len(items))
			for i := range vectors {
				vectors[i] = []float32{1, 0, 0}
			}
			return vectors, nil
		})
		done <- struct {
			vectors [][]float32
			err     error
		}{vectors: vectors, err: err}
	}()

	select {
	case <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("parallel embedding did not start")
	}
	cancel()
	close(release)

	select {
	case result := <-done:
		require.ErrorIs(t, result.err, context.Canceled)
		assert.Nil(t, result.vectors, "cancelled embedding must not return late vectors")
	case <-time.After(5 * time.Second):
		t.Fatal("parallel embedding did not stop after cancellation")
	}
}

// TestEmbedAllChunks_ConcurrencyCapIsBinding proves the configured cap
// is the *binding* constraint, not merely an upper bound that the
// workload happens to stay under. A barrier embedder blocks each call
// until exactly `cap` calls are simultaneously in flight, then releases
// them; if the pool ran fewer than `cap` workers the barrier would
// deadlock (the test would time out), and if it ran more the peak would
// exceed the cap. Reaching the barrier and observing peak == cap proves
// the pool saturates to precisely the configured width.
func TestEmbedAllChunks_ConcurrencyCapIsBinding(t *testing.T) {
	const cap = 3
	emb := &barrierEmbedder{target: cap, reached: make(chan struct{})}
	idx := newPoolTestIndexer(t, emb, cap)

	// Enough batches that the pool must reuse workers — more batches
	// than the cap guarantees the barrier is hit by the first wave.
	texts := makeTexts(30) // batch 2 → 15 batches
	done := make(chan error, 1)
	go func() {
		_, err := idx.embedAllChunks(context.Background(), texts, 2, passthroughEmbedFn(idx))
		done <- err
	}()

	select {
	case <-emb.reached:
		// The barrier opened only because `cap` calls were in flight at
		// once — the pool genuinely ran `cap` workers in parallel.
	case <-time.After(5 * time.Second):
		t.Fatal("pool never reached the concurrency cap — fewer than cap workers ran (deadlock)")
	}

	select {
	case err := <-done:
		require.NoError(t, err)
	case <-time.After(5 * time.Second):
		t.Fatal("embedAllChunks did not finish after the barrier opened")
	}

	if got := emb.peakInFlight(); got != cap {
		t.Fatalf("peak in-flight = %d, want exactly the cap %d", got, cap)
	}
}

// TestEmbedAllChunks_ZeroCapUsesDefault asserts that a zero configured
// cap falls back to the built-in default — proving the
// SetEmbeddingAPIConcurrency(0) path resolves to defaultEmbedAPIConcurrency
// rather than serializing or panicking.
func TestEmbedAllChunks_ZeroCapUsesDefault(t *testing.T) {
	emb := &poolEmbedder{delay: 10 * time.Millisecond}
	idx := newPoolTestIndexer(t, emb, 0) // 0 → default (4)

	// More batches than the default so the pool can saturate it.
	texts := makeTexts(40) // batch 2 → 20 batches
	vectors, err := idx.embedAllChunks(context.Background(), texts, 2, passthroughEmbedFn(idx))
	require.NoError(t, err)
	require.Len(t, vectors, len(texts))

	assert.LessOrEqual(t, emb.peak, defaultEmbedAPIConcurrency,
		"zero cap must fall back to the default (%d), peak was %d", defaultEmbedAPIConcurrency, emb.peak)
	assert.Greater(t, emb.peak, 1,
		"zero cap must still parallelize via the default, not serialize")
}

// TestSetEmbeddingAPIConcurrency_FlowsToPool asserts the setter wires
// the field the pool actually reads — the production path is
// cfg.Embedding.APIConcurrency → SetEmbeddingAPIConcurrency → the pool
// cap. A cap of 1 must force the serial-equivalent path (peak 1) even
// for a Concurrent() embedder, proving the setter, not the embedder,
// decides the width.
func TestSetEmbeddingAPIConcurrency_FlowsToPool(t *testing.T) {
	emb := &poolEmbedder{delay: 5 * time.Millisecond}
	idx := newPoolTestIndexer(t, emb, 1) // cap 1 → no overlap

	vectors, err := idx.embedAllChunks(context.Background(), makeTexts(20), 2, passthroughEmbedFn(idx))
	require.NoError(t, err)
	require.Len(t, vectors, 20)
	assert.Equal(t, 1, emb.peak,
		"a cap of 1 must serialize even a Concurrent() embedder (peak in-flight 1)")
}

// barrierEmbedder is a Concurrent() embedder that blocks every call on a
// shared barrier until `target` calls are simultaneously in flight, then
// releases them all. It is the deterministic way to prove the pool runs
// exactly `target` workers at once: if fewer run, the barrier never
// opens and the test deadlocks.
type barrierEmbedder struct {
	target    int
	mu        sync.Mutex
	inFlight  int
	peak      int
	released  bool
	reached   chan struct{}
	closeOnce sync.Once
}

func (b *barrierEmbedder) Concurrent() bool { return true }
func (b *barrierEmbedder) Dimensions() int  { return 3 }
func (b *barrierEmbedder) Close() error     { return nil }

func (b *barrierEmbedder) peakInFlight() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.peak
}

func (b *barrierEmbedder) Embed(ctx context.Context, text string) ([]float32, error) {
	out, err := b.EmbedBatch(ctx, []string{text})
	if err != nil {
		return nil, err
	}
	return out[0], nil
}

func (b *barrierEmbedder) EmbedBatch(_ context.Context, texts []string) ([][]float32, error) {
	b.mu.Lock()
	b.inFlight++
	if b.inFlight > b.peak {
		b.peak = b.inFlight
	}
	if b.inFlight >= b.target {
		b.released = true
		b.closeOnce.Do(func() { close(b.reached) })
	}
	b.mu.Unlock()

	// Spin until the barrier has opened so the cap-many callers overlap.
	for {
		b.mu.Lock()
		released := b.released
		b.mu.Unlock()
		if released {
			break
		}
		time.Sleep(time.Millisecond)
	}

	b.mu.Lock()
	b.inFlight--
	b.mu.Unlock()

	out := make([][]float32, len(texts))
	for i, txt := range texts {
		out[i] = []float32{textToScalar(txt), 0, 0}
	}
	return out, nil
}

// serialOnlyEmbedder is a fake embedder that does NOT implement
// Concurrent(), modelling an in-process transformer backend.
type serialOnlyEmbedder struct {
	mu       sync.Mutex
	inFlight int
	peak     int
	delay    time.Duration
}

func (e *serialOnlyEmbedder) Embed(ctx context.Context, text string) ([]float32, error) {
	out, err := e.EmbedBatch(ctx, []string{text})
	if err != nil {
		return nil, err
	}
	return out[0], nil
}

func (e *serialOnlyEmbedder) EmbedBatch(_ context.Context, texts []string) ([][]float32, error) {
	e.mu.Lock()
	e.inFlight++
	if e.inFlight > e.peak {
		e.peak = e.inFlight
	}
	e.mu.Unlock()
	defer func() {
		e.mu.Lock()
		e.inFlight--
		e.mu.Unlock()
	}()
	if e.delay > 0 {
		time.Sleep(e.delay)
	}
	out := make([][]float32, len(texts))
	for i := range texts {
		out[i] = []float32{1, 0, 0}
	}
	return out, nil
}

func (e *serialOnlyEmbedder) Dimensions() int { return 3 }
func (e *serialOnlyEmbedder) Close() error    { return nil }
