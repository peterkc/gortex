package lsp

import (
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
)

// newTestProvider builds a Provider that has never dialled anything —
// enough to be handed around as a pool/router entry without spawning a
// language server. Close on such a provider is a no-op (nil client), so
// tests can "reap" one the way the router would.
func newTestProvider() *Provider {
	return NewProvider("noop-cmd", nil, []string{"typescript"}, false, 1, zap.NewNop())
}

// TestResolverHelper_RouterBacked_LooksUpEveryAcquire is the core
// regression for the orphaned-LSP-process bug: the router-backed
// helper must consult its lookup closure on EVERY call instead of
// caching the provider it got the first time.
//
// The cached-pointer version let Router.Reap / maybeEvictLRULocked
// close a provider and delete it from the router map while the helper
// kept using it — Provider.EnsureClient then re-spawned the subprocess
// on an object the router no longer tracked, and nothing could ever
// reap the result. Re-asking the router per call also refreshes its
// lastUsed, which is what stops the reaper from taking a provider out
// from under an in-flight resolve pass in the first place.
func TestResolverHelper_RouterBacked_LooksUpEveryAcquire(t *testing.T) {
	var calls int
	provider := newTestProvider()
	h := NewLazyResolverHelper(
		func() (*Provider, error) {
			calls++
			return provider, nil
		},
		t.TempDir(),
		[]string{".ts"},
		time.Second,
		zap.NewNop(),
	)

	const acquisitions = 5
	for i := 0; i < acquisitions; i++ {
		got, release, ok := h.acquire("src/foo.ts")
		require.True(t, ok, "acquire %d must succeed", i)
		assert.Same(t, provider, got)
		release()
	}
	assert.Equal(t, acquisitions, calls,
		"router-backed helper must run the router lookup once per acquisition")
}

// TestResolverHelper_RouterBacked_ReplacesReapedProvider — once the
// router has reaped provider A (closed it and dropped it from its
// map), the next acquisition must return the router's NEW provider B.
// A resurrected A is exactly the orphan: a live subprocess the router
// does not know about.
func TestResolverHelper_RouterBacked_ReplacesReapedProvider(t *testing.T) {
	providerA := newTestProvider()
	providerB := newTestProvider()

	var reaped bool
	h := NewLazyResolverHelper(
		func() (*Provider, error) {
			if reaped {
				return providerB, nil
			}
			return providerA, nil
		},
		t.TempDir(),
		[]string{".ts"},
		time.Second,
		zap.NewNop(),
	)

	got, release, ok := h.acquire("src/foo.ts")
	require.True(t, ok)
	assert.Same(t, providerA, got, "first acquisition uses the router's current provider")
	release()

	// Simulate the router's idle reaper: close the provider and swap
	// in a fresh, router-tracked replacement for the next lookup.
	require.NoError(t, providerA.Close())
	reaped = true

	got, release, ok = h.acquire("src/foo.ts")
	require.True(t, ok)
	assert.Same(t, providerB, got, "a reaped provider must be replaced, never reused")
	assert.NotSame(t, providerA, got)
	release()
}

// TestResolverHelper_RouterBacked_LookupFailureIsNotSticky — a lookup
// that fails must not poison the helper. The router's markSpawnFailed
// already short-circuits a broken server, so a helper that latched the
// first error would simply stay blind for the daemon's lifetime.
func TestResolverHelper_RouterBacked_LookupFailureIsNotSticky(t *testing.T) {
	provider := newTestProvider()
	fail := true
	h := NewLazyResolverHelper(
		func() (*Provider, error) {
			if fail {
				return nil, errors.New("router evicted mid-spawn")
			}
			return provider, nil
		},
		t.TempDir(),
		[]string{".ts"},
		time.Second,
		zap.NewNop(),
	)

	if _, _, ok := h.acquire("src/foo.ts"); ok {
		t.Fatal("acquire must fail while the lookup errors")
	}

	fail = false
	got, release, ok := h.acquire("src/foo.ts")
	require.True(t, ok, "a later acquisition must retry the lookup")
	assert.Same(t, provider, got)
	release()
}

// TestResolverHelper_RouterBacked_AcquireSerialises — the router hands
// every caller the same *Provider and a provider's stdio is
// single-threaded, so acquire must serialise router-backed calls the
// way the old borrow channel did. Run under -race this also proves the
// per-call lookup itself is not a data race.
func TestResolverHelper_RouterBacked_AcquireSerialises(t *testing.T) {
	provider := newTestProvider()
	var (
		mu       sync.Mutex
		inFlight int
		maxSeen  int
		lookups  int
	)
	h := NewLazyResolverHelper(
		func() (*Provider, error) {
			mu.Lock()
			lookups++
			mu.Unlock()
			return provider, nil
		},
		t.TempDir(),
		[]string{".ts"},
		time.Second,
		zap.NewNop(),
	)

	const goroutines = 8
	const perGoroutine = 25
	var wg sync.WaitGroup
	for i := 0; i < goroutines; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < perGoroutine; j++ {
				_, release, ok := h.acquire("src/foo.ts")
				if !ok {
					t.Error("acquire failed unexpectedly")
					return
				}
				mu.Lock()
				inFlight++
				if inFlight > maxSeen {
					maxSeen = inFlight
				}
				mu.Unlock()

				mu.Lock()
				inFlight--
				mu.Unlock()
				release()
			}
		}()
	}
	wg.Wait()

	assert.Equal(t, 1, maxSeen, "router-backed acquisitions must not overlap")
	assert.Equal(t, goroutines*perGoroutine, lookups,
		"every acquisition must reach the router")
}

// TestResolverHelper_RouterBacked_CloseIsNoOp — the router owns the
// provider, sharing it with enrichment and with every other helper for
// the same (spec, workspace). A helper that closed it on shutdown
// would kill a subprocess the router still lists as alive.
func TestResolverHelper_RouterBacked_CloseIsNoOp(t *testing.T) {
	provider := newTestProvider()
	h := NewLazyResolverHelper(
		func() (*Provider, error) { return provider, nil },
		t.TempDir(),
		[]string{".ts"},
		time.Second,
		zap.NewNop(),
	)

	_, release, ok := h.acquire("src/foo.ts")
	require.True(t, ok)
	release()

	require.NoError(t, h.Close())
	assert.Empty(t, h.providers, "a router-backed helper must never adopt providers")

	// Still usable after Close — nothing was torn down.
	got, release, ok := h.acquire("src/foo.ts")
	require.True(t, ok)
	assert.Same(t, provider, got)
	release()
}

// TestResolverHelper_OwnedPool_SpawnsOnceAndBorrows — the opt-in
// pooled flavour (GORTEX_LSP_POOL_SIZE > 1) keeps its original
// contract: it owns its providers, so it spawns poolSize of them once
// and then borrows from the channel without re-invoking spawn.
func TestResolverHelper_OwnedPool_SpawnsOnceAndBorrows(t *testing.T) {
	const poolSize = 3
	var spawns int
	h := NewPooledResolverHelper(
		func() (*Provider, error) {
			spawns++
			return newTestProvider(), nil
		},
		t.TempDir(),
		[]string{".ts"},
		time.Second,
		poolSize,
		zap.NewNop(),
	)

	for i := 0; i < 10; i++ {
		_, release, ok := h.acquire("src/foo.ts")
		require.True(t, ok)
		release()
	}
	assert.Equal(t, poolSize, spawns, "owned pool spawns exactly poolSize providers")
	assert.Len(t, h.providers, poolSize, "owned pool must retain its providers for Close")
	require.NoError(t, h.Close())
}

// TestResolverHelper_OwnedPool_SpawnFailureIsSticky — an owned pool
// that cannot spawn stays poisoned, because retrying would spawn a
// fresh unreaped subprocess on every resolve-time call. Only the
// router-backed flavour retries, and only because the router itself
// bounds the retry.
func TestResolverHelper_OwnedPool_SpawnFailureIsSticky(t *testing.T) {
	var spawns int
	h := NewPooledResolverHelper(
		func() (*Provider, error) {
			spawns++
			return nil, errors.New("tsserver missing")
		},
		t.TempDir(),
		[]string{".ts"},
		time.Second,
		2,
		zap.NewNop(),
	)

	for i := 0; i < 3; i++ {
		if _, _, ok := h.acquire("src/foo.ts"); ok {
			t.Fatal("acquire must fail when the pool cannot spawn")
		}
	}
	assert.Equal(t, 1, spawns, "a poisoned owned pool must not retry the spawn")
}

// Given a typed router acquisition, checkout cleanup must respect its pin.
func TestRouterPinnedWorkspaceProviderSurvivesCheckoutClose(t *testing.T) {
	root := t.TempDir()
	r := NewRouter(root, zap.NewNop())
	t.Cleanup(func() { require.NoError(t, r.Close()) })
	spec := &ServerSpec{Name: "pin-fixture", Languages: []string{"go"}}
	r.RegisterSpec(spec)
	injectProvider(r, spec.Name, root, time.Now(), 0)
	r.avail[spec.Name] = true
	p, release, err := r.AcquireSpecWorkspace(spec, root)
	require.NoError(t, err)
	// When cleanup races this use, it must keep the same usable provider.
	assert.Equal(t, 1, r.Stats()[0].InUse)
	assert.Zero(t, r.CloseCheckoutWorkspace("go", root))
	assert.False(t, p.closed)
	other, releaseOther, err := r.AcquireSpecWorkspace(spec, root)
	require.NoError(t, err)
	require.Same(t, p, other)
	release()
	release()
	assert.Equal(t, 1, r.Stats()[0].InUse, "repeated release cannot consume another caller's pin")
	assert.Zero(t, r.CloseCheckoutWorkspace("go", root))
	releaseOther()
	assert.Equal(t, 1, r.CloseCheckoutWorkspace("go", root))
	assert.True(t, p.closed)
	require.NoError(t, r.Close())
	_, releaseClosed, err := r.AcquireSpecWorkspace(spec, root)
	require.Error(t, err)
	assert.Nil(t, releaseClosed)
}

func TestLazyResolverHelperHoldsRouterPinDuringCall(t *testing.T) {
	for _, fail := range []bool{false, true} {
		t.Run(fmt.Sprint(fail), func(t *testing.T) {
			// Given a router-backed helper and an existing provider.
			root := t.TempDir()
			r := NewRouter(root, zap.NewNop())
			t.Cleanup(func() { require.NoError(t, r.Close()) })
			spec := &ServerSpec{Name: "helper-pin-fixture", Languages: []string{"go"}}
			r.RegisterSpec(spec)
			injectProvider(r, spec.Name, root, time.Now(), 0)
			r.avail[spec.Name] = true
			h := NewPinnedLazyResolverHelper(func() (*Provider, func(), error) {
				if fail {
					return nil, nil, errors.New("lookup failed")
				}
				return r.AcquireSpecWorkspace(spec, root)
			}, root, []string{".ts"}, time.Second, zap.NewNop())
			p, release, ok := h.acquire("main.ts")
			if fail {
				require.False(t, ok)
				assert.Zero(t, r.Stats()[0].InUse)
				return
			}
			require.True(t, ok)
			// When cleanup runs during this call, the provider stays open.
			assert.Equal(t, 1, r.Stats()[0].InUse)
			assert.Zero(t, r.CloseCheckoutWorkspace("go", root))
			assert.False(t, p.closed)
			release()
			if stats := r.Stats(); assert.Len(t, stats, 1) {
				assert.Zero(t, stats[0].InUse)
			}
			assert.Equal(t, 1, r.CloseCheckoutWorkspace("go", root))
		})
	}
}
